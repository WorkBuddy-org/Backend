package task

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
	// Extension's zero fields take their defaults; see ExtensionConfig.
	Extension ExtensionConfig
}

// MaxTextLength caps a task's text, in characters (runes) after trimming.
const MaxTextLength = 200

// normalizeText trims text and checks it is 1..MaxTextLength characters.
func normalizeText(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrTextRequired
	}
	if utf8.RuneCountInString(text) > MaxTextLength {
		return "", ErrTextTooLong
	}
	return text, nil
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
	s.ext = s.ext.withDefaults()
	return s
}

// Create creates a task for userID in roomID. If the room has an active
// session, the task is linked to it; otherwise session_id is left null.
func (s *Service) Create(ctx context.Context, roomID, userID uuid.UUID, text string) (*Task, error) {
	text, err := normalizeText(text)
	if err != nil {
		return nil, err
	}

	_, session, err := s.rooms.GetRoomDetail(ctx, roomID)
	if err != nil {
		return nil, err
	}

	var sessionID *uuid.UUID
	if session != nil {
		sessionID = &session.ID
	}

	t, err := s.repo.Create(ctx, userID, roomID, sessionID, text)
	if err != nil {
		return nil, err
	}
	s.publishTask(ctx, ws.TypeTaskCreated, t)
	return t, nil
}

// CreateWithTimer creates a task for userID in roomID with its timer already
// running. It follows the rules of StartTimer: the user must be in the room
// and have no other active timer. On any failure no task remains.
func (s *Service) CreateWithTimer(ctx context.Context, roomID, userID uuid.UUID, text string, minutes int) (*Task, error) {
	text, err := normalizeText(text)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(TimerDurations, minutes) {
		return nil, ErrInvalidDuration
	}

	_, session, err := s.rooms.GetRoomDetail(ctx, roomID)
	if err != nil {
		return nil, err
	}
	inRoom, err := s.rooms.HasOpenParticipation(ctx, roomID, userID)
	if err != nil {
		return nil, err
	}
	if !inRoom {
		return nil, ErrNotInRoom
	}

	var sessionID *uuid.UUID
	if session != nil {
		sessionID = &session.ID
	}

	startedAt := s.now()
	endsAt := startedAt.Add(time.Duration(minutes) * time.Minute)
	t, err := s.repo.CreateWithTimer(ctx, userID, roomID, sessionID, text, minutes, startedAt, endsAt)
	if err != nil {
		return nil, err
	}

	if err := s.index.timers.schedule(ctx, t.ID, endsAt); err != nil {
		// Without an index entry the timer would never fire; undo the task.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if deleteErr := s.repo.Delete(cleanupCtx, t.ID); deleteErr != nil {
			slog.Error("failed to delete task with unscheduled timer", "task_id", t.ID, "error", deleteErr)
		}
		return nil, err
	}

	s.publishTask(ctx, ws.TypeTaskCreated, t)
	s.broadcastTimerStarted(t, startedAt, endsAt, minutes)
	return t, nil
}

// ListByRoom returns the tasks of everyone currently in the room. The caller
// must be in the room too.
func (s *Service) ListByRoom(ctx context.Context, roomID, callerID uuid.UUID) ([]RoomTask, error) {
	inRoom, err := s.rooms.HasOpenParticipation(ctx, roomID, callerID)
	if err != nil {
		return nil, err
	}
	if !inRoom {
		return nil, ErrNotParticipant
	}
	return s.repo.ListByRoom(ctx, roomID)
}

// Delete removes the owner's task and stops its timer. A scheduler holding a
// stale due entry finds no row and does nothing.
func (s *Service) Delete(ctx context.Context, taskID, userID uuid.UUID) error {
	existing, err := s.repo.FindByID(ctx, taskID)
	if err != nil {
		return err
	}
	if existing.UserID != userID {
		return ErrForbidden
	}

	if err := s.repo.Delete(ctx, taskID); err != nil {
		return err
	}
	s.unschedule(ctx, taskID)
	s.publishTask(ctx, ws.TypeTaskDeleted, existing)
	return nil
}

// publishTask broadcasts a task event to the task's room. The change is
// already stored, so a cancelled request must not lose the event.
func (s *Service) publishTask(ctx context.Context, eventType string, t *Task) {
	if s.events == nil {
		return
	}
	name, avatarURL, err := s.repo.FindAuthor(context.WithoutCancel(ctx), t.UserID)
	if err != nil {
		slog.Error("failed to load task author for event", "task_id", t.ID, "error", err)
		return
	}
	s.broadcast(t.RoomID, ws.NewEvent(eventType, t.RoomID, ws.TaskEventData{
		TaskID:          t.ID,
		UserID:          t.UserID,
		ID:              t.ID,
		Text:            t.Text,
		Author:          ws.TaskAuthor{ID: t.UserID, Name: name, AvatarURL: avatarURL},
		Completed:       t.CompletedAt != nil,
		TimerStatus:     t.TimerStatus,
		DurationMinutes: t.TimerDurationMinutes,
		TimerStartedAt:  formatOptionalTime(t.TimerStartedAt),
		EndsAt:          formatOptionalTime(t.TimerEndsAt),
		ExtendedCount:   t.ExtendedCount,
	}))
}

func formatOptionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	formatted := ws.FormatTime(*t)
	return &formatted
}

func (s *Service) Update(ctx context.Context, taskID, userID uuid.UUID, update Update) (*Task, error) {
	if update.Text == nil && update.Completed == nil {
		return nil, ErrNoFields
	}

	existing, err := s.repo.FindByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if existing.UserID != userID {
		return nil, ErrForbidden
	}

	if update.Text != nil {
		trimmed, err := normalizeText(*update.Text)
		if err != nil {
			return nil, err
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
	s.publishTask(ctx, ws.TypeTaskUpdated, updated)
	return updated, nil
}

// ListByUser returns a page of the user's tasks, newest first. limit must
// already be within 1..MaxListLimit and offset non-negative.
func (s *Service) ListByUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]Task, error) {
	return s.repo.ListByUser(ctx, userID, limit, offset)
}
