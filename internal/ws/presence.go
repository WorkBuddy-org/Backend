package ws

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// unknownUserName is shown when a present user id has no users row.
const unknownUserName = "Unknown"

type UserLookup interface {
	FindUserNamesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// PresenceStore keeps the set of user ids connected to each room in Redis
// under room:{room_id}:presence.
type PresenceStore struct {
	redis *goredis.Client
	users UserLookup
}

func NewPresenceStore(redisClient *goredis.Client, users UserLookup) *PresenceStore {
	return &PresenceStore{redis: redisClient, users: users}
}

func presenceKey(roomID uuid.UUID) string {
	return "room:" + roomID.String() + ":presence"
}

func (s *PresenceStore) Add(ctx context.Context, roomID, userID uuid.UUID) error {
	if err := s.redis.SAdd(ctx, presenceKey(roomID), userID.String()).Err(); err != nil {
		return fmt.Errorf("add presence: %w", err)
	}
	return nil
}

func (s *PresenceStore) Remove(ctx context.Context, roomID, userID uuid.UUID) error {
	if err := s.redis.SRem(ctx, presenceKey(roomID), userID.String()).Err(); err != nil {
		return fmt.Errorf("remove presence: %w", err)
	}
	return nil
}

// List returns the room's present users with display names, sorted by name
// then id so snapshots are stable.
func (s *PresenceStore) List(ctx context.Context, roomID uuid.UUID) ([]PresenceUser, error) {
	members, err := s.redis.SMembers(ctx, presenceKey(roomID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list presence: %w", err)
	}

	ids := make([]uuid.UUID, 0, len(members))
	for _, member := range members {
		id, err := uuid.Parse(member)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}

	names, err := s.users.FindUserNamesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	users := make([]PresenceUser, 0, len(ids))
	for _, id := range ids {
		name := names[id]
		if name == "" {
			name = unknownUserName
		}
		users = append(users, PresenceUser{UserID: id, Name: name})
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].Name != users[j].Name {
			return users[i].Name < users[j].Name
		}
		return users[i].UserID.String() < users[j].UserID.String()
	})

	return users, nil
}

// ResetAll deletes every room presence set. With a single server instance the
// hub is the source of truth, so anything left in Redis at startup is stale
// (e.g. after a crash) and must go.
func (s *PresenceStore) ResetAll(ctx context.Context) (int, error) {
	deleted := 0
	iter := s.redis.Scan(ctx, 0, "room:*:presence", 100).Iterator()
	for iter.Next(ctx) {
		if err := s.redis.Del(ctx, iter.Val()).Err(); err != nil {
			return deleted, fmt.Errorf("delete presence key: %w", err)
		}
		deleted++
	}
	if err := iter.Err(); err != nil {
		return deleted, fmt.Errorf("scan presence keys: %w", err)
	}
	return deleted, nil
}
