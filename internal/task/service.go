package task

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/ws"
)

// EventSink delivers WebSocket events; *ws.Hub implements it.
type EventSink interface {
	Broadcast(roomID uuid.UUID, ev ws.Event)
	SendToUser(userID uuid.UUID, ev ws.Event)
}

// TimerDeps configures the per-task timer.
type TimerDeps struct {
	// Index is required.
	Index *TimerIndex
	// Events receives timer events; nil drops them.
	Events EventSink
	// Clock defaults to time.Now. Tests inject a fake one.
	Clock func() time.Time
	// OnExpire defaults to the extension prompt.
	OnExpire ExpiryHandler
	// Extension left entirely zero defaults to DefaultExtensionPromptTimeout
	// and DefaultExtensionMaxCount.
	Extension ExtensionConfig
}

type Service struct {
	repo     *Repository
	rooms    *room.Service
	index    *TimerIndex
	events   EventSink
	clock    func() time.Time
	onExpire ExpiryHandler
	ext      ExtensionConfig
}

func NewService(repo *Repository, rooms *room.Service, timers TimerDeps) *Service {
	s := &Service{
		repo:     repo,
		rooms:    rooms,
		index:    timers.Index,
		events:   timers.Events,
		clock:    timers.Clock,
		onExpire: timers.OnExpire,
		ext:      timers.Extension,
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.onExpire == nil {
		s.onExpire = promptExtension{s}
	}
	if s.ext == (ExtensionConfig{}) {
		s.ext = ExtensionConfig{PromptTimeout: DefaultExtensionPromptTimeout, MaxCount: DefaultExtensionMaxCount}
	}
	return s
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

	updated, err := s.repo.Update(ctx, taskID, update)
	if err != nil {
		return nil, err
	}
	if existing.TimerActive() && updated.TimerStatus == TimerEnded {
		s.stopTimer(ctx, updated)
	}
	return updated, nil
}

func (s *Service) ListByUser(ctx context.Context, userID uuid.UUID) ([]Task, error) {
	return s.repo.ListByUser(ctx, userID)
}
