package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/user"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
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
		status := http.StatusInternalServerError
		if errors.Is(err, ErrOAuthNotConfigured) {
			status = http.StatusServiceUnavailable
		}
		httpx.Error(w, status, err.Error())
		return
	}

	state, err := generateOAuthState()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to start oauth flow")
		return
	}

	setOAuthStateCookie(w, provider, state)
	http.Redirect(w, r, provider.AuthCodeURL(state), http.StatusFound)
}

func (h *Handler) completeOAuth(w http.ResponseWriter, r *http.Request, providerName user.OAuthProvider) {
	provider, err := h.service.OAuthRegistry().Provider(providerName)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrOAuthNotConfigured) {
			status = http.StatusServiceUnavailable
		}
		httpx.Error(w, status, err.Error())
		return
	}

	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		httpx.Error(w, http.StatusBadRequest, "oauth authorization denied")
		return
	}

	state := r.URL.Query().Get("state")
	if !validateOAuthState(r, provider, state) {
		httpx.Error(w, http.StatusBadRequest, ErrInvalidOAuthState.Error())
		return
	}
	clearOAuthStateCookie(w, provider)

	code := r.URL.Query().Get("code")
	if code == "" {
		httpx.Error(w, http.StatusBadRequest, "missing authorization code")
		return
	}

	token, err := provider.Exchange(r.Context(), code)
	if err != nil {
		slog.Error("oauth token exchange failed", "provider", providerName, "error", err)
		httpx.Error(w, http.StatusBadGateway, "oauth token exchange failed")
		return
	}

	profile, err := provider.FetchProfile(r.Context(), token)
	if err != nil {
		slog.Error("oauth profile fetch failed", "provider", providerName, "error", err)
		httpx.Error(w, http.StatusBadGateway, "failed to fetch oauth profile")
		return
	}

	u, err := h.service.AuthenticateOAuthUser(r.Context(), profile)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrDuplicateEmail) {
			status = http.StatusConflict
		}
		httpx.Error(w, status, err.Error())
		return
	}

	pair, err := h.service.IssueTokenPair(r.Context(), u)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to issue tokens")
		return
	}

	if wantsJSON(r) {
		httpx.JSON(w, http.StatusOK, pair)
		return
	}

	redirectURL, err := buildFrontendRedirect(h.service.FrontendCallbackURL(), pair)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "invalid frontend callback url")
		return
	}

	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	u, err := h.service.RegisterUser(r.Context(), req.Email, req.Password, req.Name)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrInvalidEmail), errors.Is(err, ErrWeakPassword):
			status = http.StatusBadRequest
		case errors.Is(err, ErrDuplicateEmail):
			status = http.StatusConflict
		}
		httpx.Error(w, status, err.Error())
		return
	}

	pair, err := h.service.IssueTokenPair(r.Context(), u)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to issue tokens")
		return
	}

	httpx.JSON(w, http.StatusCreated, pair)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	u, err := h.service.AuthenticatePassword(r.Context(), req.Email, req.Password)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			status = http.StatusUnauthorized
		case errors.Is(err, ErrRateLimited):
			status = http.StatusTooManyRequests
		}
		httpx.Error(w, status, err.Error())
		return
	}

	pair, err := h.service.IssueTokenPair(r.Context(), u)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to issue tokens")
		return
	}

	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RefreshToken == "" {
		httpx.Error(w, http.StatusBadRequest, "refresh_token is required")
		return
	}

	pair, err := h.service.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		status := http.StatusUnauthorized
		if !errors.Is(err, ErrInvalidToken) && !errors.Is(err, ErrTokenExpired) && !errors.Is(err, ErrTokenRevoked) {
			status = http.StatusInternalServerError
		}
		httpx.Error(w, status, err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RefreshToken == "" {
		httpx.Error(w, http.StatusBadRequest, "refresh_token is required")
		return
	}

	if err := h.service.Logout(r.Context(), req.RefreshToken); err != nil {
		status := http.StatusUnauthorized
		if !errors.Is(err, ErrInvalidToken) {
			status = http.StatusInternalServerError
		}
		httpx.Error(w, status, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	u, err := h.service.GetUser(r.Context(), userID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrUserNotFound) {
			status = http.StatusNotFound
		}
		httpx.Error(w, status, err.Error())
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

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()

	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	return nil
}
