package task_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/task"
)

type taskEnv struct {
	t      *testing.T
	db     *pgxpool.Pool
	router http.Handler
	tokens *auth.TokenManager
}

func newTaskEnv(t *testing.T) *taskEnv {
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
	taskService := task.NewService(task.NewRepository(db), room.NewService(room.NewRepository(db)), task.TimerDeps{
		Index: task.NewTimerIndex(redisClient, "test:"+uuid.NewString()+":"),
	})
	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthMW:      auth.NewMiddleware(tokenManager),
		TaskHandler: task.NewHandler(taskService),
	})
	return &taskEnv{t: t, db: db, router: router, tokens: tokenManager}
}

// newUser creates a user and returns their access token.
func (e *taskEnv) newUser() string {
	e.t.Helper()
	u := createTestUser(e.t, e.db, "task-api-"+uuid.NewString()+"@example.com")
	token, err := e.tokens.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		e.t.Fatalf("issue token: %v", err)
	}
	return token
}

func (e *taskEnv) do(method, path, token, body string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (e *taskEnv) createTask(roomID uuid.UUID, token, text string) string {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text})
	code, resp := e.do(http.MethodPost, "/rooms/"+roomID.String()+"/tasks", token, string(body))
	if code != http.StatusCreated {
		e.t.Fatalf("create task: expected 201, got %d: %s", code, resp)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(resp), &created); err != nil {
		e.t.Fatalf("decode %s: %v", resp, err)
	}
	return created.ID
}

func TestPatchTaskRequiresFields(t *testing.T) {
	e := newTaskEnv(t)
	roomID := createTestRoom(t, e.db, "Task API Room "+uuid.NewString())
	owner := e.newUser()
	taskID := e.createTask(roomID, owner, "original")

	for _, body := range []string{`{}`, `{"text":null,"completed":null}`} {
		code, resp := e.do(http.MethodPatch, "/tasks/"+taskID, owner, body)
		if code != http.StatusBadRequest || !strings.Contains(resp, `"error":"no fields to update"`) {
			t.Fatalf("PATCH %s: expected 400 no fields to update, got %d: %s", body, code, resp)
		}
	}

	// Same answer as PATCH /profile: checked before the task is even looked up.
	if code, resp := e.do(http.MethodPatch, "/tasks/"+uuid.NewString(), owner, `{}`); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for {} on an unknown task, got %d: %s", code, resp)
	}

	// A real change still works.
	if code, resp := e.do(http.MethodPatch, "/tasks/"+taskID, owner, `{"text":"edited"}`); code != http.StatusOK || !strings.Contains(resp, `"text":"edited"`) {
		t.Fatalf("expected 200 with the new text, got %d: %s", code, resp)
	}
}
