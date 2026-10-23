package ratelimit_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/platform/ratelimit"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
)

func TestFixedWindow(t *testing.T) {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		t.Skip("REDIS_URL is not set")
	}
	client, err := redisplatform.NewClient(redisURL)
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	const window = 400 * time.Millisecond
	limiter := ratelimit.NewFixedWindow(client, 2, window)
	key := "test:" + uuid.NewString() + ":fixed-window"
	other := key + ":other"
	t.Cleanup(func() { _ = client.Del(context.Background(), key, other).Err() })

	allow := func(k string) (bool, time.Duration) {
		t.Helper()
		ok, retryAfter, err := limiter.Allow(ctx, k)
		if err != nil {
			t.Fatalf("allow %s: %v", k, err)
		}
		return ok, retryAfter
	}

	for i := range 2 {
		if ok, _ := allow(key); !ok {
			t.Fatalf("hit %d: expected allowed", i+1)
		}
	}
	ok, retryAfter := allow(key)
	if ok || retryAfter <= 0 || retryAfter > window {
		t.Fatalf("expected the 3rd hit denied with 0 < retryAfter <= %s, got ok=%v retryAfter=%s", window, ok, retryAfter)
	}
	if ok, _ := allow(other); !ok {
		t.Fatal("expected another key to have its own window")
	}

	time.Sleep(window + 50*time.Millisecond)
	if ok, _ := allow(key); !ok {
		t.Fatal("expected the window to reset")
	}

	// A counter that lost its TTL gets one again instead of blocking forever.
	if err := client.Set(ctx, key, 10, 0).Err(); err != nil {
		t.Fatalf("set key without ttl: %v", err)
	}
	ok, retryAfter = allow(key)
	if ok || retryAfter != window {
		t.Fatalf("expected denied with a fresh window, got ok=%v retryAfter=%s", ok, retryAfter)
	}
	if ttl := client.PTTL(ctx, key).Val(); ttl <= 0 || ttl > window {
		t.Fatalf("expected the key to get a ttl, got %s", ttl)
	}
}
