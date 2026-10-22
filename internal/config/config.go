package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port string
	Env  string

	LogLevel string

	DatabaseURL string
	RedisURL    string

	JWTSecret     string
	JWTAccessTTL  time.Duration
	JWTRefreshTTL time.Duration

	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string

	GitHubClientID     string
	GitHubClientSecret string
	GitHubRedirectURL  string

	FrontendCallbackURL string

	// WSAllowedOrigins lists browser origins (scheme://host[:port]) allowed to
	// open WebSockets. Empty means any origin, but only when Env is development.
	WSAllowedOrigins []string

	// ExtensionPromptTimeout is how long a user has to answer the task
	// extension prompt before being removed from the room.
	ExtensionPromptTimeout time.Duration
	// ExtensionMaxCount caps extensions per task.
	ExtensionMaxCount int

	// LiveKit settings for audio/video calls. When any is empty, call token
	// requests answer 503.
	LiveKitURL       string
	LiveKitAPIKey    string
	LiveKitAPISecret string
}

func Load() (*Config, error) {
	env := os.Getenv("ENV")
	if env == "" || env == "development" {
		_ = godotenv.Load()
	}

	cfg := &Config{
		Port:     getEnv("PORT", "8080"),
		Env:      getEnv("ENV", "development"),
		LogLevel: getEnv("LOG_LEVEL", "info"),

		DatabaseURL: getEnv("DATABASE_URL", ""),
		RedisURL:    getEnv("REDIS_URL", ""),

		JWTSecret: getEnv("JWT_SECRET", ""),

		GoogleClientID:     getEnv("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: getEnv("GOOGLE_CLIENT_SECRET", ""),
		GoogleRedirectURL:  getEnv("GOOGLE_REDIRECT_URL", ""),

		GitHubClientID:     getEnv("GITHUB_CLIENT_ID", ""),
		GitHubClientSecret: getEnv("GITHUB_CLIENT_SECRET", ""),
		GitHubRedirectURL:  getEnv("GITHUB_REDIRECT_URL", ""),

		FrontendCallbackURL: getEnv("FRONTEND_CALLBACK_URL", ""),

		WSAllowedOrigins: splitList(getEnv("WS_ALLOWED_ORIGINS", "")),

		LiveKitURL:       getEnv("LIVEKIT_URL", ""),
		LiveKitAPIKey:    getEnv("LIVEKIT_API_KEY", ""),
		LiveKitAPISecret: getEnv("LIVEKIT_API_SECRET", ""),
	}

	var err error
	cfg.JWTAccessTTL, err = time.ParseDuration(getEnv("JWT_ACCESS_TTL", "15m"))
	if err != nil {
		return nil, fmt.Errorf("parse JWT_ACCESS_TTL: %w", err)
	}

	cfg.JWTRefreshTTL, err = time.ParseDuration(getEnv("JWT_REFRESH_TTL", "720h"))
	if err != nil {
		return nil, fmt.Errorf("parse JWT_REFRESH_TTL: %w", err)
	}

	cfg.ExtensionPromptTimeout, err = time.ParseDuration(getEnv("EXTENSION_PROMPT_TIMEOUT", "60s"))
	if err != nil || cfg.ExtensionPromptTimeout <= 0 {
		return nil, fmt.Errorf("parse EXTENSION_PROMPT_TIMEOUT: must be a positive duration such as 60s")
	}

	cfg.ExtensionMaxCount, err = strconv.Atoi(getEnv("EXTENSION_MAX_COUNT", "3"))
	if err != nil || cfg.ExtensionMaxCount < 0 {
		return nil, fmt.Errorf("parse EXTENSION_MAX_COUNT: must be a non-negative integer")
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.RedisURL == "" {
		return nil, fmt.Errorf("REDIS_URL is required")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}

	if cfg.Env == "production" {
		if cfg.FrontendCallbackURL == "" {
			return nil, fmt.Errorf("FRONTEND_CALLBACK_URL is required in production")
		}
		if cfg.GoogleClientID == "" || cfg.GoogleClientSecret == "" || cfg.GoogleRedirectURL == "" {
			return nil, fmt.Errorf("production requires Google OAuth configuration: set GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, and GOOGLE_REDIRECT_URL")
		}
		if cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "" || cfg.GitHubRedirectURL == "" {
			return nil, fmt.Errorf("production requires GitHub OAuth configuration: set GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET, and GITHUB_REDIRECT_URL")
		}
	}

	return cfg, nil
}

// splitList parses a comma-separated value, dropping blank entries.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
