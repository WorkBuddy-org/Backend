package task

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/room"
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
		r.Post("/rooms/{id}/tasks", h.create)
		r.Patch("/tasks/{id}", h.update)
		r.Post("/tasks/{id}/start", h.startTimer)
		r.Post("/tasks/{id}/extend", h.extendTimer)
		r.Post("/tasks/{id}/no-extend", h.declineExtension)
		r.Get("/tasks/me", h.listMine)
	})
}

type taskDTO struct {
	ID               uuid.UUID  `json:"id"`
	RoomID           uuid.UUID  `json:"room_id"`
	SessionID        *uuid.UUID `json:"session_id,omitempty"`
	Text             string     `json:"text"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	ExtendedCount    int        `json:"extended_count"`
	ExtraMinutesUsed int        `json:"extra_minutes_used"`

	TimerStatus          string     `json:"timer_status"`
	TimerDurationMinutes *int       `json:"timer_duration_minutes,omitempty"`
	TimerStartedAt       *time.Time `json:"timer_started_at,omitempty"`
	TimerEndsAt          *time.Time `json:"timer_ends_at,omitempty"`
	PromptDeadlineAt     *time.Time `json:"prompt_deadline_at,omitempty"`
}

func toTaskDTO(t *Task) taskDTO {
	return taskDTO{
		ID:               t.ID,
		RoomID:           t.RoomID,
		SessionID:        t.SessionID,
		Text:             t.Text,
		CompletedAt:      t.CompletedAt,
		CreatedAt:        t.CreatedAt,
		UpdatedAt:        t.UpdatedAt,
		ExtendedCount:    t.ExtendedCount,
		ExtraMinutesUsed: t.ExtraMinutesUsed,

		TimerStatus:          t.TimerStatus,
		TimerDurationMinutes: t.TimerDurationMinutes,
		TimerStartedAt:       t.TimerStartedAt,
		TimerEndsAt:          t.TimerEndsAt,
		PromptDeadlineAt:     t.PromptDeadlineAt,
	}
}

type createRequest struct {
	Text string `json:"text"`
}

type updateRequest struct {
	Text      *string `json:"text"`
	Completed *bool   `json:"completed"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
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

	var req createRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	t, err := h.service.Create(r.Context(), roomID, userID, req.Text)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrTextRequired):
			status = http.StatusBadRequest
		case errors.Is(err, room.ErrRoomNotFound):
			status = http.StatusNotFound
		}
		httpx.Error(w, status, err.Error())
		return
	}

	httpx.JSON(w, http.StatusCreated, toTaskDTO(t))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid task id")
		return
	}

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

	t, err := h.service.Update(r.Context(), taskID, userID, Update(req))
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrTaskNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrForbidden):
			status = http.StatusForbidden
		case errors.Is(err, ErrTextRequired):
			status = http.StatusBadRequest
		}
		httpx.Error(w, status, err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, toTaskDTO(t))
}

type startTimerRequest struct {
	DurationMinutes int `json:"duration_minutes"`
}

func (h *Handler) startTimer(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid task id")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	var req startTimerRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	t, err := h.service.StartTimer(r.Context(), taskID, userID, req.DurationMinutes)
	if err != nil {
		writeTimerError(w, err)
		return
	}

	httpx.JSON(w, http.StatusOK, toTaskDTO(t))
}

type extendTimerRequest struct {
	Minutes int `json:"minutes"`
}

func (h *Handler) extendTimer(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid task id")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	var req extendTimerRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	t, err := h.service.ExtendTimer(r.Context(), taskID, userID, req.Minutes)
	if err != nil {
		writeTimerError(w, err)
		return
	}

	httpx.JSON(w, http.StatusOK, toTaskDTO(t))
}

// declineExtension takes no body.
func (h *Handler) declineExtension(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid task id")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	t, err := h.service.DeclineExtension(r.Context(), taskID, userID)
	if err != nil {
		writeTimerError(w, err)
		return
	}

	httpx.JSON(w, http.StatusOK, toTaskDTO(t))
}

// writeTimerError maps timer errors to statuses; anything unknown is a 500
// without internal details.
func writeTimerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidDuration), errors.Is(err, ErrInvalidExtension):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrTaskNotFound):
		httpx.Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrTaskCompleted), errors.Is(err, ErrNotInRoom), errors.Is(err, ErrTimerActive),
		errors.Is(err, ErrNotPrompting), errors.Is(err, ErrPromptExpired), errors.Is(err, ErrExtensionLimit):
		httpx.Error(w, http.StatusConflict, err.Error())
	default:
		slog.Error("task timer request failed", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	tasks, err := h.service.ListByUser(r.Context(), userID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to list tasks")
		return
	}

	dtos := make([]taskDTO, len(tasks))
	for i := range tasks {
		dtos[i] = toTaskDTO(&tasks[i])
	}
	httpx.JSON(w, http.StatusOK, dtos)
}

func decodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()

	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}
