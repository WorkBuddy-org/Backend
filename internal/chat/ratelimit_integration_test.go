package chat_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/chat"
	"github.com/workbuddy/backend/internal/ws"
)

func TestChatRateLimit(t *testing.T) {
	const (
		limit  = 3
		window = 1500 * time.Millisecond
	)
	prefix := "test:" + uuid.NewString() + ":"
	e := newChatEnvWithLimiter(t, func(c *goredis.Client) chat.RateLimiter {
		return chat.NewRedisRateLimiter(c, limit, window, prefix)
	})

	roomID := e.createRoom()
	otherRoom := e.createRoom()
	aliceID, alice := e.createUser("Alice")
	bobID, bob := e.createUser("Bob")
	e.join(roomID, aliceID)
	e.join(otherRoom, aliceID)
	e.join(roomID, bobID)

	// Under the limit.
	for i := range limit {
		if code, _, body := e.sendWithHeaders(roomID, alice, `{"text":"msg `+strconv.Itoa(i)+`"}`); code != http.StatusCreated {
			t.Fatalf("send %d: expected 201, got %d: %s", i, code, body)
		}
	}

	// Over the limit: 429 with Retry-After and the standard error body.
	code, header, body := e.sendWithHeaders(roomID, alice, `{"text":"one too many"}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 over the limit, got %d: %s", code, body)
	}
	retryAfter, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || retryAfter < 1 || retryAfter > 2 {
		t.Fatalf("expected Retry-After of 1..2 seconds, got %q", header.Get("Retry-After"))
	}
	var errBody struct {
		Error string `json:"error"`
	}
	decode(t, body, &errBody)
	if errBody.Error != "too many messages, slow down" {
		t.Fatalf("unexpected error body %s", body)
	}
	if n := e.countMessages(roomID); n != limit {
		t.Fatalf("expected %d stored messages, got %d", limit, n)
	}
	if n := e.events.count(ws.TypeChatMessage); n != limit {
		t.Fatalf("expected %d broadcasts, got %d", limit, n)
	}

	// The limit is per user and shared across rooms.
	if code, _, body := e.sendWithHeaders(otherRoom, alice, `{"text":"other room"}`); code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 in another room, got %d: %s", code, body)
	}
	if code, _, body := e.sendWithHeaders(roomID, bob, `{"text":"bob is fine"}`); code != http.StatusCreated {
		t.Fatalf("expected 201 for another user, got %d: %s", code, body)
	}

	// Validation runs first: an invalid message is a 400, not a 429.
	if code, _, body := e.sendWithHeaders(roomID, alice, `{"text":"   "}`); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty text while limited, got %d: %s", code, body)
	}

	// The window resets.
	time.Sleep(window + 100*time.Millisecond)
	if code, _, body := e.sendWithHeaders(roomID, alice, `{"text":"after the window"}`); code != http.StatusCreated {
		t.Fatalf("expected 201 after the window, got %d: %s", code, body)
	}
}

func TestChatRateLimitFailsOpen(t *testing.T) {
	// Nothing listens on port 1, so every limiter call fails.
	e := newChatEnvWithLimiter(t, func(*goredis.Client) chat.RateLimiter {
		broken := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
		t.Cleanup(func() { _ = broken.Close() })
		return chat.NewRedisRateLimiter(broken, 1, time.Minute, "")
	})

	roomID := e.createRoom()
	aliceID, alice := e.createUser("Alice")
	e.join(roomID, aliceID)

	for i := range 3 {
		if code, _, body := e.sendWithHeaders(roomID, alice, `{"text":"still works"}`); code != http.StatusCreated {
			t.Fatalf("send %d with Redis down: expected 201, got %d: %s", i, code, body)
		}
	}
}

func (e *chatEnv) sendWithHeaders(roomID uuid.UUID, token, body string) (int, http.Header, string) {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID.String()+"/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}
