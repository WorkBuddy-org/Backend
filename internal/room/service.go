package room

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/ws"
)

// Values of room_participations.left_reason.
const (
	LeftReasonVoluntary         = "left_voluntarily"
	LeftReasonKicked            = "kicked"
	LeftReasonKickedNoExtension = "kicked_no_extension"
)

// LeaveListener is told after a user's participation in a room is closed, so
// real-time connections to that room can be dropped.
type LeaveListener interface {
	UserLeft(roomID, userID uuid.UUID, reason string)
}

// EventPublisher delivers events to a room's WebSocket connections.
type EventPublisher interface {
	Broadcast(roomID uuid.UUID, ev ws.Event)
}

type Service struct {
	repo     *Repository
	listener LeaveListener
	events   EventPublisher
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

// SetLeaveListener registers l to be notified of every closed participation.
// It is a setter rather than a constructor argument because the listener (the
// WebSocket handler) itself depends on this service.
func (s *Service) SetLeaveListener(l LeaveListener) {
	s.listener = l
}

// SetEventPublisher registers p to receive room_updated events.
func (s *Service) SetEventPublisher(p EventPublisher) {
	s.events = p
}

type JoinResult struct {
	Room          *View
	Participation *Participation
}

// ComputeStatus is the single place a room's status is decided. The checks
// are ordered: an ended room stays ended however many people are still in it.
func ComputeStatus(rm *Room, participantsCount int, now time.Time) string {
	switch {
	case rm.EndsAt != nil && rm.EndsAt.Before(now):
		return StatusEnded
	case rm.StartsAt != nil && rm.StartsAt.After(now):
		return StatusStartingSoon
	case participantsCount >= rm.Capacity:
		return StatusFull
	case rm.Type == TypeDiscussion:
		return StatusOpen
	default:
		return StatusLive
	}
}

// ListRooms returns the matching rooms, each with its status and first
// participants.
func (s *Service) ListRooms(ctx context.Context, filter Filter) ([]View, error) {
	views, err := s.repo.ListRoomViews(ctx, filter)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range views {
		views[i].Status = ComputeStatus(&views[i].Room, views[i].ParticipantsCount, now)
	}
	return views, nil
}

// GetRoom is ListRooms for one room.
func (s *Service) GetRoom(ctx context.Context, roomID uuid.UUID) (*View, error) {
	views, err := s.ListRooms(ctx, Filter{ID: &roomID})
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, ErrRoomNotFound
	}
	return &views[0], nil
}

// GetRoomDetail returns the room and its active session, if any.
func (s *Service) GetRoomDetail(ctx context.Context, roomID uuid.UUID) (*Room, *Session, error) {
	rm, err := s.repo.FindRoomByID(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}

	session, err := s.repo.FindActiveSession(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}

	return rm, session, nil
}

// Join is idempotent: a user who already has an open participation gets it
// back as-is and is not counted twice, even if the room has since filled up or
// ended. Anyone else is admitted only while the room has not ended and has a
// free place; the repository checks both under a lock on the room row, so
// concurrent joins cannot overbook it.
func (s *Service) Join(ctx context.Context, roomID, userID uuid.UUID) (*JoinResult, error) {
	participation, created, err := s.repo.JoinRoom(ctx, userID, roomID, checkJoinable)
	if err != nil {
		return nil, err
	}

	view, err := s.GetRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if created {
		s.publishUpdate(view)
	}
	return &JoinResult{Room: view, Participation: participation}, nil
}

// checkJoinable decides whether a new participant may enter a room that
// already has count of them. It does not use ComputeStatus because a room that
// is starting soon can be full too, without its status saying so.
func checkJoinable(rm *Room, count int) error {
	if rm.EndsAt != nil && rm.EndsAt.Before(time.Now()) {
		return ErrRoomEnded
	}
	if count >= rm.Capacity {
		return ErrRoomFull
	}
	return nil
}

// HasOpenParticipation reports whether the user is currently in the room.
// A room that does not exist simply has no participations.
func (s *Service) HasOpenParticipation(ctx context.Context, roomID, userID uuid.UUID) (bool, error) {
	existing, err := s.repo.FindOpenParticipation(ctx, userID, roomID)
	if err != nil {
		return false, err
	}
	return existing != nil, nil
}

// ListParticipants returns who is currently in the room. The caller must be in
// the room too; a room that does not exist has no participants, so it gets
// ErrNotMember as well.
func (s *Service) ListParticipants(ctx context.Context, roomID, callerID uuid.UUID) ([]Participant, error) {
	inRoom, err := s.HasOpenParticipation(ctx, roomID, callerID)
	if err != nil {
		return nil, err
	}
	if !inRoom {
		return nil, ErrNotMember
	}
	return s.repo.ListParticipants(ctx, roomID)
}

func (s *Service) Leave(ctx context.Context, roomID, userID uuid.UUID) error {
	existing, err := s.repo.FindOpenParticipation(ctx, userID, roomID)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrNotInRoom
	}

	if err := s.repo.CloseParticipation(ctx, existing.ID, LeftReasonVoluntary); err != nil {
		return err
	}
	s.notifyLeft(ctx, roomID, userID, LeftReasonVoluntary)
	return nil
}

// Kick closes the user's open participation in the room with left_reason
// 'kicked' and notifies the listener. It returns ErrNotInRoom if the user had
// already left.
func (s *Service) Kick(ctx context.Context, roomID, userID uuid.UUID) error {
	return s.remove(ctx, roomID, userID, LeftReasonKicked)
}

// KickNoExtension is Kick for a user who declined or ignored the task
// extension prompt: left_reason is 'kicked_no_extension'.
func (s *Service) KickNoExtension(ctx context.Context, roomID, userID uuid.UUID) error {
	return s.remove(ctx, roomID, userID, LeftReasonKickedNoExtension)
}

func (s *Service) remove(ctx context.Context, roomID, userID uuid.UUID, reason string) error {
	existing, err := s.repo.FindOpenParticipation(ctx, userID, roomID)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrNotInRoom
	}

	if err := s.repo.CloseParticipation(ctx, existing.ID, reason); err != nil {
		return err
	}
	s.notifyLeft(ctx, roomID, userID, reason)
	return nil
}

func (s *Service) notifyLeft(ctx context.Context, roomID, userID uuid.UUID, reason string) {
	if s.listener != nil {
		s.listener.UserLeft(roomID, userID, reason)
	}
	if s.events == nil {
		return
	}
	// The participation is already closed, so a cancelled request must not
	// lose the event.
	view, err := s.GetRoom(context.WithoutCancel(ctx), roomID)
	if err != nil {
		slog.Error("failed to load room for room_updated", "room_id", roomID, "error", err)
		return
	}
	s.publishUpdate(view)
}

func (s *Service) publishUpdate(view *View) {
	if s.events == nil {
		return
	}
	s.events.Broadcast(view.ID, ws.NewEvent(ws.TypeRoomUpdated, view.ID, ws.RoomUpdatedData{
		RoomID:            view.ID,
		Status:            view.Status,
		ParticipantsCount: view.ParticipantsCount,
		Capacity:          view.Capacity,
	}))
}
