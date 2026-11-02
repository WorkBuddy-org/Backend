package call

import (
	"errors"
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
		httpx.InvalidPathID(w, "room")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	tok, err := h.service.IssueToken(r.Context(), roomID, userID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotConfigured):
			httpx.Error(w, http.StatusServiceUnavailable, httpx.CodeServiceUnavailable, err.Error())
		case errors.Is(err, ErrNotInRoom):
			httpx.Forbidden(w, err.Error())
		default:
			httpx.Internal(w, "issue call token failed", err, "room_id", roomID, "user_id", userID)
		}
		return
	}

	httpx.JSON(w, http.StatusOK, tokenDTO{URL: tok.URL, Token: tok.Token})
}
