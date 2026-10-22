package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/chat"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/user"
	"github.com/workbuddy/backend/internal/ws"
)

func TestSendMessage(t *testing.T) {
	e := newChatEnv(t)
	roomID := e.createRoom()
	alice, aliceToken := e.createUser("Alice")
	e.join(roomID, alice)

	code, body := e.send(roomID, aliceToken, `{"text": "  hello room  "}`)
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", code, body)
	}
	var msg messageResponse
	decode(t, body, &msg)
	if msg.Text != "hello room" || msg.UserID != alice || msg.RoomID != roomID || msg.UserName != "Alice" {
		t.Fatalf("unexpected message %+v", msg)
	}
	if msg.ID == uuid.Nil || msg.CreatedAt.IsZero() {
		t.Fatalf("expected id and created_at, got %+v", msg)
	}

	events := e.events.all()
	if len(events) != 1 || events[0].Type != ws.TypeChatMessage || events[0].RoomID != roomID {
		t.Fatalf("expected one chat_message broadcast to the room, got %+v", events)
	}
	var data struct {
		ID        uuid.UUID `json:"id"`
		RoomID    uuid.UUID `json:"room_id"`
		UserID    uuid.UUID `json:"user_id"`
		UserName  string    `json:"user_name"`
		Text      string    `json:"text"`
		CreatedAt string    `json:"created_at"`
	}
	raw, _ := json.Marshal(events[0].Data)
	decode(t, string(raw), &data)
	if data.ID != msg.ID || data.RoomID != roomID || data.UserID != alice || data.UserName != "Alice" || data.Text != "hello room" {
		t.Fatalf("unexpected chat_message data %s", raw)
	}
	if _, err := time.Parse(time.RFC3339, data.CreatedAt); err != nil {
		t.Fatalf("created_at %q is not RFC 3339: %v", data.CreatedAt, err)
	}
}

func TestSendMessageValidation(t *testing.T) {
	e := newChatEnv(t)
	roomID := e.createRoom()
	alice, aliceToken := e.createUser("Alice")
	e.join(roomID, alice)

	// 2000 two-byte characters: the limit counts characters, not bytes.
	atLimit, _ := json.Marshal(map[string]string{"text": strings.Repeat("ж", chat.MaxTextLength)})
	tooLong, _ := json.Marshal(map[string]string{"text": strings.Repeat("a", chat.MaxTextLength+1)})
	// Surrounding whitespace is trimmed before the length check.
	paddedAtLimit, _ := json.Marshal(map[string]string{"text": "  " + strings.Repeat("a", chat.MaxTextLength) + "\n"})

	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty", `{"text": ""}`, http.StatusBadRequest},
		{"whitespace only", `{"text": " \t\n "}`, http.StatusBadRequest},
		{"missing text", `{}`, http.StatusBadRequest},
		{"too long", string(tooLong), http.StatusBadRequest},
		{"unknown field", `{"text": "hi", "image": "x"}`, http.StatusBadRequest},
		{"malformed", `{"text":`, http.StatusBadRequest},
		{"exactly at limit", string(atLimit), http.StatusCreated},
		{"padded at limit", string(paddedAtLimit), http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := e.send(roomID, aliceToken, tc.body)
			if code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, code, body)
			}
		})
	}

	if got := e.countMessages(roomID); got != 2 {
		t.Fatalf("expected only the 2 valid messages stored, got %d", got)
	}
	if code, _ := e.do(http.MethodPost, "/rooms/not-a-uuid/messages", aliceToken, `{"text": "x"}`); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid room id, got %d", code)
	}
}

func TestChatRequiresParticipation(t *testing.T) {
	e := newChatEnv(t)
	roomID := e.createRoom()
	alice, aliceToken := e.createUser("Alice")
	bob, bobToken := e.createUser("Bob")
	e.join(roomID, alice)
	if code, body := e.send(roomID, aliceToken, `{"text": "members only"}`); code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", code, body)
	}

	// Bob never joined.
	if code, body := e.send(roomID, bobToken, `{"text": "let me in"}`); code != http.StatusForbidden {
		t.Fatalf("expected non-participant send 403, got %d: %s", code, body)
	}
	if code, body := e.list(roomID, bobToken, ""); code != http.StatusForbidden {
		t.Fatalf("expected non-participant list 403, got %d: %s", code, body)
	}

	// A user who left is no longer a participant.
	e.join(roomID, bob)
	if code, body := e.list(roomID, bobToken, ""); code != http.StatusOK {
		t.Fatalf("expected participant list 200, got %d: %s", code, body)
	}
	if err := e.rooms.Leave(context.Background(), roomID, bob); err != nil {
		t.Fatalf("leave room: %v", err)
	}
	if code, body := e.send(roomID, bobToken, `{"text": "still here?"}`); code != http.StatusForbidden {
		t.Fatalf("expected send after leaving 403, got %d: %s", code, body)
	}
	if code, body := e.list(roomID, bobToken, ""); code != http.StatusForbidden {
		t.Fatalf("expected list after leaving 403, got %d: %s", code, body)
	}

	// A room that does not exist has no participants.
	if code, _ := e.send(uuid.New(), aliceToken, `{"text": "hi"}`); code != http.StatusForbidden {
		t.Fatalf("expected send to unknown room 403, got %d", code)
	}

	if code, _ := e.do(http.MethodGet, "/rooms/"+roomID.String()+"/messages", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated list 401, got %d", code)
	}
	if got := e.countMessages(roomID); got != 1 {
		t.Fatalf("expected rejected sends not to be stored, got %d messages", got)
	}
	if n := e.events.count(ws.TypeChatMessage); n != 1 {
		t.Fatalf("expected only the accepted message broadcast, got %d", n)
	}
}

