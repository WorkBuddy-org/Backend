package task

import (
	"errors"
	"net/http"
	"strconv"
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
		httpx.InvalidPathID(w, "room")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	var req createRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}

	t, err := h.service.Create(r.Context(), roomID, userID, req.Text)
	if err != nil {
		writeError(w, err)
		return
	}

	httpx.JSON(w, http.StatusCreated, toTaskDTO(t))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.InvalidPathID(w, "task")
		return
	}

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

	t, err := h.service.Update(r.Context(), taskID, userID, Update(req))
	if err != nil {
		writeError(w, err)
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
		httpx.InvalidPathID(w, "task")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	var req startTimerRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}

	t, err := h.service.StartTimer(r.Context(), taskID, userID, req.DurationMinutes)
	if err != nil {
		writeError(w, err)
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
		httpx.InvalidPathID(w, "task")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	var req extendTimerRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.DecodeError(w, err)
		return
	}

	t, err := h.service.ExtendTimer(r.Context(), taskID, userID, req.Minutes)
	if err != nil {
		writeError(w, err)
		return
	}

	httpx.JSON(w, http.StatusOK, toTaskDTO(t))
}

// declineExtension takes no body.
func (h *Handler) declineExtension(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.InvalidPathID(w, "task")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	t, err := h.service.DeclineExtension(r.Context(), taskID, userID)
	if err != nil {
		writeError(w, err)
		return
	}

	httpx.JSON(w, http.StatusOK, toTaskDTO(t))
}

// writeError maps task and timer errors to codes; anything unknown is a 500
// without internal details.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTextRequired):
		httpx.Validation(w, err.Error(), httpx.Fields{"text": httpx.FieldRequired})
	case errors.Is(err, ErrTextTooLong):
		httpx.Validation(w, err.Error(), httpx.Fields{"text": httpx.FieldTooLong})
	case errors.Is(err, ErrNoFields):
		httpx.Validation(w, err.Error(), nil)
	case errors.Is(err, ErrInvalidDuration):
		httpx.Validation(w, err.Error(), httpx.Fields{"duration_minutes": httpx.FieldInvalidValue})
	case errors.Is(err, ErrInvalidExtension):
		httpx.Validation(w, err.Error(), httpx.Fields{"minutes": httpx.FieldInvalidValue})
	case errors.Is(err, ErrTaskNotFound), errors.Is(err, room.ErrRoomNotFound):
		httpx.NotFound(w, err.Error())
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrNotInRoom):
		httpx.Forbidden(w, err.Error())
	case errors.Is(err, ErrExtensionLimit):
		httpx.Error(w, http.StatusConflict, httpx.CodeExtendLimitReached, err.Error())
	case errors.Is(err, ErrTaskCompleted), errors.Is(err, ErrTimerActive),
		errors.Is(err, ErrNotPrompting), errors.Is(err, ErrPromptExpired):
		httpx.Error(w, http.StatusConflict, httpx.CodeConflict, err.Error())
	default:
		httpx.Internal(w, "task request failed", err)
	}
}

func (h *Handler) listMine(w http.ResponseWriter, r *http.Request) {
	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	limit, offset, ok := parsePage(w, r)
	if !ok {
		return
	}

	tasks, err := h.service.ListByUser(r.Context(), userID, limit, offset)
	if err != nil {
		httpx.Internal(w, "list tasks failed", err, "user_id", userID)
		return
	}

	dtos := make([]taskDTO, len(tasks))
	for i := range tasks {
		dtos[i] = toTaskDTO(&tasks[i])
	}
	httpx.JSON(w, http.StatusOK, dtos)
}

// Page sizes for GET /tasks/me.
const (
	DefaultListLimit = 50
	MaxListLimit     = 100
)

// parsePage reads ?limit= and ?offset=. Limits above the maximum are capped
// rather than rejected, like chat history. It writes the 400 itself and
// returns ok=false on bad input.
func parsePage(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	query := r.URL.Query()

	limit = DefaultListLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpx.Validation(w, "limit must be a positive integer", httpx.Fields{"limit": httpx.FieldInvalidValue})
			return 0, 0, false
		}
		limit = min(n, MaxListLimit)
	}

	if raw := query.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			httpx.Validation(w, "offset must be a non-negative integer", httpx.Fields{"offset": httpx.FieldInvalidValue})
			return 0, 0, false
		}
		offset = n
	}

	return limit, offset, true
}
