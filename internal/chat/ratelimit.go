package chat

import (
	"context"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/platform/ratelimit"
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

// RedisRateLimiter is a fixed-window limiter keyed by user, shared by all
// rooms.
type RedisRateLimiter struct {
	window *ratelimit.FixedWindow
	prefix string
}

// NewRedisRateLimiter allows limit sends per window per user. prefix
// namespaces the keys (tests use a unique one).
func NewRedisRateLimiter(client *goredis.Client, limit int, window time.Duration, prefix string) *RedisRateLimiter {
	return &RedisRateLimiter{window: ratelimit.NewFixedWindow(client, limit, window), prefix: prefix}
}

func (l *RedisRateLimiter) Allow(ctx context.Context, userID uuid.UUID) (bool, time.Duration, error) {
	return l.window.Allow(ctx, l.prefix+"chat_rate:"+userID.String())
}
