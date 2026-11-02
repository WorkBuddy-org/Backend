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
	"github.com/workbuddy/backend/internal/user"
)

type meEnv struct {
	t      *testing.T
	router http.Handler
	token  string
}

func newMeEnv(t *testing.T) *meEnv {
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

	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:             db,
		Redis:          redisClient,
		AuthMW:         auth.NewMiddleware(tokens),
		ProfileHandler: profile.NewHandler(profile.NewService(profile.NewRepository(db))),
		AuthHandler:    auth.NewHandler(auth.NewService(auth.NewRepository(db), tokens, auth.NewOAuthRegistry(auth.OAuthConfig{}), cfg.JWTRefreshTTL, cfg.FrontendCallbackURL, redisClient)),
	})

	email := "me-" + uuid.NewString() + "@example.com"
	u, err := auth.NewRepository(db).CreateEmailUser(ctx, email, "Ada", "Lovelace", "not-a-real-hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
	token, err := tokens.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return &meEnv{t: t, router: router, token: token}
}

func (e *meEnv) do(method, path, body string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (e *meEnv) json(method, path, body string, want int) map[string]any {
	e.t.Helper()
	code, resp := e.do(method, path, body)
	if code != want {
		e.t.Fatalf("%s %s %s: expected %d, got %d: %s", method, path, body, want, code, resp)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		e.t.Fatalf("decode %s: %v", resp, err)
	}
	return out
}

func TestGetMe(t *testing.T) {
	e := newMeEnv(t)
	me := e.json(http.MethodGet, "/me", "", http.StatusOK)

	for field, want := range map[string]any{
		"first_name": "Ada", "last_name": "Lovelace", "name": "Ada Lovelace", "plan": "free",
		"onboarding_completed": false, "avatar_url": nil, "profession": nil, "profession_other": nil, "goal": nil, "experience": nil,
	} {
		got, ok := me[field]
		if !ok || got != want {
			t.Fatalf("%s: expected %v, got %v (present %v)", field, want, got, ok)
		}
	}
	if me["id"] == "" || me["email"] == "" {
		t.Fatalf("expected id and email, got %v", me)
	}
	if _, ok := me["password_hash"]; ok {
		t.Fatal("password_hash must not be exposed")
	}

	e.token = ""
	if code, _ := e.do(http.MethodGet, "/me", ""); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d", code)
	}
}

func TestProfileEndpointsStillWork(t *testing.T) {
	e := newMeEnv(t)
	if got := e.json(http.MethodGet, "/profile", "", http.StatusOK); got["name"] != "Ada Lovelace" {
		t.Fatalf("unexpected profile %v", got)
	}
	if got := e.json(http.MethodPatch, "/profile", `{"role":"Dev"}`, http.StatusOK); got["role"] != "Dev" {
		t.Fatalf("unexpected profile %v", got)
	}
}

func TestOnboardingFlow(t *testing.T) {
	e := newMeEnv(t)

	got := e.json(http.MethodGet, "/me/onboarding", "", http.StatusOK)
	if got["onboarding_completed"] != false || got["goal"] != nil || len(got) != 5 {
		t.Fatalf("unexpected initial onboarding %v", got)
	}

	// Without a profession onboarding is not completed yet.
	got = e.json(http.MethodPatch, "/me/onboarding", `{"goal":"improve_focus","experience":"1_3"}`, http.StatusOK)
	if got["goal"] != "improve_focus" || got["experience"] != "1_3" || got["onboarding_completed"] != false {
		t.Fatalf("unexpected onboarding %v", got)
	}

	got = e.json(http.MethodPatch, "/me/onboarding", `{"profession":"other","profession_other":"  Data engineer "}`, http.StatusOK)
	if got["profession"] != "other" || got["profession_other"] != "Data engineer" || got["onboarding_completed"] != true || got["goal"] != "improve_focus" {
		t.Fatalf("unexpected onboarding %v", got)
	}

	// profession_other can be sent alone while the stored profession is other.
	got = e.json(http.MethodPatch, "/me/onboarding", `{"profession_other":"Analyst"}`, http.StatusOK)
	if got["profession_other"] != "Analyst" {
		t.Fatalf("unexpected onboarding %v", got)
	}

	// Moving away from other clears it; completed stays true.
	got = e.json(http.MethodPatch, "/me/onboarding", `{"profession":"qa"}`, http.StatusOK)
	if got["profession"] != "qa" || got["profession_other"] != nil || got["onboarding_completed"] != true {
		t.Fatalf("unexpected onboarding %v", got)
	}

	if me := e.json(http.MethodGet, "/me", "", http.StatusOK); me["profession"] != "qa" || me["goal"] != "improve_focus" {
		t.Fatalf("/me must reflect onboarding, got %v", me)
	}
}

