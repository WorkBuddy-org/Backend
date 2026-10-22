package chat

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/ws"
)

// MaxTextLength is the longest message, in characters (runes), after
// trimming.
const MaxTextLength = 2000

// Page sizes for GET /rooms/{id}/messages.
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// Participation tells whether a user is currently in a room; *room.Service
// implements it.
type Participation interface {
	HasOpenParticipation(ctx context.Context, roomID, userID uuid.UUID) (bool, error)
}

// EventSink delivers room broadcasts; *ws.Hub implements it.
type EventSink interface {
	Broadcast(roomID uuid.UUID, ev ws.Event)
}

type Service struct {
	repo    *Repository
	rooms   Participation
	events  EventSink
	limiter RateLimiter
}

// NewService builds the chat service. A nil events drops broadcasts; a nil
// limiter disables rate limiting.
func NewService(repo *Repository, rooms Participation, events EventSink, limiter RateLimiter) *Service {
	return &Service{repo: repo, rooms: rooms, events: events, limiter: limiter}
}

// Send stores a message from a user who is currently in the room and
// broadcasts it to the room, including the sender's own sockets.
func (s *Service) Send(ctx context.Context, roomID, userID uuid.UUID, text string) (*Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrTextRequired
	}
	if utf8.RuneCountInString(text) > MaxTextLength {
		return nil, ErrTextTooLong
	}
	if err := s.requireParticipant(ctx, roomID, userID); err != nil {
		return nil, err
	}
	if err := s.checkRate(ctx, userID); err != nil {
		return nil, err
	}

	m, err := s.repo.Create(ctx, roomID, userID, text)
	if err != nil {
		return nil, err
	}

	if s.events != nil {
		s.events.Broadcast(roomID, ws.NewEvent(ws.TypeChatMessage, roomID, ws.ChatMessageData{
			ID:        m.ID,
			RoomID:    m.RoomID,
			UserID:    m.UserID,
			UserName:  m.UserName,
			Text:      m.Text,
			CreatedAt: ws.FormatTime(m.CreatedAt),
		}))
	}
	return m, nil
}

// List returns a page of the room's history, newest first, for a user who is
// currently in the room. limit must already be within 1..MaxPageSize.
func (s *Service) List(ctx context.Context, roomID, userID uuid.UUID, before *uuid.UUID, limit int) (*Page, error) {
	if err := s.requireParticipant(ctx, roomID, userID); err != nil {
		return nil, err
	}

	// Fetch one extra row to know whether an older page exists.
	messages, err := s.repo.List(ctx, roomID, before, limit+1)
	if err != nil {
		return nil, err
	}

	page := &Page{Items: messages}
	if len(messages) > limit {
		page.Items = messages[:limit]
		next := page.Items[limit-1].ID
		page.NextCursor = &next
	}
	return page, nil
}

// checkRate counts a send against the user's limit. If the limiter fails, the
// message is let through: chat stays usable while Redis is down.
func (s *Service) checkRate(ctx context.Context, userID uuid.UUID) error {
	if s.limiter == nil {
		return nil
	}
	allowed, retryAfter, err := s.limiter.Allow(ctx, userID)
	if err != nil {
		slog.Warn("chat rate limiter unavailable, allowing message", "user_id", userID, "error", err)
		return nil
	}
	if !allowed {
		return &RateLimitedError{RetryAfter: retryAfter}
	}
	return nil
}

func (s *Service) requireParticipant(ctx context.Context, roomID, userID uuid.UUID) error {
	inRoom, err := s.rooms.HasOpenParticipation(ctx, roomID, userID)
	if err != nil {
		return err
	}
	if !inRoom {
		return ErrNotInRoom
	}
	return nil
}