func TestListPagination(t *testing.T) {
	e := newChatEnv(t)
	roomID := e.createRoom()
	alice, aliceToken := e.createUser("Alice")
	e.join(roomID, alice)

	// Empty room: empty list (not null), no cursor.
	code, body := e.list(roomID, aliceToken, "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"next_cursor":null`) {
		t.Fatalf("expected empty page, got %s", body)
	}

	var sent []uuid.UUID
	for _, text := range []string{"m1", "m2", "m3", "m4", "m5"} {
		code, body := e.send(roomID, aliceToken, `{"text": "`+text+`"}`)
		if code != http.StatusCreated {
			t.Fatalf("send %s: %d %s", text, code, body)
		}
		var m messageResponse
		decode(t, body, &m)
		sent = append(sent, m.ID)
	}

	// Pages of 2, newest first: [m5 m4] [m3 m2] [m1].
	page := e.page(roomID, aliceToken, "limit=2")
	assertTexts(t, page, "m5", "m4")
	assertCursor(t, page, &sent[3])

	page = e.page(roomID, aliceToken, "limit=2&before="+page.NextCursor.String())
	assertTexts(t, page, "m3", "m2")
	assertCursor(t, page, &sent[1])

	page = e.page(roomID, aliceToken, "limit=2&before="+page.NextCursor.String())
	assertTexts(t, page, "m1")
	assertCursor(t, page, nil)

	// A page that ends exactly on the oldest message has no cursor.
	page = e.page(roomID, aliceToken, "limit=5")
	assertTexts(t, page, "m5", "m4", "m3", "m2", "m1")
	assertCursor(t, page, nil)

	// Before the oldest message: empty page.
	page = e.page(roomID, aliceToken, "before="+sent[0].String())
	assertTexts(t, page)
	assertCursor(t, page, nil)

	// The default limit covers all five.
	page = e.page(roomID, aliceToken, "")
	assertTexts(t, page, "m5", "m4", "m3", "m2", "m1")

	otherRoom := e.createRoom()
	e.join(otherRoom, alice)
	code, body = e.send(otherRoom, aliceToken, `{"text": "elsewhere"}`)
	if code != http.StatusCreated {
		t.Fatalf("send to other room: %d %s", code, body)
	}
	var foreign messageResponse
	decode(t, body, &foreign)

	for _, q := range []string{
		"limit=0", "limit=-1", "limit=abc", "limit=1.5",
		"before=not-a-uuid",
		"before=" + uuid.NewString(),    // no such message
		"before=" + foreign.ID.String(), // message of another room
	} {
		if code, body := e.list(roomID, aliceToken, q); code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", q, code, body)
		}
	}
}

