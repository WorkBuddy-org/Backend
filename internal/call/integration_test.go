package call_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	lkauth "github.com/livekit/protocol/auth"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/call"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/user"
)

const (
	testLiveKitURL    = "wss://livekit.example.com"
	testLiveKitKey    = "APItestkey"
	testLiveKitSecret = "test-livekit-secret-that-is-long-enough-for-hs256"
)

func TestCallTokenIssued(t *testing.T) {
	e := newCallEnv(t)
	roomID := e.createRoom()
	aliceID, alice := e.createUser("Alice Caller")
	e.join(roomID, aliceID)

	before := time.Now()
	code, body := e.post(e.router, roomID.String(), alice)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	if strings.Contains(body, testLiveKitSecret) {
		t.Fatal("response leaks the LiveKit API secret")
	}

	var resp struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	decode(t, body, &resp)
	if resp.URL != testLiveKitURL {
		t.Fatalf("expected url %q, got %q", testLiveKitURL, resp.URL)
	}

	verifier, err := lkauth.ParseAPIToken(resp.Token)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if verifier.APIKey() != testLiveKitKey {
		t.Fatalf("expected issuer %q, got %q", testLiveKitKey, verifier.APIKey())
	}
	if _, _, err := verifier.Verify("wrong-secret"); err == nil {
		t.Fatal("token must not verify with a different secret")
	}
	claims, grants, err := verifier.Verify(testLiveKitSecret)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}

	if grants.Identity != aliceID.String() || claims.Subject != aliceID.String() {
		t.Fatalf("expected identity %s, got identity=%q sub=%q", aliceID, grants.Identity, claims.Subject)
	}
	if grants.Name != "Alice Caller" {
		t.Fatalf("expected name %q, got %q", "Alice Caller", grants.Name)
	}

	v := grants.Video
	if v == nil {
		t.Fatal("expected a video grant")
	}
	if v.Room != roomID.String() {
		t.Fatalf("expected room %s, got %q", roomID, v.Room)
	}
	if !v.RoomJoin || v.CanPublish == nil || !*v.CanPublish || v.CanSubscribe == nil || !*v.CanSubscribe {
		t.Fatalf("expected roomJoin+canPublish+canSubscribe, got %+v", v)
	}
	if v.RoomAdmin || v.RoomCreate || v.RoomList || v.RoomRecord {
		t.Fatalf("expected no admin grants, got %+v", v)
	}

	if claims.Expiry == nil {
		t.Fatal("expected an expiry")
	}
	ttl := claims.Expiry.Time().Sub(before)
	if ttl < call.TokenTTL-time.Minute || ttl > call.TokenTTL+time.Minute {
		t.Fatalf("expected ttl about %s, got %s", call.TokenTTL, ttl)
	}
}

func TestCallTokenRequiresParticipation(t *testing.T) {
	e := newCallEnv(t)
	roomID := e.createRoom()
	aliceID, alice := e.createUser("Alice")
	e.join(roomID, aliceID)
	bobID, bob := e.createUser("Bob")

	// Bob never joined.
	if code, body := e.post(e.router, roomID.String(), bob); code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-participant, got %d: %s", code, body)
	}

	// Bob joined, then left.
	e.join(roomID, bobID)
	if code, body := e.post(e.router, roomID.String(), bob); code != http.StatusOK {
		t.Fatalf("expected 200 after joining, got %d: %s", code, body)
	}
	if err := e.rooms.Leave(context.Background(), roomID, bobID); err != nil {
		t.Fatalf("leave room: %v", err)
	}
	if code, body := e.post(e.router, roomID.String(), bob); code != http.StatusForbidden {
		t.Fatalf("expected 403 after leaving, got %d: %s", code, body)
	}

	// A room that doesn't exist has no participants.
	if code, body := e.post(e.router, uuid.NewString(), alice); code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unknown room, got %d: %s", code, body)
	}

	if code, body := e.post(e.router, "not-a-uuid", alice); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a bad room id, got %d: %s", code, body)
	}
	if code, body := e.post(e.router, roomID.String(), ""); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d: %s", code, body)
	}
}

func TestCallTokenNotConfigured(t *testing.T) {
	e := newCallEnv(t)
	roomID := e.createRoom()
	aliceID, alice := e.createUser("Alice")
	e.join(roomID, aliceID)

	partial := []call.Config{
		{},
		{URL: testLiveKitURL, APIKey: testLiveKitKey},
		{URL: testLiveKitURL, APISecret: testLiveKitSecret},
		{APIKey: testLiveKitKey, APISecret: testLiveKitSecret},
	}
	for _, cfg := range partial {
		code, body := e.post(e.newRouter(cfg), roomID.String(), alice)
		if code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for config %+v, got %d: %s", cfg, code, body)
		}
		var resp struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		decode(t, body, &resp)
		if resp.Error.Code != "SERVICE_UNAVAILABLE" || resp.Error.Message != "call service not configured" {
			t.Fatalf("expected error %q, got %q", "call service not configured", resp.Error.Message)
		}
	}
}

type callEnv struct {
	t      *testing.T
	db     *pgxpool.Pool
	tokens *auth.TokenManager
	rooms  *room.Service
	router http.Handler

	newRouter func(call.Config) http.Handler
}

func newCallEnv(t *testing.T) *callEnv {
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
	authRepo := auth.NewRepository(db)

	e := &callEnv{t: t, db: db, tokens: tokenManager, rooms: roomService}
	e.newRouter = func(callCfg call.Config) http.Handler {
		return httpapi.NewRouter(httpapi.RouterDeps{
			DB:          db,
			Redis:       redisClient,
			AuthMW:      auth.NewMiddleware(tokenManager),
			CallHandler: call.NewHandler(call.NewService(callCfg, roomService, authRepo)),
		})
	}
	e.router = e.newRouter(call.Config{URL: testLiveKitURL, APIKey: testLiveKitKey, APISecret: testLiveKitSecret})
	return e
}

func (e *callEnv) createUser(name string) (uuid.UUID, string) {
	e.t.Helper()
	ctx := context.Background()
	email := "call-" + uuid.NewString() + "@example.com"
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

func (e *callEnv) createRoom() uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	err := e.db.QueryRow(context.Background(),
		`INSERT INTO rooms (name, type) VALUES ($1, 'focus') RETURNING id`, "Call Room "+uuid.NewString()).Scan(&id)
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

func (e *callEnv) join(roomID, userID uuid.UUID) {
	e.t.Helper()
	if _, err := e.rooms.Join(context.Background(), roomID, userID); err != nil {
		e.t.Fatalf("join room: %v", err)
	}
}

func (e *callEnv) post(router http.Handler, roomID, token string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rooms/"+roomID+"/call/token", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func decode(t *testing.T, body string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), dst); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}
