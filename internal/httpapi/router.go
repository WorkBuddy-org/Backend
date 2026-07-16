package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/platform/httpx"
)

type HealthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

type RouterDeps struct {
	DB          *pgxpool.Pool
	Redis       *goredis.Client
	AuthHandler *auth.Handler
	AuthMW      *auth.Middleware
}

func NewRouter(deps RouterDeps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/health", healthHandler(deps.DB, deps.Redis))
	deps.AuthHandler.RegisterRoutes(r, deps.AuthMW)

	return r
}

func healthHandler(db *pgxpool.Pool, redisClient *goredis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		checks := map[string]string{
			"postgres": "ok",
			"redis":    "ok",
		}
		status := http.StatusOK
		overall := "ok"

		if err := db.Ping(ctx); err != nil {
			checks["postgres"] = "unavailable"
			overall = "degraded"
			status = http.StatusServiceUnavailable
			slog.Warn("health check failed", "component", "postgres", "error", err)
		}

		if err := redisClient.Ping(ctx).Err(); err != nil {
			checks["redis"] = "unavailable"
			overall = "degraded"
			status = http.StatusServiceUnavailable
			slog.Warn("health check failed", "component", "redis", "error", err)
		}

		httpx.JSON(w, status, HealthResponse{
			Status: overall,
			Checks: checks,
		})
	}
}

func PingDependencies(ctx context.Context, db *pgxpool.Pool, redisClient *goredis.Client) error {
	if err := db.Ping(ctx); err != nil {
		return err
	}
	return redisClient.Ping(ctx).Err()
}
