package auth

import (
	"context"
	"errors"
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
			httpx.Unauthorized(w, "missing or invalid authorization header")
			return
		}

		claims, err := m.tokens.ParseAccessToken(token)
		if err != nil {
			WriteTokenError(w, err)
			return
		}

		ctx := context.WithValue(r.Context(), userIDContextKey, claims.Subject)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WriteTokenError writes the 401 for an access token that failed to parse:
// TOKEN_EXPIRED when it is genuine but expired, so the client knows to
// refresh, and UNAUTHORIZED otherwise.
func WriteTokenError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrTokenExpired) {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeTokenExpired, "access token expired")
		return
	}
	httpx.Unauthorized(w, "invalid access token")
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
