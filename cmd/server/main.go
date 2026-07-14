package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/platform/postgres"
	"github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/server"
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
	authService := auth.NewService(authRepo, tokenManager, oauthRegistry, cfg.JWTRefreshTTL, cfg.FrontendCallbackURL)

	router := httpx.NewRouter(httpx.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthHandler: auth.NewHandler(authService),
		AuthMW:      auth.NewMiddleware(tokenManager),
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
