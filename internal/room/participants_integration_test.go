package room_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/user"
)

type participantResponse struct {
	UserID   uuid.UUID `json:"user_id"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joined_at"`
}

type roomEnv struct {
	t      *testing.T
	db     *pgxpool.Pool
	rooms  *room.Service
	router http.Handler
	tokens *auth.TokenManager
}

func newRoomEnv(t *testing.T) *roomEnv {
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
	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthMW:      auth.NewMiddleware(tokenManager),
		RoomHandler: room.NewHandler(roomService),
	})
	return &roomEnv{t: t, db: db, rooms: roomService, router: router, tokens: tokenManager}
}

func (e *roomEnv) newUser(name string) (uuid.UUID, string) {
	e.t.Helper()
	ctx := context.Background()
	email := "rooms-" + uuid.NewString() + "@example.com"
	u, err := auth.NewRepository(e.db).CreateUser(ctx, &auth.OAuthProfile{
		Provider: user.OAuthProviderGoogle, OAuthID: email, Email: email, Name: name,
	})
	if err != nil {
		e.t.Fatalf("create user: %v", err)
	}
	e.t.Cleanup(func() { _, _ = e.db.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
	token, err := e.tokens.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		e.t.Fatalf("issue token: %v", err)
	}
	return u.ID, token
}

func (e *roomEnv) do(method, path, token string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestListParticipants(t *testing.T) {
	e := newRoomEnv(t)
	ctx := context.Background()
	db, roomService, newUser := e.db, e.rooms, e.newUser
	get := func(roomID, token string) (int, string) {
		t.Helper()
		return e.do(http.MethodGet, "/rooms/"+roomID+"/participants", token)
	}
	list := func(roomID, token string) []participantResponse {
		t.Helper()
		code, body := get(roomID, token)
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", code, body)
		}
		var out []participantResponse
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return out
	}

	roomID := createTestRoom(t, db, "Participants Room "+uuid.NewString())
	aliceID, alice := newUser("Alice")
	bobID, bob := newUser("Bob")
	_, carol := newUser("Carol")

	if _, err := roomService.Join(ctx, roomID, aliceID); err != nil {
		t.Fatalf("join alice: %v", err)
	}
	if _, err := roomService.Join(ctx, roomID, bobID); err != nil {
		t.Fatalf("join bob: %v", err)
	}
	// A stray second open row for Alice must not duplicate her.
	if _, err := db.Exec(ctx, `INSERT INTO room_participations (user_id, room_id) VALUES ($1, $2)`, aliceID, roomID); err != nil {
		t.Fatalf("insert duplicate participation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "DELETE FROM room_participations WHERE room_id = $1", roomID)
	})

	got := list(roomID.String(), bob)
	if len(got) != 2 {
		t.Fatalf("expected 2 participants, got %+v", got)
	}
	if got[0].UserID != aliceID || got[0].Name != "Alice" || got[1].UserID != bobID || got[1].Name != "Bob" {
		t.Fatalf("expected Alice then Bob, got %+v", got)
	}
	if got[0].JoinedAt.IsZero() || got[0].JoinedAt.After(got[1].JoinedAt) {
		t.Fatalf("expected joined_at set and ascending, got %+v", got)
	}

	var raw []map[string]any
	_, body := get(roomID.String(), bob)
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if len(raw[0]) != 3 {
		t.Fatalf("expected exactly user_id, name, joined_at, got %v", raw[0])
	}

	// Callers must be in the room themselves.
	if code, body := get(roomID.String(), carol); code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-participant, got %d: %s", code, body)
	}
	if code, body := get(uuid.NewString(), alice); code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unknown room, got %d: %s", code, body)
	}
	if code, body := get("not-a-uuid", alice); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a bad room id, got %d: %s", code, body)
	}
	if code, body := get(roomID.String(), ""); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d: %s", code, body)
	}

	// After Bob leaves he is gone from the list and can no longer read it.
	if err := roomService.Leave(ctx, roomID, bobID); err != nil {
		t.Fatalf("leave bob: %v", err)
	}
	if got := list(roomID.String(), alice); len(got) != 1 || got[0].UserID != aliceID {
		t.Fatalf("expected only Alice after Bob left, got %+v", got)
	}
	if code, body := get(roomID.String(), bob); code != http.StatusForbidden {
		t.Fatalf("expected 403 after leaving, got %d: %s", code, body)
	}
}

// Rooms are not session-based: room responses carry no session fields, even
// if a legacy room_sessions row is active.
func TestRoomResponsesHaveNoSessionFields(t *testing.T) {
	e := newRoomEnv(t)
	ctx := context.Background()
	roomID := createTestRoom(t, e.db, "Sessionless Room "+uuid.NewString())
	_, token := e.newUser("Ada")

	if _, err := e.db.Exec(ctx, `INSERT INTO room_sessions (room_id, status) VALUES ($1, 'focus')`, roomID); err != nil {
		t.Fatalf("insert room session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.db.Exec(context.Background(), "DELETE FROM room_participations WHERE room_id = $1", roomID)
		_, _ = e.db.Exec(context.Background(), "DELETE FROM room_sessions WHERE room_id = $1", roomID)
	})

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/rooms/" + roomID.String()},
		{http.MethodPost, "/rooms/" + roomID.String() + "/join"},
	} {
		code, body := e.do(c.method, c.path, token)
		if code != http.StatusOK {
			t.Fatalf("%s %s: expected 200, got %d: %s", c.method, c.path, code, body)
		}
		var raw map[string]map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if _, ok := raw["active_session"]; ok {
			t.Fatalf("%s %s: unexpected active_session in %s", c.method, c.path, body)
		}
		if raw["room"]["id"] != roomID.String() {
			t.Fatalf("%s %s: expected room %s in %s", c.method, c.path, roomID, body)
		}
		if p, ok := raw["participation"]; ok {
			if _, has := p["session_id"]; has {
				t.Fatalf("%s %s: unexpected participation.session_id in %s", c.method, c.path, body)
			}
		}
	}
}
