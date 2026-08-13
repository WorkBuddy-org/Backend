package room

import (
	"errors"
	"net/http"
	"strings"
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
		r.Get("/rooms", h.list)
		r.Get("/rooms/{id}", h.get)
		r.Post("/rooms/{id}/join", h.join)
		r.Post("/rooms/{id}/leave", h.leave)
	})
}

type roomDTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

type sessionDTO struct {
	ID        uuid.UUID  `json:"id"`
	RoomID    uuid.UUID  `json:"room_id"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Status    string     `json:"status"`
}

type participationDTO struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"user_id"`
	RoomID     uuid.UUID  `json:"room_id"`
	SessionID  *uuid.UUID `json:"session_id,omitempty"`
	JoinedAt   time.Time  `json:"joined_at"`
	LeftAt     *time.Time `json:"left_at,omitempty"`
	LeftReason *string    `json:"left_reason,omitempty"`
}

type roomDetailResponse struct {
	Room          roomDTO     `json:"room"`
	ActiveSession *sessionDTO `json:"active_session,omitempty"`
}

type joinResponse struct {
	Room          roomDTO          `json:"room"`
	ActiveSession *sessionDTO      `json:"active_session,omitempty"`
	Participation participationDTO `json:"participation"`
}

func toRoomDTO(rm *Room) roomDTO {
	return roomDTO{
		ID:        rm.ID,
		Name:      rm.Name,
		Type:      rm.Type,
		Tags:      rm.Tags,
		CreatedAt: rm.CreatedAt,
	}
}

func toSessionDTO(s *Session) *sessionDTO {
	if s == nil {
		return nil
	}
	return &sessionDTO{
		ID:        s.ID,
		RoomID:    s.RoomID,
		StartedAt: s.StartedAt,
		EndedAt:   s.EndedAt,
		Status:    s.Status,
	}
}

func toParticipationDTO(p *Participation) participationDTO {
	return participationDTO{
		ID:         p.ID,
		UserID:     p.UserID,
		RoomID:     p.RoomID,
		SessionID:  p.SessionID,
		JoinedAt:   p.JoinedAt,
		LeftAt:     p.LeftAt,
		LeftReason: p.LeftReason,
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	filter := Filter{
		Type: r.URL.Query().Get("type"),
	}
	if tags := r.URL.Query().Get("tags"); tags != "" {
		filter.Tags = strings.Split(tags, ",")
	}

	rooms, err := h.service.ListRooms(r.Context(), filter)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "failed to list rooms")
		return
	}

	dtos := make([]roomDTO, len(rooms))
	for i := range rooms {
		dtos[i] = toRoomDTO(&rooms[i])
	}
	httpx.JSON(w, http.StatusOK, dtos)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid room id")
		return
	}

	rm, session, err := h.service.GetRoomDetail(r.Context(), id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrRoomNotFound) {
			status = http.StatusNotFound
		}
		httpx.Error(w, status, err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, roomDetailResponse{
		Room:          toRoomDTO(rm),
		ActiveSession: toSessionDTO(session),
	})
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid room id")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	result, err := h.service.Join(r.Context(), id, userID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrRoomNotFound) {
			status = http.StatusNotFound
		}
		httpx.Error(w, status, err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, joinResponse{
		Room:          toRoomDTO(result.Room),
		ActiveSession: toSessionDTO(result.ActiveSession),
		Participation: toParticipationDTO(result.Participation),
	})
}

func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid room id")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Error(w, http.StatusUnauthorized, "missing user context")
		return
	}

	if err := h.service.Leave(r.Context(), id, userID); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrNotInRoom) {
			status = http.StatusNotFound
		}
		httpx.Error(w, status, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