func TestListPaginationLimitsAndTies(t *testing.T) {
	e := newChatEnv(t)
	roomID := e.createRoom()
	alice, aliceToken := e.createUser("Alice")
	e.join(roomID, alice)

	// 120 messages, all with the same created_at: ordering falls back to id
	// and paging must neither skip nor repeat any of them.
	_, err := e.db.Exec(context.Background(), `
		INSERT INTO chat_messages (room_id, user_id, text, created_at)
		SELECT $1, $2, 'bulk ' || g, '2026-01-01T00:00:00Z'
		FROM generate_series(1, 120) AS g`, roomID, alice)
	if err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	// Limits above the maximum are capped.
	page := e.page(roomID, aliceToken, "limit=1000")
	if len(page.Items) != chat.MaxPageSize || page.NextCursor == nil {
		t.Fatalf("expected %d items and a cursor, got %d items, cursor %v", chat.MaxPageSize, len(page.Items), page.NextCursor)
	}

	seen := map[uuid.UUID]bool{}
	query := "limit=7"
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
		page := e.page(roomID, aliceToken, query)
		for _, m := range page.Items {
			if seen[m.ID] {
				t.Fatalf("message %s returned twice", m.ID)
			}
			seen[m.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		query = "limit=7&before=" + page.NextCursor.String()
	}
	if len(seen) != 120 {
		t.Fatalf("expected to page through 120 messages, saw %d", len(seen))
	}
}

func assertTexts(t *testing.T, page pageResponse, want ...string) {
	t.Helper()
	got := make([]string, len(page.Items))
	for i, m := range page.Items {
		got[i] = m.Text
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("expected texts %v, got %v", want, got)
	}
}

func assertCursor(t *testing.T, page pageResponse, want *uuid.UUID) {
	t.Helper()
	switch {
	case want == nil && page.NextCursor != nil:
		t.Fatalf("expected no next_cursor, got %s", page.NextCursor)
	case want != nil && (page.NextCursor == nil || *page.NextCursor != *want):
		t.Fatalf("expected next_cursor %s, got %v", want, page.NextCursor)
	}
}

type messageResponse struct {
	ID        uuid.UUID `json:"id"`
	RoomID    uuid.UUID `json:"room_id"`
	UserID    uuid.UUID `json:"user_id"`
	UserName  string    `json:"user_name"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

type pageResponse struct {
	Items      []messageResponse `json:"items"`
	NextCursor *uuid.UUID        `json:"next_cursor"`
}

type chatEnv struct {
	t      *testing.T
	db     *pgxpool.Pool
	tokens *auth.TokenManager
	rooms  *room.Service
	router http.Handler
	events *eventRecorder
}

func newChatEnv(t *testing.T) *chatEnv {
	t.Helper()
	return newChatEnvWithLimiter(t, nil)
}

// newChatEnvWithLimiter builds the env with the limiter newLimiter returns
// for the test Redis client; a nil newLimiter disables rate limiting.
func newChatEnvWithLimiter(t *testing.T, newLimiter func(*goredis.Client) chat.RateLimiter) *chatEnv {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}

	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	db, err := postgres.NewPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(db.Close)

	redisClient, err := redisplatform.NewClient(cfg.RedisURL)
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}
	t.Cleanup(func() { _ = redisClient.Close() })

	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	roomService := room.NewService(room.NewRepository(db))
	events := &eventRecorder{}
	var limiter chat.RateLimiter
	if newLimiter != nil {
		limiter = newLimiter(redisClient)
	}
	chatService := chat.NewService(chat.NewRepository(db), roomService, events, limiter)

	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthMW:      auth.NewMiddleware(tokenManager),
		ChatHandler: chat.NewHandler(chatService),
	})

	return &chatEnv{t: t, db: db, tokens: tokenManager, rooms: roomService, router: router, events: events}
}

func (e *chatEnv) createUser(name string) (uuid.UUID, string) {
	e.t.Helper()
	ctx := context.Background()
	email := "chat-" + uuid.NewString() + "@example.com"
	u, err := auth.NewRepository(e.db).CreateUser(ctx, &auth.OAuthProfile{
		Provider: user.OAuthProviderGoogle,
		OAuthID:  email,
		Email:    email,
		Name:     name,
	})
	if err != nil {
		e.t.Fatalf("create test user: %v", err)
	}
	e.t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.db.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", u.ID)
	})

	token, err := e.tokens.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		e.t.Fatalf("issue access token: %v", err)
	}
	return u.ID, token
}

func (e *chatEnv) createRoom() uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	err := e.db.QueryRow(context.Background(),
		`INSERT INTO rooms (name, type) VALUES ($1, 'focus') RETURNING id`, "Chat Room "+uuid.NewString()).Scan(&id)
	if err != nil {
		e.t.Fatalf("create test room: %v", err)
	}
	e.t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.db.Exec(cleanupCtx, "DELETE FROM rooms WHERE id = $1", id)
	})
	return id
}

func (e *chatEnv) join(roomID, userID uuid.UUID) {
	e.t.Helper()
	if _, err := e.rooms.Join(context.Background(), roomID, userID); err != nil {
		e.t.Fatalf("join room: %v", err)
	}
}

func (e *chatEnv) countMessages(roomID uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(context.Background(), `SELECT count(*) FROM chat_messages WHERE room_id = $1`, roomID).Scan(&n); err != nil {
		e.t.Fatalf("count messages: %v", err)
	}
	return n
}

func (e *chatEnv) do(method, path, token, body string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (e *chatEnv) send(roomID uuid.UUID, token, body string) (int, string) {
	e.t.Helper()
	return e.do(http.MethodPost, "/rooms/"+roomID.String()+"/messages", token, body)
}

func (e *chatEnv) list(roomID uuid.UUID, token, rawQuery string) (int, string) {
	e.t.Helper()
	u := url.URL{Path: "/rooms/" + roomID.String() + "/messages", RawQuery: rawQuery}
	return e.do(http.MethodGet, u.String(), token, "")
}

func (e *chatEnv) page(roomID uuid.UUID, token, rawQuery string) pageResponse {
	e.t.Helper()
	code, body := e.list(roomID, token, rawQuery)
	if code != http.StatusOK {
		e.t.Fatalf("list %q: expected 200, got %d: %s", rawQuery, code, body)
	}
	var p pageResponse
	decode(e.t, body, &p)
	return p
}

func decode(t *testing.T, body string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), dst); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

type eventRecorder struct {
	mu     sync.Mutex
	events []ws.Event
}

func (r *eventRecorder) Broadcast(_ uuid.UUID, ev ws.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) all() []ws.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ws.Event(nil), r.events...)
}

func (r *eventRecorder) count(eventType string) int {
	n := 0
	for _, ev := range r.all() {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}
