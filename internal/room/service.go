package room

import (
	"context"

	"github.com/google/uuid"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
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

func (s *Service) Leave(ctx context.Context, roomID, userID uuid.UUID) error {
	existing, err := s.repo.FindOpenParticipation(ctx, userID, roomID)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrNotInRoom
	}

	return s.repo.CloseParticipation(ctx, existing.ID, "left_voluntarily")
}
