package task

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/room"
)

type Service struct {
	repo  *Repository
	rooms *room.Service
}

func NewService(repo *Repository, rooms *room.Service) *Service {
	return &Service{repo: repo, rooms: rooms}
}

// Create creates a task for userID in roomID. If the room has an active
// session, the task is linked to it; otherwise session_id is left null.
func (s *Service) Create(ctx context.Context, roomID, userID uuid.UUID, text string) (*Task, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrTextRequired
	}

	_, session, err := s.rooms.GetRoomDetail(ctx, roomID)
	if err != nil {
		return nil, err
	}

	var sessionID *uuid.UUID
	if session != nil {
		sessionID = &session.ID
	}

	return s.repo.Create(ctx, userID, roomID, sessionID, text)
}

func (s *Service) Update(ctx context.Context, taskID, userID uuid.UUID, update Update) (*Task, error) {
	existing, err := s.repo.FindByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if existing.UserID != userID {
		return nil, ErrForbidden
	}

	if update.Text != nil {
		trimmed := strings.TrimSpace(*update.Text)
		if trimmed == "" {
			return nil, ErrTextRequired
		}
		update.Text = &trimmed
	}

	return s.repo.Update(ctx, taskID, update)
}

func (s *Service) ListByUser(ctx context.Context, userID uuid.UUID) ([]Task, error) {
	return s.repo.ListByUser(ctx, userID)
}
