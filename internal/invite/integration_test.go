package invite_test

import (
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
	"github.com/workbuddy/backend/internal/invite"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/user"
)

func TestCreateAndCheckInviteValidity(t *testing.T) {
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

	inviteService := invite.NewService(invite.NewRepository(db))
	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:            db,
		Redis:         redisClient,
		AuthHandler:   auth.NewHandler(authService),
		AuthMW:        auth.NewMiddleware(tokenManager),
		InviteHandler: invite.NewHandler(inviteService),
	})

	testUser := createTestUser(t, db, "invite-flow@example.com")
	pair, err := authService.IssueTokenPair(ctx, testUser)
	if err != nil {
		t.Fatalf("issue token pair: %v", err)
	}

	// Creating an invite requires auth.
	unauthReq := httptest.NewRequest(http.MethodPost, "/invites", nil)
	unauthRec := httptest.NewRecorder()
	router.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated create 401, got %d: %s", unauthRec.Code, unauthRec.Body.String())
	}

	createReq := httptest.NewRequest(http.MethodPost, "/invites", nil)
	createReq.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create 201, got %d: %s", createRec.Code, createRec.Body.String())
	}

	var created struct {
		ID        string `json:"id"`
		Code      string `json:"code"`
		CreatedBy string `json:"created_by"`
	}
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Code == "" {
		t.Fatal("expected a non-empty invite code")
	}
	if created.CreatedBy != testUser.ID.String() {
		t.Fatalf("expected created_by %s, got %s", testUser.ID, created.CreatedBy)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Exec(cleanupCtx, "DELETE FROM invites WHERE id = $1", created.ID)
	})

	// The redeem-check endpoint is public and doesn't require auth.
	checkReq := httptest.NewRequest(http.MethodGet, "/invites/"+created.Code, nil)
	checkRec := httptest.NewRecorder()
	router.ServeHTTP(checkRec, checkReq)
	if checkRec.Code != http.StatusOK {
		t.Fatalf("expected check 200, got %d: %s", checkRec.Code, checkRec.Body.String())
	}

	var validity struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(checkRec.Body).Decode(&validity); err != nil {
		t.Fatalf("decode check response: %v", err)
	}
	if !validity.Valid {
		t.Fatal("expected freshly created invite to be valid")
	}

	unknownReq := httptest.NewRequest(http.MethodGet, "/invites/does-not-exist", nil)
	unknownRec := httptest.NewRecorder()
	router.ServeHTTP(unknownRec, unknownReq)
	if unknownRec.Code != http.StatusOK {
		t.Fatalf("expected unknown code check 200, got %d: %s", unknownRec.Code, unknownRec.Body.String())
	}

	var unknownValidity struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(unknownRec.Body).Decode(&unknownValidity); err != nil {
		t.Fatalf("decode unknown check response: %v", err)
	}
	if unknownValidity.Valid {
		t.Fatal("expected unknown code to be invalid")
	}
	if unknownValidity.Reason != "not_found" {
		t.Fatalf("expected reason not_found, got %q", unknownValidity.Reason)
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
