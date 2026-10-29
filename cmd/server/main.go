package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/call"
	"github.com/workbuddy/backend/internal/chat"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/invite"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/platform/postgres"
	"github.com/workbuddy/backend/internal/platform/ratelimit"
	"github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/profile"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/server"
	"github.com/workbuddy/backend/internal/task"
	"github.com/workbuddy/backend/internal/ws"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx := context.Background()

	db, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	redisClient, err := redis.NewClient(cfg.RedisURL)
	if err != nil {
		slog.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			slog.Error("failed to close redis client", "error", err)
		}
	}()

	authRepo := auth.NewRepository(db)
	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	oauthRegistry := auth.NewOAuthRegistry(auth.OAuthConfig{
		GoogleClientID:     cfg.GoogleClientID,
		GoogleClientSecret: cfg.GoogleClientSecret,
		GoogleRedirectURL:  cfg.GoogleRedirectURL,
		GitHubClientID:     cfg.GitHubClientID,
		GitHubClientSecret: cfg.GitHubClientSecret,
		GitHubRedirectURL:  cfg.GitHubRedirectURL,
	})
	authService := auth.NewService(authRepo, tokenManager, oauthRegistry, cfg.JWTRefreshTTL, cfg.FrontendCallbackURL, redisClient)

	roomService := room.NewService(room.NewRepository(db))
	inviteService := invite.NewService(invite.NewRepository(db))
	profileService := profile.NewService(profile.NewRepository(db))

	// Presence lives in Redis but the hub is in memory, so with a single
	// instance any presence left over from a previous run is stale.
	wsPresence := ws.NewPresenceStore(redisClient, authRepo)
	if n, err := wsPresence.ResetAll(ctx); err != nil {
		slog.Error("failed to reset websocket presence", "error", err)
		os.Exit(1)
	} else if n > 0 {
		slog.Info("cleared stale websocket presence", "rooms", n)
	}
	wsHub := ws.NewHub()
	wsAllowAnyOrigin := len(cfg.WSAllowedOrigins) == 0 && cfg.Env == "development"
	if len(cfg.WSAllowedOrigins) == 0 && !wsAllowAnyOrigin {
		slog.Warn("WS_ALLOWED_ORIGINS is empty: browser WebSocket connections will be rejected")
	}
	wsHandler := ws.NewHandler(ws.HandlerDeps{
		Hub:      wsHub,
		Presence: wsPresence,
		Tokens:   tokenManager,
		Rooms:    roomService,
		Config: ws.Config{
			AllowedOrigins: cfg.WSAllowedOrigins,
			AllowAnyOrigin: wsAllowAnyOrigin,
		},
	})
	roomService.SetLeaveListener(wsHandler)

	taskService := task.NewService(task.NewRepository(db), roomService, task.TimerDeps{
		Index:  task.NewTimerIndex(redisClient, ""),
		Events: wsHub,
		Extension: task.ExtensionConfig{
			PromptTimeout: cfg.ExtensionPromptTimeout,
			MaxCount:      cfg.ExtensionMaxCount,
		},
	})

	// The scheduler rebuilds its Redis index from Postgres before polling, so
	// timers survive restarts; overdue ones fire on the first tick.
	schedulerCtx, stopScheduler := context.WithCancel(ctx)
	defer stopScheduler()
	timerScheduler := task.NewScheduler(taskService, task.DefaultSchedulerInterval)
	if err := timerScheduler.Start(schedulerCtx); err != nil {
		slog.Error("failed to start task timer scheduler", "error", err)
		os.Exit(1)
	}

	callService := call.NewService(call.Config{
		URL:       cfg.LiveKitURL,
		APIKey:    cfg.LiveKitAPIKey,
		APISecret: cfg.LiveKitAPISecret,
	}, roomService, authRepo)
	if !callService.Configured() {
		slog.Warn("LIVEKIT_URL, LIVEKIT_API_KEY or LIVEKIT_API_SECRET is empty: call token requests will return 503")
	}

	var chatLimiter chat.RateLimiter
	if cfg.ChatRateLimit > 0 {
		chatLimiter = chat.NewRedisRateLimiter(redisClient, cfg.ChatRateLimit, cfg.ChatRateWindow, "")
	}

	corsOrigins := httpx.NewOrigins(cfg.CORSAllowedOrigins)
	if len(corsOrigins.List()) == 0 {
		slog.Warn("CORS_ALLOWED_ORIGINS is empty: cross-origin browser requests and cookie-authenticated writes with an Origin header will be rejected")
	}

	authHandler := auth.NewHandler(authService)
	authHandler.SetCookieConfig(auth.CookieConfig{Secure: cfg.CookieSecure, Domain: cfg.CookieDomain})
	authHandler.SetAllowedOrigins(corsOrigins)
	authMW := auth.NewMiddleware(tokenManager)
	authMW.SetAllowedOrigins(corsOrigins)
	if cfg.RegisterRateLimit > 0 {
		authHandler.SetRegisterLimiter(ratelimit.NewFixedWindow(redisClient, cfg.RegisterRateLimit, cfg.RegisterRateWindow))
	}
	if cfg.TrustedProxyHeader == "" {
		slog.Info("TRUSTED_PROXY_HEADER is empty: client IPs are taken from the connection address")
	}

	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:             db,
		Redis:          redisClient,
		AuthHandler:    authHandler,
		AuthMW:         authMW,
		RoomHandler:    room.NewHandler(roomService),
		InviteHandler:  invite.NewHandler(inviteService),
		TaskHandler:    task.NewHandler(taskService),
		ProfileHandler: profile.NewHandler(profileService),
		ChatHandler:    chat.NewHandler(chat.NewService(chat.NewRepository(db), roomService, wsHub, chatLimiter)),
		WSHandler:      wsHandler,
		CallHandler:    call.NewHandler(callService),

		TrustedProxyHeader: cfg.TrustedProxyHeader,
		CORSAllowedOrigins: corsOrigins.List(),
	})
	addr := ":" + cfg.Port
	srv := server.New(addr, router)

	go func() {
		slog.Info("server starting", "addr", addr, "env", cfg.Env)
		if err := srv.Start(); err != nil {
			slog.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	slog.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stopScheduler()
	timerScheduler.Wait()

	// Shutdown does not close hijacked connections, so close sockets first;
	// their disconnect handling clears presence while Redis is still open.
	wsHub.CloseAll()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	slog.Info("server stopped")
}

func newLogger(level string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	return slog.New(handler)
}
