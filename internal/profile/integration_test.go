package profile_test

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
	"github.com/workbuddy/backend/internal/profile"
	"github.com/workbuddy/backend/internal/user"
)

func TestGetAndUpdateProfile(t *testing.T) {
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
	authService := auth.NewService(authRepo, tokenManager, auth.NewOAuthRegistry(auth.OAuthConfig{}), cfg.JWTRefreshTTL, cfg.FrontendCallbackURL)

	profileService := profile.NewService(profile.NewRepository(db))

	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:             db,
		Redis:          redisClient,
		AuthHandler:    auth.NewHandler(authService),
		AuthMW:         auth.NewMiddleware(tokenManager),
		ProfileHandler: profile.NewHandler(profileService),
	})

	testUser := createTestUser(t, db, "profile-flow@example.com")
	tokenPair, err := authService.IssueTokenPair(ctx, testUser)
	if err != nil {
		t.Fatalf("issue token pair: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/profile", nil)
	getReq.Header.Set("Authorization", "Bearer "+tokenPair.AccessToken)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected get 200, got %d: %s", getRec.Code, getRec.Body.String())
	}

	var fetched struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(getRec.Body).Decode(&fetched); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if fetched.Email != "profile-flow@example.com" {
		t.Fatalf("expected email to round-trip, got %q", fetched.Email)
	}

	patchBody, _ := json.Marshal(map[string]string{
		"name":     "Updated Name",
		"timezone": "Europe/Warsaw",
	})
	patchReq := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewReader(patchBody))
	patchReq.Header.Set("Authorization", "Bearer "+tokenPair.AccessToken)
	patchReq.Header.Set("Content-Type", "application/json")
	patchRec := httptest.NewRecorder()
	router.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("expected patch 200, got %d: %s", patchRec.Code, patchRec.Body.String())
	}

	var patched struct {
		Name     string  `json:"name"`
		Role     *string `json:"role,omitempty"`
		Timezone *string `json:"timezone,omitempty"`
	}
	if err := json.NewDecoder(patchRec.Body).Decode(&patched); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	if patched.Name != "Updated Name" {
		t.Fatalf("expected name to be updated, got %q", patched.Name)
	}
	if patched.Timezone == nil || *patched.Timezone != "Europe/Warsaw" {
		t.Fatalf("expected timezone to be updated, got %v", patched.Timezone)
	}
	if patched.Role != nil {
		t.Fatalf("expected role to remain unset, got %v", *patched.Role)
	}

	emptyPatchReq := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewReader([]byte("{}")))
	emptyPatchReq.Header.Set("Authorization", "Bearer "+tokenPair.AccessToken)
	emptyPatchReq.Header.Set("Content-Type", "application/json")
	emptyPatchRec := httptest.NewRecorder()
	router.ServeHTTP(emptyPatchRec, emptyPatchReq)
	if emptyPatchRec.Code != http.StatusBadRequest {
		t.Fatalf("expected no-fields patch 400, got %d: %s", emptyPatchRec.Code, emptyPatchRec.Body.String())
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
