package profile

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/user"
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
		r.Get("/profile", h.get)
		r.Patch("/profile", h.update)
	})
}

type profileDTO struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	Role         *string   `json:"role,omitempty"`
	Stack        *string   `json:"stack,omitempty"`
	Timezone     *string   `json:"timezone,omitempty"`
	AvatarStatus string    `json:"avatar_status"`
	CreatedAt    time.Time `json:"created_at"`
}

func toProfileDTO(u *user.User) profileDTO {
	return profileDTO{
		ID:           u.ID,
		Email:        u.Email,
		Name:         u.Name,
		Role:         u.Role,
		Stack:        u.Stack,
		Timezone:     u.Timezone,
		AvatarStatus: u.AvatarStatus,
		CreatedAt:    u.CreatedAt,
	}
}

type updateRequest struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Stack    *string `json:"stack"`
	Timezone *string `json:"timezone"`
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	u, err := h.service.Get(r.Context(), userID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrUserNotFound) {
			status = http.StatusNotFound
		}
		httpx.Error(w, status, err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, toProfileDTO(u))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	var req updateRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	u, err := h.service.Update(r.Context(), userID, Update{
		Name:     req.Name,
		Role:     req.Role,
		Stack:    req.Stack,
		Timezone: req.Timezone,
	})
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrNoFields):
			status = http.StatusBadRequest
		case errors.Is(err, ErrUserNotFound):
			status = http.StatusNotFound
		}
		httpx.Error(w, status, err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, toProfileDTO(u))
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()

	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}
