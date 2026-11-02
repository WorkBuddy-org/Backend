package auth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
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
	"github.com/workbuddy/backend/internal/platform/ratelimit"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
)

// newRegisterRouter builds a router that trusts X-Forwarded-For and limits
// registration to limit requests per 10 minutes, using the client returned
// by limiterRedis. randomIP returns a fresh client IP whose limiter key is
// removed when the test ends.
func newRegisterRouter(t *testing.T, limit int, limiterRedis func(real *goredis.Client) *goredis.Client) (router http.Handler, randomIP func() string) {
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

	var ips []string
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.Exec(cleanupCtx, "DELETE FROM users WHERE email LIKE 'register-limit-%@example.com'")
		for _, ip := range ips {
			_ = redisClient.Del(cleanupCtx, "register_attempts:"+ip).Err()
		}
	})

	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	service := auth.NewService(auth.NewRepository(db), tokenManager, auth.NewOAuthRegistry(auth.OAuthConfig{}),
		cfg.JWTRefreshTTL, cfg.FrontendCallbackURL, redisClient)
	handler := auth.NewHandler(service)
	handler.SetRegisterLimiter(ratelimit.NewFixedWindow(limiterRedis(redisClient), limit, 10*time.Minute))

	router = httpapi.NewRouter(httpapi.RouterDeps{
		DB:                 db,
		Redis:              redisClient,
		AuthHandler:        handler,
		AuthMW:             auth.NewMiddleware(tokenManager),
		TrustedProxyHeader: "X-Forwarded-For",
	})
	return router, func() string {
		ip := fmt.Sprintf("10.%d.%d.%d", rand.IntN(256), rand.IntN(256), rand.IntN(254)+1)
		ips = append(ips, ip)
		return ip
	}
}

func register(t *testing.T, router http.Handler, forwardedFor string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"email":    "register-limit-" + uuid.NewString() + "@example.com",
		"password": "correct-horse",
		"name":     "Register Tester",
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", forwardedFor)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestRegisterRateLimitPerIP(t *testing.T) {
	router, randomIP := newRegisterRouter(t, 3, func(real *goredis.Client) *goredis.Client { return real })
	ip := randomIP()

	// Only the first X-Forwarded-For value is the client.
	for i := range 3 {
		if rec := register(t, router, ip+", 172.16.0.1"); rec.Code != http.StatusCreated {
			t.Fatalf("attempt %d: expected 201, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := register(t, router, ip)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs < 1 || secs > int((10*time.Minute).Seconds()) {
		t.Fatalf("expected Retry-After of 1..600 seconds, got %q", rec.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "RATE_LIMITED" || body.Error.Message != "too many registration attempts, try again later" {
		t.Fatalf("unexpected 429 body %s", rec.Body.String())
	}

	// The limit is per client IP: another address is unaffected.
	if rec := register(t, router, randomIP()); rec.Code != http.StatusCreated {
		t.Fatalf("other ip: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterRateLimitFailsOpen(t *testing.T) {
	// Nothing listens on port 1, so every limiter call fails.
	router, randomIP := newRegisterRouter(t, 1, func(*goredis.Client) *goredis.Client {
		broken := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
		t.Cleanup(func() { _ = broken.Close() })
		return broken
	})
	ip := randomIP()

	for i := range 3 {
		if rec := register(t, router, ip); rec.Code != http.StatusCreated {
			t.Fatalf("attempt %d with Redis down: expected 201, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
}
