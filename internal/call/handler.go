package call

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/platform/httpx"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterRoutes(r chi.Router, authMiddleware *auth.Middleware) {
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware.RequireAuth)
		r.Post("/rooms/{id}/call/token", h.issueToken)
	})
}

type tokenDTO struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// issueToken takes no request body; any body sent is ignored.
func (h *Handler) issueToken(w http.ResponseWriter, r *http.Request) {
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid room id")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	tok, err := h.service.IssueToken(r.Context(), roomID, userID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotConfigured):
			httpx.Error(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, ErrNotInRoom):
			httpx.Error(w, http.StatusForbidden, err.Error())
		default:
			slog.Error("issue call token failed", "room_id", roomID, "user_id", userID, "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	httpx.JSON(w, http.StatusOK, tokenDTO{URL: tok.URL, Token: tok.Token})
}
