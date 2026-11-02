package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
)

// newLoginRouter builds a router whose auth service uses the client returned
// by limiterRedis, and registers a fresh email user with password
// "correct-horse".
func newLoginRouter(t *testing.T, limiterRedis func(real *goredis.Client) *goredis.Client) (http.Handler, string) {
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

	email := "login-limit-" + uuid.NewString() + "@example.com"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Exec(cleanupCtx, "DELETE FROM users WHERE email = $1", email)
		_ = redisClient.Del(cleanupCtx, "login_attempts:"+email).Err()
	})

	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	service := auth.NewService(auth.NewRepository(db), tokenManager, auth.NewOAuthRegistry(auth.OAuthConfig{}),
		cfg.JWTRefreshTTL, cfg.FrontendCallbackURL, limiterRedis(redisClient))
	if _, err := service.RegisterUser(ctx, email, "correct-horse", "Limit Tester"); err != nil {
		t.Fatalf("register user: %v", err)
	}

	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthHandler: auth.NewHandler(service),
		AuthMW:      auth.NewMiddleware(tokenManager),
	})
	return router, email
}

func login(t *testing.T, router http.Handler, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestLoginRateLimitRetryAfter(t *testing.T) {
	router, email := newLoginRouter(t, func(real *goredis.Client) *goredis.Client { return real })

	for i := range 5 {
		if rec := login(t, router, email, "wrong-password"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	// The 6th attempt within the window is limited, even with the right password.
	for _, password := range []string{"wrong-password", "correct-horse"} {
		rec := login(t, router, email, password)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
		}
		secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		if err != nil || secs < 1 || secs > int((15*time.Minute).Seconds()) {
			t.Fatalf("expected Retry-After of 1..900 seconds, got %q", rec.Header().Get("Retry-After"))
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "RATE_LIMITED" || body.Error.Message != "too many login attempts, try again later" {
			t.Fatalf("unexpected 429 body %s", rec.Body.String())
		}
	}
}

func TestLoginRateLimitFailsOpen(t *testing.T) {
	// Nothing listens on port 1, so every limiter call fails.
	router, email := newLoginRouter(t, func(*goredis.Client) *goredis.Client {
		broken := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
		t.Cleanup(func() { _ = broken.Close() })
		return broken
	})

	// Far more than the limit, and none of them is a 429 or a 500.
	for i := range 7 {
		if rec := login(t, router, email, "wrong-password"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d with Redis down: expected 401, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec := login(t, router, email, "correct-horse"); rec.Code != http.StatusOK {
		t.Fatalf("expected login 200 with Redis down, got %d: %s", rec.Code, rec.Body.String())
	}
}
