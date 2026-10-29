package profile_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/profile"
)

func TestUpdateProfileValidation(t *testing.T) {
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
	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:             db,
		Redis:          redisClient,
		AuthMW:         auth.NewMiddleware(tokenManager),
		ProfileHandler: profile.NewHandler(profile.NewService(profile.NewRepository(db))),
	})

	u := createTestUser(t, db, "profile-validation-"+uuid.NewString()+"@example.com")
	token, err := tokenManager.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	patch := func(body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/profile", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		return rec.Code, out
	}
	jsonStr := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}

	invalid := []struct {
		name, body, wantErr string
	}{
		{"empty name", `{"name":""}`, "name must be 1-100 characters"},
		{"blank name", `{"name":"   "}`, "name must be 1-100 characters"},
		{"name too long", `{"name":` + jsonStr(strings.Repeat("ж", 101)) + `}`, "name must be 1-100 characters"},
		{"role too long", `{"role":` + jsonStr(strings.Repeat("r", 101)) + `}`, "role must be at most 100 characters"},
		{"stack too long", `{"stack":` + jsonStr(strings.Repeat("s", 201)) + `}`, "stack must be at most 200 characters"},
		{"unknown timezone", `{"timezone":"Mars/Olympus"}`, "timezone must be a valid IANA time zone name, e.g. Europe/Berlin"},
		{"empty timezone", `{"timezone":""}`, "timezone must be a valid IANA time zone name, e.g. Europe/Berlin"},
		{"Local timezone", `{"timezone":"Local"}`, "timezone must be a valid IANA time zone name, e.g. Europe/Berlin"},
		{"offset timezone", `{"timezone":"+03:00"}`, "timezone must be a valid IANA time zone name, e.g. Europe/Berlin"},
		{"valid name, invalid timezone", `{"name":"Should Not Stick","timezone":"Nope/Nope"}`, "timezone must be a valid IANA time zone name, e.g. Europe/Berlin"},
	}
	for _, c := range invalid {
		code, out := patch(c.body)
		if code != http.StatusBadRequest || errorField(out, "code") != "VALIDATION_ERROR" || errorField(out, "message") != c.wantErr {
			t.Fatalf("%s: expected 400 %q, got %d %v", c.name, c.wantErr, code, out)
		}
	}
	// A rejected request changes nothing, not even its valid fields.
	var name string
	if err := db.QueryRow(ctx, `SELECT name FROM users WHERE id = $1`, u.ID).Scan(&name); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if name != "Test User" {
		t.Fatalf("expected name unchanged after rejected patch, got %q", name)
	}

	valid := []struct {
		name, body, field, want string
	}{
		{"trimmed name", `{"name":"  Ada  "}`, "name", "Ada"},
		{"100-char name", `{"name":` + jsonStr(strings.Repeat("ж", 100)) + `}`, "name", strings.Repeat("ж", 100)},
		{"100-char role", `{"role":` + jsonStr(strings.Repeat("r", 100)) + `}`, "role", strings.Repeat("r", 100)},
		{"cleared role", `{"role":"  "}`, "role", ""},
		{"200-char stack", `{"stack":` + jsonStr(strings.Repeat("s", 200)) + `}`, "stack", strings.Repeat("s", 200)},
		{"UTC", `{"timezone":"UTC"}`, "timezone", "UTC"},
		{"trimmed IANA zone", `{"timezone":" Asia/Tokyo "}`, "timezone", "Asia/Tokyo"},
		{"three-level zone", `{"timezone":"America/Argentina/Buenos_Aires"}`, "timezone", "America/Argentina/Buenos_Aires"},
	}
	for _, c := range valid {
		code, out := patch(c.body)
		if code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d %v", c.name, code, out)
		}
		if out[c.field] != c.want {
			t.Fatalf("%s: expected %s %q, got %v", c.name, c.field, c.want, out[c.field])
		}
	}
}

// errorField returns a string field of the {"error": {...}} object in out.
func errorField(out map[string]any, key string) string {
	obj, _ := out["error"].(map[string]any)
	s, _ := obj[key].(string)
	return s
}
