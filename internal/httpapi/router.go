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
	"github.com/workbuddy/backend/internal/call"
	"github.com/workbuddy/backend/internal/chat"
	"github.com/workbuddy/backend/internal/invite"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/profile"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/task"
	"github.com/workbuddy/backend/internal/ws"
)

type HealthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

type RouterDeps struct {
	DB             *pgxpool.Pool
	Redis          *goredis.Client
	AuthHandler    *auth.Handler
	AuthMW         *auth.Middleware
	RoomHandler    *room.Handler
	InviteHandler  *invite.Handler
	TaskHandler    *task.Handler
	ProfileHandler *profile.Handler
	ChatHandler    *chat.Handler
	WSHandler      *ws.Handler
	CallHandler    *call.Handler

	// TrustedProxyHeader, when set, names the header whose first value is
	// used as the client IP (see httpx.TrustedProxy).
	TrustedProxyHeader string
}

func NewRouter(deps RouterDeps) http.Handler {
	r := chi.NewRouter()

	r.Use(httpx.TrustedProxy(deps.TrustedProxyHeader))
	r.Use(middleware.RequestID)
	r.Use(httpx.Recoverer)
	r.NotFound(httpx.NotFoundHandler)
	r.MethodNotAllowed(httpx.MethodNotAllowedHandler)

	r.Get("/health", healthHandler(deps.DB, deps.Redis))
	deps.AuthHandler.RegisterRoutes(r, deps.AuthMW)

	if deps.RoomHandler != nil {
		deps.RoomHandler.RegisterRoutes(r, deps.AuthMW)
	}
	if deps.InviteHandler != nil {
		deps.InviteHandler.RegisterRoutes(r, deps.AuthMW)
	}
	if deps.TaskHandler != nil {
		deps.TaskHandler.RegisterRoutes(r, deps.AuthMW)
	}
	if deps.ProfileHandler != nil {
		deps.ProfileHandler.RegisterRoutes(r, deps.AuthMW)
	}
	if deps.ChatHandler != nil {
		deps.ChatHandler.RegisterRoutes(r, deps.AuthMW)
	}
	if deps.WSHandler != nil {
		deps.WSHandler.RegisterRoutes(r)
	}
	if deps.CallHandler != nil {
		deps.CallHandler.RegisterRoutes(r, deps.AuthMW)
	}

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
