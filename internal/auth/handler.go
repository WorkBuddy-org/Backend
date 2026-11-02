package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/platform/ratelimit"
	"github.com/workbuddy/backend/internal/user"
)

type Handler struct {
	service         *Service
	registerLimiter *ratelimit.FixedWindow
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// SetRegisterLimiter limits POST /auth/register per client IP; nil (the
// default) disables the limit.
func (h *Handler) SetRegisterLimiter(l *ratelimit.FixedWindow) {
	h.registerLimiter = l
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

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type meResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
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

func (h *Handler) completeOAuth(w http.ResponseWriter, r *http.Request, providerName user.OAuthProvider) {
	provider, err := h.service.OAuthRegistry().Provider(providerName)
	if err != nil {
		writeProviderError(w, providerName, err)
		return
	}

	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		httpx.Forbidden(w, "oauth authorization denied")
		return
	}

	state := r.URL.Query().Get("state")
	if !validateOAuthState(r, provider, state) {
		httpx.Forbidden(w, ErrInvalidOAuthState.Error())
		return
	}
	clearOAuthStateCookie(w, provider)

	code := r.URL.Query().Get("code")
	if code == "" {
		httpx.Validation(w, "missing authorization code", httpx.Fields{"code": httpx.FieldRequired})
		return
	}

	token, err := provider.Exchange(r.Context(), code)
	if err != nil {
		slog.Error("oauth token exchange failed", "provider", providerName, "error", err)
		httpx.Error(w, http.StatusBadGateway, httpx.CodeInternal, "oauth token exchange failed")
		return
	}

	profile, err := provider.FetchProfile(r.Context(), token)
	if err != nil {
		slog.Error("oauth profile fetch failed", "provider", providerName, "error", err)
		httpx.Error(w, http.StatusBadGateway, httpx.CodeInternal, "failed to fetch oauth profile")
		return
	}

	u, err := h.service.AuthenticateOAuthUser(r.Context(), profile)
	if err != nil {
		if errors.Is(err, ErrDuplicateEmail) {
			httpx.Error(w, http.StatusConflict, httpx.CodeEmailExists, err.Error())
			return
		}
		httpx.Internal(w, "oauth user lookup failed", err, "provider", providerName)
		return
	}

	pair, err := h.service.IssueTokenPair(r.Context(), u)
	if err != nil {
		httpx.Internal(w, "issue token pair failed", err)
		return
	}

	if wantsJSON(r) {
		httpx.JSON(w, http.StatusOK, pair)
		return
	}

	redirectURL, err := buildFrontendRedirect(h.service.FrontendCallbackURL(), pair)
	if err != nil {
		httpx.Internal(w, "build frontend redirect failed", err)
		return
	}

	http.Redirect(w, r, redirectURL, http.StatusFound)
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

	pair, err := h.service.IssueTokenPair(r.Context(), u)
	if err != nil {
		httpx.Internal(w, "issue token pair failed", err)
		return
	}

	httpx.JSON(w, http.StatusCreated, pair)
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

	pair, err := h.service.IssueTokenPair(r.Context(), u)
	if err != nil {
		httpx.Internal(w, "issue token pair failed", err)
		return
	}

	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}
	if req.RefreshToken == "" {
		httpx.Validation(w, "refresh_token is required", httpx.Fields{"refresh_token": httpx.FieldRequired})
		return
	}

	pair, err := h.service.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		if isRefreshRejected(err) {
			httpx.Unauthorized(w, err.Error())
			return
		}
		httpx.Internal(w, "refresh failed", err)
		return
	}

	httpx.JSON(w, http.StatusOK, pair)
}

// isRefreshRejected reports whether a Refresh error is the client's refresh
// token being unknown, expired or revoked, as opposed to a server failure.
func isRefreshRejected(err error) bool {
	return errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrTokenExpired) || errors.Is(err, ErrTokenRevoked)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}
	if req.RefreshToken == "" {
		httpx.Validation(w, "refresh_token is required", httpx.Fields{"refresh_token": httpx.FieldRequired})
		return
	}

	if err := h.service.Logout(r.Context(), req.RefreshToken); err != nil {
		if errors.Is(err, ErrInvalidToken) {
			httpx.Unauthorized(w, err.Error())
			return
		}
		httpx.Internal(w, "logout failed", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
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

	httpx.JSON(w, http.StatusOK, meResponse{
		ID:    u.ID.String(),
		Email: u.Email,
		Name:  u.Name,
	})
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

func buildFrontendRedirect(baseURL string, pair *TokenPair) (string, error) {
	if baseURL == "" {
		return "", fmt.Errorf("frontend callback url is empty")
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	q := u.Query()
	q.Set("access_token", pair.AccessToken)
	q.Set("refresh_token", pair.RefreshToken)
	q.Set("expires_in", strconv.FormatInt(pair.ExpiresIn, 10))
	u.RawQuery = q.Encode()

	return u.String(), nil
}

func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json")
}
