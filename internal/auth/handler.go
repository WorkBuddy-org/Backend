package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/platform/ratelimit"
	"github.com/workbuddy/backend/internal/user"
)

type Handler struct {
	service         *Service
	registerLimiter *ratelimit.FixedWindow
	cookies         CookieConfig
	origins         *httpx.Origins
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// SetRegisterLimiter limits POST /auth/register per client IP; nil (the
// default) disables the limit.
func (h *Handler) SetRegisterLimiter(l *ratelimit.FixedWindow) {
	h.registerLimiter = l
}

// SetCookieConfig sets the attributes of the auth cookies.
func (h *Handler) SetCookieConfig(c CookieConfig) {
	h.cookies = c
}

// SetAllowedOrigins sets the origins that may call POST /auth/refresh and
// POST /auth/logout, which are authenticated by the refresh cookie. With none
// set, such requests carrying an Origin header are rejected.
func (h *Handler) SetAllowedOrigins(origins *httpx.Origins) {
	h.origins = origins
}

func (h *Handler) RegisterRoutes(r chi.Router, authMiddleware *Middleware) {
	r.Get("/auth/google", h.googleLogin)
	r.Get("/auth/google/callback", h.googleCallback)
	r.Get("/auth/github", h.githubLogin)
	r.Get("/auth/github/callback", h.githubCallback)
	r.Post("/auth/register", h.register)
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
	r.Post("/auth/logout", h.logout)

	r.Group(func(r chi.Router) {
		r.Use(authMiddleware.RequireAuth)
		r.Get("/auth/me", h.me)
	})
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type meResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// authResponse is the body of register and login; the tokens travel only in
// cookies.
type authResponse struct {
	User meResponse `json:"user"`
}

func toMeResponse(u *user.User) meResponse {
	return meResponse{
		ID:    u.ID.String(),
		Email: u.Email,
		Name:  u.Name,
	}
}

func (h *Handler) googleLogin(w http.ResponseWriter, r *http.Request) {
	h.startOAuth(w, r, user.OAuthProviderGoogle)
}

func (h *Handler) googleCallback(w http.ResponseWriter, r *http.Request) {
	h.completeOAuth(w, r, user.OAuthProviderGoogle)
}

func (h *Handler) githubLogin(w http.ResponseWriter, r *http.Request) {
	h.startOAuth(w, r, user.OAuthProviderGitHub)
}

func (h *Handler) githubCallback(w http.ResponseWriter, r *http.Request) {
	h.completeOAuth(w, r, user.OAuthProviderGitHub)
}

func (h *Handler) startOAuth(w http.ResponseWriter, r *http.Request, providerName user.OAuthProvider) {
	provider, err := h.service.OAuthRegistry().Provider(providerName)
	if err != nil {
		writeProviderError(w, providerName, err)
		return
	}

	state, err := generateOAuthState()
	if err != nil {
		httpx.Internal(w, "generate oauth state failed", err)
		return
	}

	setOAuthStateCookie(w, provider, state)
	http.Redirect(w, r, provider.AuthCodeURL(state), http.StatusFound)
}

// completeOAuth always ends in a redirect to FRONTEND_CALLBACK_URL: with
// ?status=ok and the auth cookies set on success, or ?error=<CODE> on any
// failure. Tokens never appear in the URL.
func (h *Handler) completeOAuth(w http.ResponseWriter, r *http.Request, providerName user.OAuthProvider) {
	pair, code := h.oauthTokens(r, w, providerName)

	query := url.Values{}
	if code != "" {
		query.Set("error", string(code))
	} else {
		query.Set("status", "ok")
	}
	target, err := frontendRedirect(h.service.FrontendCallbackURL(), query)
	if err != nil {
		httpx.Internal(w, "build frontend redirect failed", err, "provider", providerName)
		return
	}

	if pair != nil {
		h.cookies.setAuthCookies(w, pair, h.service.RefreshTTL())
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// oauthTokens runs the provider callback and returns a token pair for the
// signed-in user, or the error code to send the frontend. Failures are logged
// here; the redirect only carries the code.
func (h *Handler) oauthTokens(r *http.Request, w http.ResponseWriter, providerName user.OAuthProvider) (*TokenPair, httpx.Code) {
	provider, err := h.service.OAuthRegistry().Provider(providerName)
	if err != nil {
		if errors.Is(err, ErrOAuthNotConfigured) {
			return nil, httpx.CodeServiceUnavailable
		}
		slog.Error("oauth provider lookup failed", "provider", providerName, "error", err)
		return nil, httpx.CodeInternal
	}

	if r.URL.Query().Get("error") != "" {
		return nil, httpx.CodeForbidden
	}

	if !validateOAuthState(r, provider, r.URL.Query().Get("state")) {
		return nil, httpx.CodeForbidden
	}
	clearOAuthStateCookie(w, provider)

	code := r.URL.Query().Get("code")
	if code == "" {
		return nil, httpx.CodeValidation
	}

	token, err := provider.Exchange(r.Context(), code)
	if err != nil {
		slog.Error("oauth token exchange failed", "provider", providerName, "error", err)
		return nil, httpx.CodeInternal
	}

	profile, err := provider.FetchProfile(r.Context(), token)
	if err != nil {
		slog.Error("oauth profile fetch failed", "provider", providerName, "error", err)
		return nil, httpx.CodeInternal
	}

	u, err := h.service.AuthenticateOAuthUser(r.Context(), profile)
	if err != nil {
		if errors.Is(err, ErrDuplicateEmail) {
			return nil, httpx.CodeEmailExists
		}
		slog.Error("oauth user lookup failed", "provider", providerName, "error", err)
		return nil, httpx.CodeInternal
	}

	pair, err := h.service.IssueTokenPair(r.Context(), u)
	if err != nil {
		slog.Error("issue token pair failed", "provider", providerName, "error", err)
		return nil, httpx.CodeInternal
	}
	return pair, ""
}

// writeProviderError answers an OAuth request for a provider that cannot be
// used: 503 when it is not configured, 500 otherwise.
func writeProviderError(w http.ResponseWriter, providerName user.OAuthProvider, err error) {
	if errors.Is(err, ErrOAuthNotConfigured) {
		httpx.Error(w, http.StatusServiceUnavailable, httpx.CodeServiceUnavailable, err.Error())
		return
	}
	httpx.Internal(w, "oauth provider lookup failed", err, "provider", providerName)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	if !h.allowRegister(w, r) {
		return
	}

	var req registerRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}

	u, err := h.service.RegisterUser(r.Context(), req.Email, req.Password, req.Name)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidEmail):
			httpx.Validation(w, err.Error(), httpx.Fields{"email": httpx.FieldInvalidFormat})
		case errors.Is(err, ErrWeakPassword):
			httpx.Validation(w, err.Error(), httpx.Fields{"password": httpx.FieldTooShort})
		case errors.Is(err, ErrDuplicateEmail):
			httpx.Error(w, http.StatusConflict, httpx.CodeEmailExists, err.Error())
		default:
			httpx.Internal(w, "register failed", err)
		}
		return
	}

	h.signIn(w, r, u, http.StatusCreated)
}

