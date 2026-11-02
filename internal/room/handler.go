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
		r.Get("/rooms/{id}/participants", h.participants)
	})
}

type roomDTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

type participationDTO struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"user_id"`
	RoomID     uuid.UUID  `json:"room_id"`
	JoinedAt   time.Time  `json:"joined_at"`
	LeftAt     *time.Time `json:"left_at,omitempty"`
	LeftReason *string    `json:"left_reason,omitempty"`
}

type participantDTO struct {
	UserID   uuid.UUID `json:"user_id"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joined_at"`
}

// Rooms are not session-based (timers are per task), so room responses carry
// no session fields.
type roomDetailResponse struct {
	Room roomDTO `json:"room"`
}

type joinResponse struct {
	Room          roomDTO          `json:"room"`
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

func toParticipationDTO(p *Participation) participationDTO {
	return participationDTO{
		ID:         p.ID,
		UserID:     p.UserID,
		RoomID:     p.RoomID,
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
		httpx.Internal(w, "list rooms failed", err)
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
		httpx.InvalidPathID(w, "room")
		return
	}

	rm, _, err := h.service.GetRoomDetail(r.Context(), id)
	if err != nil {
		writeError(w, err, "get room failed", "room_id", id)
		return
	}

	httpx.JSON(w, http.StatusOK, roomDetailResponse{Room: toRoomDTO(rm)})
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.InvalidPathID(w, "room")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	result, err := h.service.Join(r.Context(), id, userID)
	if err != nil {
		writeError(w, err, "join room failed", "room_id", id, "user_id", userID)
		return
	}

	httpx.JSON(w, http.StatusOK, joinResponse{
		Room:          toRoomDTO(result.Room),
		Participation: toParticipationDTO(result.Participation),
	})
}

func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.InvalidPathID(w, "room")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	if err := h.service.Leave(r.Context(), id, userID); err != nil {
		writeError(w, err, "leave room failed", "room_id", id, "user_id", userID)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) participants(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.InvalidPathID(w, "room")
		return
	}

	userID, err := auth.CurrentUserID(r)
	if err != nil {
		httpx.Unauthorized(w, "missing user context")
		return
	}

	participants, err := h.service.ListParticipants(r.Context(), id, userID)
	if err != nil {
		writeError(w, err, "list participants failed", "room_id", id, "user_id", userID)
		return
	}

	dtos := make([]participantDTO, len(participants))
	for i, p := range participants {
		dtos[i] = participantDTO(p)
	}
	httpx.JSON(w, http.StatusOK, dtos)
}

// writeError maps room errors to codes; anything unknown is logged with args
// and answered with a generic 500.
func writeError(w http.ResponseWriter, err error, logMsg string, args ...any) {
	switch {
	case errors.Is(err, ErrRoomNotFound), errors.Is(err, ErrNotInRoom):
		httpx.NotFound(w, err.Error())
	case errors.Is(err, ErrNotMember):
		httpx.Forbidden(w, err.Error())
	default:
		httpx.Internal(w, logMsg, err, args...)
	}
}
