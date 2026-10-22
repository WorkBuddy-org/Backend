package chat

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware.RequireAuth)
		r.Post("/rooms/{id}/messages", h.send)
		r.Get("/rooms/{id}/messages", h.list)
	})
}

type messageDTO struct {
	ID        uuid.UUID `json:"id"`
	RoomID    uuid.UUID `json:"room_id"`
	UserID    uuid.UUID `json:"user_id"`
	UserName  string    `json:"user_name"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

func toMessageDTO(m *Message) messageDTO {
	return messageDTO{
		ID:        m.ID,
		RoomID:    m.RoomID,
		UserID:    m.UserID,
		UserName:  m.UserName,
		Text:      m.Text,
		CreatedAt: m.CreatedAt,
	}
}

type pageDTO struct {
	Items      []messageDTO `json:"items"`
	NextCursor *uuid.UUID   `json:"next_cursor"`
}

type sendRequest struct {
	Text string `json:"text"`
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
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

	var req sendRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	m, err := h.service.Send(r.Context(), roomID, userID, req.Text)
	if err != nil {
		writeError(w, err)
		return
	}

	httpx.JSON(w, http.StatusCreated, toMessageDTO(m))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
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

	query := r.URL.Query()

	var before *uuid.UUID
	if raw := query.Get("before"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, ErrInvalidCursor.Error())
			return
		}
		before = &id
	}

	// Limits above the maximum are capped rather than rejected.
	limit := DefaultPageSize
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpx.Error(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, MaxPageSize)
	}

	page, err := h.service.List(r.Context(), roomID, userID, before, limit)
	if err != nil {
		writeError(w, err)
		return
	}

	dto := pageDTO{Items: make([]messageDTO, len(page.Items)), NextCursor: page.NextCursor}
	for i := range page.Items {
		dto.Items[i] = toMessageDTO(&page.Items[i])
	}
	httpx.JSON(w, http.StatusOK, dto)
}

// writeError maps chat errors to statuses; anything unknown is a 500 without
// internal details.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTextRequired), errors.Is(err, ErrTextTooLong), errors.Is(err, ErrInvalidCursor):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotInRoom):
		httpx.Error(w, http.StatusForbidden, err.Error())
	default:
		slog.Error("chat request failed", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

func decodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()

	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}
