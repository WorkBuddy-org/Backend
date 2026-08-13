package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/platform/httpx"
)

type contextKey string

const userIDContextKey contextKey = "userID"

type Middleware struct {
	tokens *TokenManager
}

func NewMiddleware(tokens *TokenManager) *Middleware {
	return &Middleware{tokens: tokens}
}

func (m *Middleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := bearerToken(r.Header.Get("Authorization"))
		if err != nil {
			httpx.Error(w, http.StatusUnauthorized, "missing or invalid authorization header")
			return
		}

		claims, err := m.tokens.ParseAccessToken(token)
		if err != nil {
			httpx.Error(w, http.StatusUnauthorized, "invalid access token")
			return
		}

		ctx := context.WithValue(r.Context(), userIDContextKey, claims.Subject)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func UserIDFromContext(ctx context.Context) (string, bool) {
	userID, ok := ctx.Value(userIDContextKey).(string)
	return userID, ok
}

// CurrentUserID extracts and parses the authenticated user's ID from the
// request context. It requires RequireAuth to have run first.
func CurrentUserID(r *http.Request) (uuid.UUID, error) {
	raw, ok := UserIDFromContext(r.Context())
	if !ok {
		return uuid.Nil, fmt.Errorf("missing user context")
	}
	return uuid.Parse(raw)
}

func bearerToken(header string) (string, error) {
	if header == "" {
		return "", fmt.Errorf("missing authorization header")
	}

	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", fmt.Errorf("invalid authorization header")
	}

	return parts[1], nil
}

func parseUserID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, ErrInvalidToken
	}
	return id, nil
}