func TestOnboardingEnums(t *testing.T) {
	e := newMeEnv(t)
	for field, allowed := range map[string][]string{
		"goal":       user.Goals,
		"experience": user.Experiences,
		"profession": user.Professions,
	} {
		for _, value := range allowed {
			e.json(http.MethodPatch, "/me/onboarding", `{"`+field+`":"`+value+`"}`, http.StatusOK)
		}
	}
}

func TestOnboardingValidation(t *testing.T) {
	e := newMeEnv(t)
	for _, tc := range []struct{ name, body, want string }{
		{"empty body", `{}`, `"code":"VALIDATION_ERROR","message":"no fields to update"`},
		{"all null", `{"goal":null}`, `"message":"no fields to update"`},
		{"bad goal", `{"goal":"nope"}`, `"goal":"INVALID_VALUE"`},
		{"bad experience", `{"experience":"10"}`, `"experience":"INVALID_VALUE"`},
		{"bad profession", `{"profession":"chef"}`, `"profession":"INVALID_VALUE"`},
		{"other without profession", `{"profession_other":"Chef"}`, `"profession_other":"INVALID_VALUE"`},
		{"other with another profession", `{"profession":"qa","profession_other":"Chef"}`, `"profession_other":"INVALID_VALUE"`},
		{"other too long", `{"profession":"other","profession_other":"` + strings.Repeat("x", 101) + `"}`, `"profession_other":"TOO_LONG"`},
		{"unknown field", `{"plan":"pro"}`, `"plan":"UNKNOWN_FIELD"`},
		{"wrong type", `{"goal":1}`, `"goal":"INVALID_TYPE"`},
	} {
		code, body := e.do(http.MethodPatch, "/me/onboarding", tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Fatalf("%s: expected 400 with %s, got %d: %s", tc.name, tc.want, code, body)
		}
	}

	// Several bad fields are reported together, and nothing is saved.
	code, body := e.do(http.MethodPatch, "/me/onboarding", `{"goal":"improve_focus","experience":"bad","profession":"bad"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, `"experience":"INVALID_VALUE"`) || !strings.Contains(body, `"profession":"INVALID_VALUE"`) {
		t.Fatalf("expected both fields in %d: %s", code, body)
	}
	if got := e.json(http.MethodGet, "/me/onboarding", "", http.StatusOK); got["goal"] != nil {
		t.Fatalf("a rejected request must save nothing, got %v", got)
	}
}

func TestOnboardingSkip(t *testing.T) {
	e := newMeEnv(t)
	e.json(http.MethodPatch, "/me/onboarding", `{"goal":"improve_skills"}`, http.StatusOK)

	got := e.json(http.MethodPost, "/me/onboarding/skip", "", http.StatusOK)
	if got["onboarding_completed"] != true || got["goal"] != "improve_skills" || got["profession"] != nil {
		t.Fatalf("skip must only set completed, got %v", got)
	}
	if me := e.json(http.MethodGet, "/me", "", http.StatusOK); me["onboarding_completed"] != true {
		t.Fatalf("/me must show completed, got %v", me)
	}
}
