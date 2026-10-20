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

func TestRefreshAndLogoutFlow(t *testing.T) {
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
	handler := auth.NewHandler(service)
	middleware := auth.NewMiddleware(tokenManager)
	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthHandler: handler,
		AuthMW:      middleware,
	})

	testUser := createTestUser(t, db, "refresh-flow@example.com")
	pair, err := service.IssueTokenPair(ctx, testUser)
	if err != nil {
		t.Fatalf("issue token pair: %v", err)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	meRec := httptest.NewRecorder()
	router.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("expected /auth/me 200, got %d: %s", meRec.Code, meRec.Body.String())
	}

	refreshBody, _ := json.Marshal(map[string]string{"refresh_token": pair.RefreshToken})
	refreshReq := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewReader(refreshBody))
	refreshReq.Header.Set("Content-Type", "application/json")
	refreshRec := httptest.NewRecorder()
	router.ServeHTTP(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusOK {
		t.Fatalf("expected /auth/refresh 200, got %d: %s", refreshRec.Code, refreshRec.Body.String())
	}

	var refreshed auth.TokenPair
	if err := json.NewDecoder(refreshRec.Body).Decode(&refreshed); err != nil {
		t.Fatalf("decode refresh response: %v", err)
	}
	if refreshed.AccessToken == "" || refreshed.RefreshToken == "" {
		t.Fatal("expected refreshed token pair")
	}
	if refreshed.RefreshToken == pair.RefreshToken {
		t.Fatal("expected rotated refresh token")
	}

	logoutBody, _ := json.Marshal(map[string]string{"refresh_token": refreshed.RefreshToken})
	logoutReq := httptest.NewRequest(http.MethodPost, "/auth/logout", bytes.NewReader(logoutBody))
	logoutReq.Header.Set("Content-Type", "application/json")
	logoutRec := httptest.NewRecorder()
	router.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("expected /auth/logout 204, got %d: %s", logoutRec.Code, logoutRec.Body.String())
	}

	staleRefreshBody, _ := json.Marshal(map[string]string{"refresh_token": pair.RefreshToken})
	staleRefreshReq := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewReader(staleRefreshBody))
	staleRefreshReq.Header.Set("Content-Type", "application/json")
	staleRefreshRec := httptest.NewRecorder()
	router.ServeHTTP(staleRefreshRec, staleRefreshReq)
	if staleRefreshRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected stale refresh 401, got %d: %s", staleRefreshRec.Code, staleRefreshRec.Body.String())
	}
}

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

	var registered auth.TokenPair
	if err := json.NewDecoder(registerRec.Body).Decode(&registered); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if registered.AccessToken == "" || registered.RefreshToken == "" {
		t.Fatal("expected tokens from register")
	}

	// Registering the same email again must be rejected as a duplicate.
	dupRec := httptest.NewRecorder()
	dupReq := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewReader(registerBody))
	dupReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusConflict {
		t.Fatalf("expected duplicate register 409, got %d: %s", dupRec.Code, dupRec.Body.String())
	}

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

	var loggedIn auth.TokenPair
	if err := json.NewDecoder(loginRec.Body).Decode(&loggedIn); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if loggedIn.AccessToken == "" || loggedIn.RefreshToken == "" {
		t.Fatal("expected tokens from login")
	}

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

	// An oauth-only account has no password set; logging in with a password
	// must fail the same way as any other invalid credentials, not 500 and
	// not reveal that the account exists via a different error.
	oauthUser := createTestUser(t, db, "oauth-only-flow@example.com")
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
