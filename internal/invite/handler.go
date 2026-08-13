package invite

import (
	"net/http"
	"time"

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
	r.Get("/invites/{code}", h.checkValidity)

	r.Group(func(r chi.Router) {
		r.Use(authMiddleware.RequireAuth)
		r.Post("/invites", h.create)
	})
}

type inviteDTO struct {
	ID        uuid.UUID `json:"id"`
	Code      string    `json:"code"`
	CreatedBy uuid.UUID `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type validityResponse struct {
	Valid  bool   `json:"valid"`
	Reason string `json:"reason,omitempty"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	inv, err := h.service.Create(r.Context(), userID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to create invite")
		return
	}

	httpx.JSON(w, http.StatusCreated, inviteDTO{
		ID:        inv.ID,
		Code:      inv.Code,
		CreatedBy: inv.CreatedBy,
		CreatedAt: inv.CreatedAt,
	})
}

func (h *Handler) checkValidity(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		httpx.Error(w, http.StatusBadRequest, "invite code is required")
		return
	}

	result, err := h.service.CheckValidity(r.Context(), code)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to check invite")
		return
	}

	httpx.JSON(w, http.StatusOK, validityResponse{Valid: result.Valid, Reason: result.Reason})
}
