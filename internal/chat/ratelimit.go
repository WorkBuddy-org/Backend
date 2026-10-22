package chat

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// Default send rate: messages per window, per user.
const (
	DefaultRateLimit  = 10
	DefaultRateWindow = 10 * time.Second
)

// RateLimiter decides whether a user may send another message now. When not,
// retryAfter says how long until the next one is accepted.
type RateLimiter interface {
	Allow(ctx context.Context, userID uuid.UUID) (allowed bool, retryAfter time.Duration, err error)
}

// fixedWindowScript counts a send in the user's current window and returns
// the count and the window's remaining milliseconds. The window starts with
// the first send; the TTL is (re)applied if it is missing so a key can never
// outlive its window.
var fixedWindowScript = goredis.NewScript(`
local n = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if n == 1 or ttl < 0 then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
	ttl = tonumber(ARGV[1])
end
return {n, ttl}
`)

// RedisRateLimiter is a fixed-window limiter keyed by user, shared by all
// rooms.
type RedisRateLimiter struct {
	client *goredis.Client
	limit  int
	window time.Duration
	prefix string
}

// NewRedisRateLimiter allows limit sends per window per user. prefix
// namespaces the keys (tests use a unique one).
func NewRedisRateLimiter(client *goredis.Client, limit int, window time.Duration, prefix string) *RedisRateLimiter {
	return &RedisRateLimiter{client: client, limit: limit, window: window, prefix: prefix}
}

func (l *RedisRateLimiter) Allow(ctx context.Context, userID uuid.UUID) (bool, time.Duration, error) {
	key := l.prefix + "chat_rate:" + userID.String()
	res, err := fixedWindowScript.Run(ctx, l.client, []string{key}, l.window.Milliseconds()).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("chat rate limit: %w", err)
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("chat rate limit: unexpected reply %v", res)
	}

	if res[0] > int64(l.limit) {
		return false, time.Duration(res[1]) * time.Millisecond, nil
	}
	return true, 0, nil
}
