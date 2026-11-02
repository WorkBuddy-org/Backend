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
	tokens  *TokenManager
	origins *httpx.Origins
}

func NewMiddleware(tokens *TokenManager) *Middleware {
	return &Middleware{tokens: tokens}
}

// SetAllowedOrigins sets the origins (CORS_ALLOWED_ORIGINS) that may send
// cookie-authenticated POST/PUT/PATCH/DELETE requests. With none set, any
// such request carrying an Origin header is rejected.
func (m *Middleware) SetAllowedOrigins(origins *httpx.Origins) {
	m.origins = origins
}

// RequireAuth authenticates the request by the access_token cookie or, when
// there is none, the Authorization: Bearer header. Cookie-authenticated
// state-changing requests must also pass the Origin check; Bearer requests
// can't be forged cross-site, so they are exempt.
func (m *Middleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, viaCookie, err := requestAccessToken(r)
		if err != nil {
			httpx.Unauthorized(w, "missing or invalid authorization header")
			return
		}

		if viaCookie && isUnsafeMethod(r.Method) && !originAllowed(r, m.origins) {
			httpx.Forbidden(w, "origin not allowed")
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

// requestAccessToken returns the access token from the access_token cookie,
// falling back to the Authorization header; viaCookie tells which one.
func requestAccessToken(r *http.Request) (token string, viaCookie bool, err error) {
	if c, err := r.Cookie(AccessTokenCookie); err == nil && c.Value != "" {
		return c.Value, true, nil
	}
	token, err = bearerToken(r.Header.Get("Authorization"))
	return token, false, err
}

// AccessTokenFromCookie returns the access_token cookie's value, or "".
func AccessTokenFromCookie(r *http.Request) string {
	if c, err := r.Cookie(AccessTokenCookie); err == nil {
		return c.Value
	}
	return ""
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
