package task

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// Redis keys of the scheduling index. Postgres stays the source of truth;
// these sorted sets only say when to look at a task again.
const (
	timersDueKey  = "timers:due"
	promptsDueKey = "prompts:due"
)

// claimScript removes a member only if its score is still due. A timer that
// was re-scheduled (e.g. extended) after the scheduler read the due list has
// a later score and is left alone, so only the claimer of the current entry
// acts on it.
var claimScript = goredis.NewScript(`
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if score and tonumber(score) <= tonumber(ARGV[2]) then
	return redis.call('ZREM', KEYS[1], ARGV[1])
end
return 0
`)

// dueSet is a Redis sorted set of task ids scored by unix time (seconds,
// millisecond precision).
type dueSet struct {
	redis *goredis.Client
	key   string
}

func score(t time.Time) float64 {
	return float64(t.UnixMilli()) / 1000
}

func (s dueSet) schedule(ctx context.Context, taskID uuid.UUID, at time.Time) error {
	if err := s.redis.ZAdd(ctx, s.key, goredis.Z{Score: score(at), Member: taskID.String()}).Err(); err != nil {
		return fmt.Errorf("schedule %s: %w", s.key, err)
	}
	return nil
}

func (s dueSet) unschedule(ctx context.Context, taskID uuid.UUID) error {
	if err := s.redis.ZRem(ctx, s.key, taskID.String()).Err(); err != nil {
		return fmt.Errorf("unschedule %s: %w", s.key, err)
	}
	return nil
}

// due returns the ids whose time is at or before now.
func (s dueSet) due(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	members, err := s.redis.ZRangeByScore(ctx, s.key, &goredis.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatFloat(score(now), 'f', -1, 64),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.key, err)
	}

	ids := make([]uuid.UUID, 0, len(members))
	for _, m := range members {
		id, err := uuid.Parse(m)
		if err != nil {
			// Not ours to interpret; drop it so it is not re-read forever.
			_ = s.redis.ZRem(ctx, s.key, m).Err()
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// take removes the entry whatever its score and reports whether this caller
// removed it. Used by user answers, which race the deadline's claim for the
// same entry.
func (s dueSet) take(ctx context.Context, taskID uuid.UUID) (bool, error) {
	n, err := s.redis.ZRem(ctx, s.key, taskID.String()).Result()
	if err != nil {
		return false, fmt.Errorf("take %s: %w", s.key, err)
	}
	return n == 1, nil
}

// claim reports whether this caller removed the due entry and so owns acting
// on it.
func (s dueSet) claim(ctx context.Context, taskID uuid.UUID, now time.Time) (bool, error) {
	n, err := claimScript.Run(ctx, s.redis, []string{s.key}, taskID.String(), score(now)).Int()
	if err != nil {
		return false, fmt.Errorf("claim %s: %w", s.key, err)
	}
	return n == 1, nil
}

// TimerIndex holds the sorted sets the scheduler polls: running timers by
// ends_at and extension prompts by their deadline.
type TimerIndex struct {
	redis   *goredis.Client
	timers  dueSet
	prompts dueSet
}

// NewTimerIndex puts keyPrefix in front of every key; production passes ""
// so the keys are exactly timers:due etc. Tests use a unique prefix to stay
// isolated from each other.
func NewTimerIndex(redisClient *goredis.Client, keyPrefix string) *TimerIndex {
	return &TimerIndex{
		redis:   redisClient,
		timers:  dueSet{redis: redisClient, key: keyPrefix + timersDueKey},
		prompts: dueSet{redis: redisClient, key: keyPrefix + promptsDueKey},
	}
}

// rebuild replaces the index with the given active timers.
func (x *TimerIndex) rebuild(ctx context.Context, active []Task) error {
	pipe := x.redis.TxPipeline()
	pipe.Del(ctx, x.timers.key, x.prompts.key)
	for _, t := range active {
		switch {
		case t.TimerStatus == TimerRunning && t.TimerEndsAt != nil:
			pipe.ZAdd(ctx, x.timers.key, goredis.Z{Score: score(*t.TimerEndsAt), Member: t.ID.String()})
		case t.TimerStatus == TimerPrompting && t.PromptDeadlineAt != nil:
			pipe.ZAdd(ctx, x.prompts.key, goredis.Z{Score: score(*t.PromptDeadlineAt), Member: t.ID.String()})
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("rebuild timer index: %w", err)
	}
	return nil
}
