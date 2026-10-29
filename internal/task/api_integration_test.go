package task_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
	_, token := e.newUserWithID()
	return token
}

func (e *taskEnv) newUserWithID() (uuid.UUID, string) {
	e.t.Helper()
	u := createTestUser(e.t, e.db, "task-api-"+uuid.NewString()+"@example.com")
	token, err := e.tokens.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		e.t.Fatalf("issue token: %v", err)
	}
	return u.ID, token
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
		if code != http.StatusBadRequest || !strings.Contains(resp, `"code":"VALIDATION_ERROR","message":"no fields to update"`) {
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

func TestListMyTasksPagination(t *testing.T) {
	e := newTaskEnv(t)
	ctx := context.Background()
	roomID := createTestRoom(t, e.db, "Task Page Room "+uuid.NewString())
	ownerID, owner := e.newUserWithID()
	otherID, _ := e.newUserWithID()

	const total = 105
	if _, err := e.db.Exec(ctx,
		`INSERT INTO tasks (user_id, room_id, text) SELECT $1, $2, 'task ' || g FROM generate_series(1, $3) g`,
		ownerID, roomID, total); err != nil {
		t.Fatalf("insert tasks: %v", err)
	}
	if _, err := e.db.Exec(ctx, `INSERT INTO tasks (user_id, room_id, text) VALUES ($1, $2, 'not mine')`, otherID, roomID); err != nil {
		t.Fatalf("insert other task: %v", err)
	}

	list := func(query string) []string {
		t.Helper()
		code, body := e.do(http.MethodGet, "/tasks/me"+query, owner, "")
		if code != http.StatusOK {
			t.Fatalf("GET /tasks/me%s: expected 200, got %d: %s", query, code, body)
		}
		// The response stays a plain JSON array.
		var items []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(body), &items); err != nil {
			t.Fatalf("decode %s as an array: %v", body, err)
		}
		ids := make([]string, len(items))
		for i, it := range items {
			if it.Text == "not mine" {
				t.Fatalf("another user's task leaked into %s", query)
			}
			ids[i] = it.ID
		}
		return ids
	}

	if n := len(list("")); n != 50 {
		t.Fatalf("expected the default page of 50, got %d", n)
	}
	if n := len(list("?limit=1000")); n != 100 {
		t.Fatalf("expected limit capped to 100, got %d", n)
	}

	// Walking the pages visits every task exactly once, in a stable order.
	all := append(list("?limit=100"), list("?limit=100&offset=100")...)
	if len(all) != total {
		t.Fatalf("expected %d tasks over two pages, got %d", total, len(all))
	}
	var walked []string
	for offset := 0; ; offset += 40 {
		page := list("?limit=40&offset=" + strconv.Itoa(offset))
		walked = append(walked, page...)
		if len(page) < 40 {
			break
		}
	}
	if strings.Join(walked, ",") != strings.Join(all, ",") {
		t.Fatal("pages of 40 don't match pages of 100")
	}
	seen := map[string]bool{}
	for _, id := range walked {
		if seen[id] {
			t.Fatalf("task %s returned twice", id)
		}
		seen[id] = true
	}

	if got := list("?offset=1000"); len(got) != 0 {
		t.Fatalf("expected [] past the end, got %d items", len(got))
	}

	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=abc", "?offset=-1", "?offset=x"} {
		if code, body := e.do(http.MethodGet, "/tasks/me"+q, owner, ""); code != http.StatusBadRequest {
			t.Fatalf("GET /tasks/me%s: expected 400, got %d: %s", q, code, body)
		}
	}
}