// allowRegister counts a registration request for the client IP and writes
// a 429 with Retry-After when over the limit. Every request counts, valid or
// not. If Redis fails the request is let through, like the login limiter.
func (h *Handler) allowRegister(w http.ResponseWriter, r *http.Request) bool {
	if h.registerLimiter == nil {
		return true
	}
	allowed, retryAfter, err := h.registerLimiter.Allow(r.Context(), registerAttemptsKey(httpx.ClientIP(r)))
	if err != nil {
		slog.Warn("register rate limiter unavailable, allowing request", "error", err)
		return true
	}
	if !allowed {
		httpx.RateLimited(w, retryAfter, ErrRegisterLimited.Error())
		return false
	}
	return true
}

func registerAttemptsKey(ip string) string {
	return "register_attempts:" + ip
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}

	u, err := h.service.AuthenticatePassword(r.Context(), req.Email, req.Password)
	if err != nil {
		var limited *RateLimitedError
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			httpx.Error(w, http.StatusUnauthorized, httpx.CodeInvalidCredentials, err.Error())
		case errors.As(err, &limited):
			httpx.RateLimited(w, limited.RetryAfter, err.Error())
		default:
			httpx.Internal(w, "login failed", err)
		}
		return
	}

	h.signIn(w, r, u, http.StatusOK)
}

