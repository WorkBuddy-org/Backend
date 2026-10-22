// Package ratelimit holds the Redis fixed-window limiter shared by the chat
// and login rate limits.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// fixedWindowScript counts a hit in the key's current window and returns the
// count and the window's remaining milliseconds. The window starts with the
// first hit; the TTL is (re)applied if it is missing so a key can never
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

// FixedWindow allows limit hits per window per key.
type FixedWindow struct {
	client *goredis.Client
	limit  int
	window time.Duration
}

func NewFixedWindow(client *goredis.Client, limit int, window time.Duration) *FixedWindow {
	return &FixedWindow{client: client, limit: limit, window: window}
}

// Allow counts a hit for key. When the limit is exceeded it returns false and
// how long until the window resets. Callers decide what a Redis error means;
// the API's limiters fail open.
func (l *FixedWindow) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	res, err := fixedWindowScript.Run(ctx, l.client, []string{key}, l.window.Milliseconds()).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("rate limit: %w", err)
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("rate limit: unexpected reply %v", res)
	}

	if res[0] > int64(l.limit) {
		return false, time.Duration(res[1]) * time.Millisecond, nil
	}
	return true, 0, nil
}
