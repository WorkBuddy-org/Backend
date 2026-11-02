package profile

import (
	"errors"
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
		r.Get("/me", h.me)
		r.Get("/me/onboarding", h.getOnboarding)
		r.Patch("/me/onboarding", h.updateOnboarding)
		r.Post("/me/onboarding/skip", h.skipOnboarding)
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
		httpx.Unauthorized(w, "missing user context")
		return
	}

	u, err := h.service.Get(r.Context(), userID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			httpx.NotFound(w, err.Error())
			return
		}
		httpx.Internal(w, "get profile failed", err, "user_id", userID)
		return
	}

	httpx.JSON(w, http.StatusOK, toProfileDTO(u))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	var req updateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}

	u, err := h.service.Update(r.Context(), userID, Update(req))
	if err != nil {
		if fields, ok := validationFields(err); ok {
			httpx.Validation(w, err.Error(), fields)
			return
		}
		if errors.Is(err, ErrUserNotFound) {
			httpx.NotFound(w, err.Error())
			return
		}
		httpx.Internal(w, "update profile failed", err, "user_id", userID)
		return
	}

	httpx.JSON(w, http.StatusOK, toProfileDTO(u))
}

// validationFields maps a profile validation error to its per-field reason;
// ok is false for errors that are not validation errors.
func validationFields(err error) (httpx.Fields, bool) {
	switch {
	case errors.Is(err, ErrNoFields):
		return nil, true
	case errors.Is(err, ErrInvalidName):
		return httpx.Fields{"name": httpx.FieldInvalidLength}, true
	case errors.Is(err, ErrRoleTooLong):
		return httpx.Fields{"role": httpx.FieldTooLong}, true
	case errors.Is(err, ErrStackTooLong):
		return httpx.Fields{"stack": httpx.FieldTooLong}, true
	case errors.Is(err, ErrInvalidTimezone):
		return httpx.Fields{"timezone": httpx.FieldInvalidValue}, true
	}
	return nil, false
}

type onboardingDTO struct {
	Goal                *string `json:"goal"`
	Experience          *string `json:"experience"`
	Profession          *string `json:"profession"`
	ProfessionOther     *string `json:"profession_other"`
	OnboardingCompleted bool    `json:"onboarding_completed"`
}

func toOnboardingDTO(u *user.User) onboardingDTO {
	return onboardingDTO{
		Goal:                u.Goal,
		Experience:          u.Experience,
		Profession:          u.Profession,
		ProfessionOther:     u.ProfessionOther,
		OnboardingCompleted: u.OnboardingCompleted,
	}
}

type onboardingRequest struct {
	Goal            *string `json:"goal"`
	Experience      *string `json:"experience"`
	Profession      *string `json:"profession"`
	ProfessionOther *string `json:"profession_other"`
}

// currentUser loads the signed-in user, writing the error response itself.
func (h *Handler) currentUser(w http.ResponseWriter, r *http.Request) (*user.User, bool) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return nil, false
	}
	u, err := h.service.Get(r.Context(), userID)
	if err != nil {
		writeUserError(w, err, userID)
		return nil, false
	}
	return u, true
}

func writeUserError(w http.ResponseWriter, err error, userID uuid.UUID) {
	switch {
	case httpx.WriteValidationError(w, err):
	case errors.Is(err, ErrNoFields):
		httpx.Validation(w, err.Error(), nil)
	case errors.Is(err, ErrUserNotFound):
		httpx.NotFound(w, err.Error())
	default:
		httpx.Internal(w, "user request failed", err, "user_id", userID)
	}
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	if u, ok := h.currentUser(w, r); ok {
		httpx.JSON(w, http.StatusOK, user.ToDTO(u))
	}
}

func (h *Handler) getOnboarding(w http.ResponseWriter, r *http.Request) {
	if u, ok := h.currentUser(w, r); ok {
		httpx.JSON(w, http.StatusOK, toOnboardingDTO(u))
	}
}

func (h *Handler) updateOnboarding(w http.ResponseWriter, r *http.Request) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	var req onboardingRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}

	u, err := h.service.UpdateOnboarding(r.Context(), userID, OnboardingInput(req))
	if err != nil {
		writeUserError(w, err, userID)
		return
	}
	httpx.JSON(w, http.StatusOK, toOnboardingDTO(u))
}

// skipOnboarding takes no body.
func (h *Handler) skipOnboarding(w http.ResponseWriter, r *http.Request) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	u, err := h.service.SkipOnboarding(r.Context(), userID)
	if err != nil {
		writeUserError(w, err, userID)
		return
	}
	httpx.JSON(w, http.StatusOK, toOnboardingDTO(u))
}
