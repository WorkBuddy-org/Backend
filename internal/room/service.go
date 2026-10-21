package room

import (
	"context"

	"github.com/google/uuid"
)

// Values of room_participations.left_reason.
const (
	LeftReasonVoluntary = "left_voluntarily"
	LeftReasonKicked    = "kicked"
)

// LeaveListener is told after a user's participation in a room is closed, so
// real-time connections to that room can be dropped.
type LeaveListener interface {
	UserLeft(roomID, userID uuid.UUID, reason string)
}

type Service struct {
	repo     *Repository
	listener LeaveListener
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

type JoinResult struct {
	Room          *Room
	ActiveSession *Session
	Participation *Participation
}

func (s *Service) ListRooms(ctx context.Context, filter Filter) ([]Room, error) {
	return s.repo.ListRooms(ctx, filter)
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

// Join is idempotent: if the user already has an open participation in this
// room, it is returned as-is rather than creating a duplicate.
func (s *Service) Join(ctx context.Context, roomID, userID uuid.UUID) (*JoinResult, error) {
	rm, session, err := s.GetRoomDetail(ctx, roomID)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.FindOpenParticipation(ctx, userID, roomID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &JoinResult{Room: rm, ActiveSession: session, Participation: existing}, nil
	}

	var sessionID *uuid.UUID
	if session != nil {
		sessionID = &session.ID
	}

	participation, err := s.repo.CreateParticipation(ctx, userID, roomID, sessionID)
	if err != nil {
		return nil, err
	}

	return &JoinResult{Room: rm, ActiveSession: session, Participation: participation}, nil
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
	s.notifyLeft(roomID, userID, LeftReasonVoluntary)
	return nil
}

func (s *Service) notifyLeft(roomID, userID uuid.UUID, reason string) {
	if s.listener != nil {
		s.listener.UserLeft(roomID, userID, reason)
	}
}
