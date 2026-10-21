package task_test

import (
	"bytes"
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
	"github.com/workbuddy/backend/internal/task"
	"github.com/workbuddy/backend/internal/user"
)

func TestCreateAndCompleteTask(t *testing.T) {
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
	taskService := task.NewService(task.NewRepository(db), roomService, task.TimerDeps{
		Index: task.NewTimerIndex(redisClient, "test:"+uuid.NewString()+":"),
	})

	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthHandler: auth.NewHandler(authService),
		AuthMW:      auth.NewMiddleware(tokenManager),
		TaskHandler: task.NewHandler(taskService),
	})

	owner := createTestUser(t, db, "task-owner@example.com")
	ownerPair, err := authService.IssueTokenPair(ctx, owner)
	if err != nil {
		t.Fatalf("issue token pair: %v", err)
	}

	other := createTestUser(t, db, "task-other@example.com")
	otherPair, err := authService.IssueTokenPair(ctx, other)
	if err != nil {
		t.Fatalf("issue token pair: %v", err)
	}

	testRoomID := createTestRoom(t, db, "Task Test Room "+uuid.NewString())

	createBody, _ := json.Marshal(map[string]string{"text": "write integration tests"})
	createReq := httptest.NewRequest(http.MethodPost, "/rooms/"+testRoomID.String()+"/tasks", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+ownerPair.AccessToken)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	var created struct {
		ID          string  `json:"id"`
		Text        string  `json:"text"`
		CompletedAt *string `json:"completed_at"`
	}
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Text != "write integration tests" {
		t.Fatalf("expected text to round-trip, got %q", created.Text)
	}
	if created.CompletedAt != nil {
		t.Fatal("expected new task to be incomplete")
	}

	// A different user may not update someone else's task.
	forbiddenBody, _ := json.Marshal(map[string]bool{"completed": true})
	forbiddenReq := httptest.NewRequest(http.MethodPatch, "/tasks/"+created.ID, bytes.NewReader(forbiddenBody))
	forbiddenReq.Header.Set("Authorization", "Bearer "+otherPair.AccessToken)
	forbiddenReq.Header.Set("Content-Type", "application/json")
	forbiddenRec := httptest.NewRecorder()
	router.ServeHTTP(forbiddenRec, forbiddenReq)
	if forbiddenRec.Code != http.StatusForbidden {
		t.Fatalf("expected non-owner update 403, got %d: %s", forbiddenRec.Code, forbiddenRec.Body.String())
	}

	completeBody, _ := json.Marshal(map[string]bool{"completed": true})
	completeReq := httptest.NewRequest(http.MethodPatch, "/tasks/"+created.ID, bytes.NewReader(completeBody))
	completeReq.Header.Set("Authorization", "Bearer "+ownerPair.AccessToken)
	completeReq.Header.Set("Content-Type", "application/json")
	completeRec := httptest.NewRecorder()
	router.ServeHTTP(completeRec, completeReq)
	if completeRec.Code != http.StatusOK {
		t.Fatalf("expected complete 200, got %d: %s", completeRec.Code, completeRec.Body.String())
	}

	var completed struct {
		CompletedAt *string `json:"completed_at"`
	}
	if err := json.NewDecoder(completeRec.Body).Decode(&completed); err != nil {
		t.Fatalf("decode complete response: %v", err)
	}
	if completed.CompletedAt == nil {
		t.Fatal("expected completed_at to be set")
	}

	listReq := httptest.NewRequest(http.MethodGet, "/tasks/me", nil)
	listReq.Header.Set("Authorization", "Bearer "+ownerPair.AccessToken)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list 200, got %d: %s", listRec.Code, listRec.Body.String())
	}

	var listed []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(listRec.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	found := false
	for _, item := range listed {
		if item.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected created task to appear in /tasks/me")
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
