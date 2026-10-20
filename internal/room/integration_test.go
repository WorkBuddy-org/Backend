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

func TestJoinAndLeaveRoom(t *testing.T) {
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
	authRepo := auth.NewRepository(db)
	authService := auth.NewService(authRepo, tokenManager, auth.NewOAuthRegistry(auth.OAuthConfig{}), cfg.JWTRefreshTTL, cfg.FrontendCallbackURL, redisClient)

	roomService := room.NewService(room.NewRepository(db))
	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthHandler: auth.NewHandler(authService),
		AuthMW:      auth.NewMiddleware(tokenManager),
		RoomHandler: room.NewHandler(roomService),
	})

	testUser := createTestUser(t, db, "room-flow@example.com")
	pair, err := authService.IssueTokenPair(ctx, testUser)
	if err != nil {
		t.Fatalf("issue token pair: %v", err)
	}

	testRoomID := createTestRoom(t, db, "Test Room "+uuid.NewString())

	joinReq := httptest.NewRequest(http.MethodPost, "/rooms/"+testRoomID.String()+"/join", nil)
	joinReq.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	joinRec := httptest.NewRecorder()
	router.ServeHTTP(joinRec, joinReq)
	if joinRec.Code != http.StatusOK {
		t.Fatalf("expected join 200, got %d: %s", joinRec.Code, joinRec.Body.String())
	}

	var joinBody struct {
		Participation struct {
			ID     string  `json:"id"`
			LeftAt *string `json:"left_at"`
		} `json:"participation"`
	}
	if err := json.NewDecoder(joinRec.Body).Decode(&joinBody); err != nil {
		t.Fatalf("decode join response: %v", err)
	}
	if joinBody.Participation.ID == "" {
		t.Fatal("expected participation id in join response")
	}
	if joinBody.Participation.LeftAt != nil {
		t.Fatal("expected new participation to be open")
	}

	// Rejoining while already in the room should be idempotent: same
	// participation returned, no duplicate row created.
	rejoinReq := httptest.NewRequest(http.MethodPost, "/rooms/"+testRoomID.String()+"/join", nil)
	rejoinReq.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	rejoinRec := httptest.NewRecorder()
	router.ServeHTTP(rejoinRec, rejoinReq)
	if rejoinRec.Code != http.StatusOK {
		t.Fatalf("expected rejoin 200, got %d: %s", rejoinRec.Code, rejoinRec.Body.String())
	}

	var rejoinBody struct {
		Participation struct {
			ID string `json:"id"`
		} `json:"participation"`
	}
	if err := json.NewDecoder(rejoinRec.Body).Decode(&rejoinBody); err != nil {
		t.Fatalf("decode rejoin response: %v", err)
	}
	if rejoinBody.Participation.ID != joinBody.Participation.ID {
		t.Fatalf("expected idempotent rejoin to return same participation, got %s vs %s", rejoinBody.Participation.ID, joinBody.Participation.ID)
	}

	leaveReq := httptest.NewRequest(http.MethodPost, "/rooms/"+testRoomID.String()+"/leave", nil)
	leaveReq.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	leaveRec := httptest.NewRecorder()
	router.ServeHTTP(leaveRec, leaveReq)
	if leaveRec.Code != http.StatusNoContent {
		t.Fatalf("expected leave 204, got %d: %s", leaveRec.Code, leaveRec.Body.String())
	}

	// Leaving again with no open participation should 404.
	leaveAgainReq := httptest.NewRequest(http.MethodPost, "/rooms/"+testRoomID.String()+"/leave", nil)
	leaveAgainReq.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	leaveAgainRec := httptest.NewRecorder()
	router.ServeHTTP(leaveAgainRec, leaveAgainReq)
	if leaveAgainRec.Code != http.StatusNotFound {
		t.Fatalf("expected second leave 404, got %d: %s", leaveAgainRec.Code, leaveAgainRec.Body.String())
	}
}

func createTestUser(t *testing.T, db *pgxpool.Pool, email string) *user.User {
	t.Helper()

	repo := auth.NewRepository(db)
	ctx := context.Background()

	profile := &auth.OAuthProfile{
		Provider: user.OAuthProviderGoogle,
		OAuthID:  email,
		Email:    email,
		Name:     "Test User",
	}

	u, err := repo.CreateUser(ctx, profile)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", u.ID)
	})

	return u
}

func createTestRoom(t *testing.T, db *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()

	ctx := context.Background()
	var id uuid.UUID
	err := db.QueryRow(ctx, `INSERT INTO rooms (name, type) VALUES ($1, 'focus') RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatalf("create test room: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Exec(cleanupCtx, "DELETE FROM tasks WHERE room_id = $1", id)
		_, _ = db.Exec(cleanupCtx, "DELETE FROM rooms WHERE id = $1", id)
	})

	return id
}