// signIn issues a token pair for u, stores it in the auth cookies and answers
// {"user": ...} with status.
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request, u *user.User, status int) {
	pair, err := h.service.IssueTokenPair(r.Context(), u)
	if err != nil {
		httpx.Internal(w, "issue token pair failed", err)
		return
	}

	h.cookies.setAuthCookies(w, pair, h.service.RefreshTTL())
	httpx.JSON(w, status, authResponse{User: toMeResponse(u)})
}

// refresh rotates the refresh token from the refresh cookie and sets fresh
// cookies. Any request body is ignored.
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r, h.origins) {
		httpx.Forbidden(w, "origin not allowed")
		return
	}

	raw := refreshTokenFromCookie(r)
	if raw == "" {
		h.cookies.clearAuthCookies(w)
		httpx.Unauthorized(w, "missing refresh token")
		return
	}

	pair, err := h.service.Refresh(r.Context(), raw)
	if err != nil {
		if isRefreshRejected(err) {
			h.cookies.clearAuthCookies(w)
			httpx.Unauthorized(w, err.Error())
			return
		}
		httpx.Internal(w, "refresh failed", err)
		return
	}

	h.cookies.setAuthCookies(w, pair, h.service.RefreshTTL())
	w.WriteHeader(http.StatusNoContent)
}

// isRefreshRejected reports whether a Refresh error is the client's refresh
// token being unknown, expired or revoked, as opposed to a server failure.
func isRefreshRejected(err error) bool {
	return errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrTokenExpired) || errors.Is(err, ErrTokenRevoked)
}

// logout revokes the refresh cookie's token, if any, and clears both
// cookies. It succeeds without a cookie or with an already unknown token, so
// it is always safe to call.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r, h.origins) {
		httpx.Forbidden(w, "origin not allowed")
		return
	}

	h.cookies.clearAuthCookies(w)

	if raw := refreshTokenFromCookie(r); raw != "" {
		if err := h.service.Logout(r.Context(), raw); err != nil && !errors.Is(err, ErrInvalidToken) {
			httpx.Internal(w, "logout failed", err)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func refreshTokenFromCookie(r *http.Request) string {
	if c, err := r.Cookie(RefreshTokenCookie); err == nil {
		return c.Value
	}
	return ""
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	u, err := h.service.GetUser(r.Context(), userID)
	if err != nil {
		switch {
		case errors.Is(err, ErrUserNotFound):
			httpx.NotFound(w, err.Error())
		case errors.Is(err, ErrInvalidToken):
			httpx.Unauthorized(w, err.Error())
		default:
			httpx.Internal(w, "get user failed", err)
		}
		return
	}

	httpx.JSON(w, http.StatusOK, toMeResponse(u))
}

func setOAuthStateCookie(w http.ResponseWriter, provider *OAuthProvider, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:     provider.StateCookieName(),
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearOAuthStateCookie(w http.ResponseWriter, provider *OAuthProvider) {
	http.SetCookie(w, &http.Cookie{
		Name:     provider.StateCookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func validateOAuthState(r *http.Request, provider *OAuthProvider, state string) bool {
	if state == "" {
		return false
	}

	cookie, err := r.Cookie(provider.StateCookieName())
	if err != nil {
		return false
	}

	return cookie.Value != "" && cookie.Value == state
}

// frontendRedirect returns baseURL with query merged into its query string.
func frontendRedirect(baseURL string, query url.Values) (string, error) {
	if baseURL == "" {
		return "", fmt.Errorf("frontend callback url is empty")
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	q := u.Query()
	for k, vs := range query {
		q[k] = vs
	}
	u.RawQuery = q.Encode()

	return u.String(), nil
}
