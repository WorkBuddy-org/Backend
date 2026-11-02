package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/user"
)

func TestRegisterAndLoginFlow(t *testing.T) {
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

	repo := auth.NewRepository(db)
	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	service := auth.NewService(repo, tokenManager, auth.NewOAuthRegistry(auth.OAuthConfig{}), cfg.JWTRefreshTTL, cfg.FrontendCallbackURL, redisClient)
	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthHandler: auth.NewHandler(service),
		AuthMW:      auth.NewMiddleware(tokenManager),
	})

	registerEmail := "register-flow@example.com"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Exec(cleanupCtx, "DELETE FROM users WHERE email = $1", registerEmail)
	})

	registerBody, _ := json.Marshal(map[string]string{
		"email":    registerEmail,
		"password": "correct-password",
		"name":     "Register Flow",
	})
	registerReq := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerRec := httptest.NewRecorder()
	router.ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusCreated {
		t.Fatalf("expected register 201, got %d: %s", registerRec.Code, registerRec.Body.String())
	}

	assertSignedIn(t, registerRec, registerEmail)

	// Registering the same email again must be rejected as a duplicate.
	dupRec := httptest.NewRecorder()
	dupReq := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewReader(registerBody))
	dupReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusConflict {
		t.Fatalf("expected duplicate register 409, got %d: %s", dupRec.Code, dupRec.Body.String())
	}
	assertErrorCode(t, dupRec.Body.String(), "EMAIL_EXISTS", "")

	// A password under 8 characters must be rejected.
	weakBody, _ := json.Marshal(map[string]string{
		"email":    "weak-password@example.com",
		"password": "short",
		"name":     "Weak Password",
	})
	weakRec := httptest.NewRecorder()
	weakReq := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewReader(weakBody))
	weakReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(weakRec, weakReq)
	if weakRec.Code != http.StatusBadRequest {
		t.Fatalf("expected weak password register 400, got %d: %s", weakRec.Code, weakRec.Body.String())
	}
	assertErrorCode(t, weakRec.Body.String(), "VALIDATION_ERROR", `"fields":{"password":"TOO_SHORT"}`)

	// Correct credentials must authenticate and issue tokens.
	loginBody, _ := json.Marshal(map[string]string{
		"email":    registerEmail,
		"password": "correct-password",
	})
	loginRec := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", loginRec.Code, loginRec.Body.String())
	}

	assertSignedIn(t, loginRec, registerEmail)

	// Wrong password must be rejected without leaking which part was wrong.
	wrongBody, _ := json.Marshal(map[string]string{
		"email":    registerEmail,
		"password": "totally-wrong-password",
	})
	wrongRec := httptest.NewRecorder()
	wrongReq := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(wrongBody))
	wrongReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(wrongRec, wrongReq)
	if wrongRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected wrong password login 401, got %d: %s", wrongRec.Code, wrongRec.Body.String())
	}
	assertErrorCode(t, wrongRec.Body.String(), "INVALID_CREDENTIALS", "")

	// An oauth-only account has no password set; logging in with a password
	// must fail the same way as any other invalid credentials, not 500 and
	// not reveal that the account exists via a different error.
	oauthUser := createTestUser(t, db, "oauth-only-flow@example.com")
	// Failed logins count against the email for 15 minutes; start clean so
	// repeated runs do not hit the login limit.
	_ = redisClient.Del(ctx, "login_attempts:"+oauthUser.Email).Err()
	oauthLoginBody, _ := json.Marshal(map[string]string{
		"email":    oauthUser.Email,
		"password": "any-password",
	})
	oauthLoginRec := httptest.NewRecorder()
	oauthLoginReq := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(oauthLoginBody))
	oauthLoginReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(oauthLoginRec, oauthLoginReq)
	if oauthLoginRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected oauth-only account login 401, got %d: %s", oauthLoginRec.Code, oauthLoginRec.Body.String())
	}
	assertErrorCode(t, oauthLoginRec.Body.String(), "INVALID_CREDENTIALS", "")
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

// assertErrorCode checks that body is an error response with the given code
// and, when wantDetails is not empty, that its raw details contain it.
func assertErrorCode(t *testing.T, body, wantCode, wantDetails string) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	if resp.Error.Code != wantCode || resp.Error.Message == "" {
		t.Fatalf("expected error code %s with a message, got %s", wantCode, body)
	}
	if wantDetails != "" && !bytes.Contains(resp.Error.Details, []byte(wantDetails)) {
		t.Fatalf("expected details containing %s, got %s", wantDetails, body)
	}
}

// assertSignedIn checks a register/login response: {"user": ...} for email in
// the body and both auth cookies set.
func assertSignedIn(t *testing.T, rec *httptest.ResponseRecorder, email string) {
	t.Helper()
	var resp struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.User.ID == "" || resp.User.Email != email {
		t.Fatalf("expected {\"user\": ...} for %s, got %s", email, rec.Body.String())
	}
	requireCookie(t, rec, auth.AccessTokenCookie)
	requireCookie(t, rec, auth.RefreshTokenCookie)
}
