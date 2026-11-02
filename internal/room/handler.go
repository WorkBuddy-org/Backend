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

type participantPreviewDTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	AvatarURL *string   `json:"avatar_url"`
}

type roomDTO struct {
	ID                uuid.UUID               `json:"id"`
	Title             string                  `json:"title"`
	Type              string                  `json:"type"`
	Status            string                  `json:"status"`
	Tags              []string                `json:"tags"`
	Participants      []participantPreviewDTO `json:"participants"`
	ParticipantsCount int                     `json:"participants_count"`
	Capacity          int                     `json:"capacity"`
	StartsAt          *time.Time              `json:"starts_at"`
	EndsAt            *time.Time              `json:"ends_at"`
	// Reservations don't exist yet, so this is always false.
	ReservedByMe bool `json:"reserved_by_me"`
}

type roomListResponse struct {
	Items []roomDTO `json:"items"`
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

type joinResponse struct {
	Room          roomDTO          `json:"room"`
	Participation participationDTO `json:"participation"`
}

func toRoomDTO(v *View) roomDTO {
	participants := make([]participantPreviewDTO, len(v.Participants))
	for i, p := range v.Participants {
		participants[i] = participantPreviewDTO{ID: p.UserID, Name: p.Name, AvatarURL: p.AvatarURL}
	}
	return roomDTO{
		ID:                v.ID,
		Title:             v.Name,
		Type:              v.Type,
		Status:            v.Status,
		Tags:              v.Tags,
		Participants:      participants,
		ParticipantsCount: v.ParticipantsCount,
		Capacity:          v.Capacity,
		StartsAt:          v.StartsAt,
		EndsAt:            v.EndsAt,
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
	var filter Filter
	switch t := r.URL.Query().Get("type"); t {
	case "", "all":
	case TypeFocus, TypeDiscussion:
		filter.Type = t
	default:
		httpx.Validation(w, "type must be one of all, focus, discussion", httpx.Fields{"type": httpx.FieldInvalidValue})
		return
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
	httpx.JSON(w, http.StatusOK, roomListResponse{Items: dtos})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.InvalidPathID(w, "room")
		return
	}

	view, err := h.service.GetRoom(r.Context(), id)
	if err != nil {
		writeError(w, err, "get room failed", "room_id", id)
		return
	}

	httpx.JSON(w, http.StatusOK, toRoomDTO(view))
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
	case errors.Is(err, ErrRoomFull):
		httpx.Error(w, http.StatusConflict, httpx.CodeRoomFull, err.Error())
	case errors.Is(err, ErrRoomEnded):
		httpx.Error(w, http.StatusConflict, httpx.CodeRoomEnded, err.Error())
	default:
		httpx.Internal(w, logMsg, err, args...)
	}
}
