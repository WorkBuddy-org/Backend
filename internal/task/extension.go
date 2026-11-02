package task

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/ws"
)

const (
	DefaultExtensionPromptTimeout   = 60 * time.Second
	DefaultExtensionMaxCount        = 3
	DefaultExtensionMaxTotalMinutes = 30
)

// DefaultExtensionOptions are the minutes offered by the prompt.
var DefaultExtensionOptions = []int{5, 10, 15}

type ExtensionConfig struct {
	// PromptTimeout is how long the owner has to answer the prompt.
	PromptTimeout time.Duration
	// MaxCount caps extensions per task.
	MaxCount int
	// Options are the minutes POST /tasks/{id}/extend accepts.
	Options []int
	// MaxTotalMinutes caps the extra minutes per task, over all extensions.
	MaxTotalMinutes int
}

// withDefaults fills in zero values. A config left entirely zero also gets
// DefaultExtensionMaxCount; otherwise MaxCount 0 means no extensions.
func (c ExtensionConfig) withDefaults() ExtensionConfig {
	if c.PromptTimeout == 0 && c.MaxCount == 0 && len(c.Options) == 0 && c.MaxTotalMinutes == 0 {
		c.MaxCount = DefaultExtensionMaxCount
	}
	if c.PromptTimeout == 0 {
		c.PromptTimeout = DefaultExtensionPromptTimeout
	}
	if len(c.Options) == 0 {
		c.Options = DefaultExtensionOptions
	}
	if c.MaxTotalMinutes == 0 {
		c.MaxTotalMinutes = DefaultExtensionMaxTotalMinutes
	}
	return c
}

// allowedMinutes returns the options the task can still use: none once the
// count limit is reached, and none that would exceed the total minutes.
func (c ExtensionConfig) allowedMinutes(t *Task) []int {
	allowed := []int{}
	if t.ExtendedCount >= c.MaxCount {
		return allowed
	}
	for _, minutes := range c.Options {
		if t.ExtraMinutesUsed+minutes <= c.MaxTotalMinutes {
			allowed = append(allowed, minutes)
		}
	}
	return allowed
}

// promptExtension is the default ExpiryHandler. It tells the room the timer
// ended, gives the owner PromptTimeout to ask for more time and sends the
// prompt to the owner's sockets in the task's room only.
type promptExtension struct {
	s *Service
}

func (p promptExtension) TimerExpired(ctx context.Context, t *Task) {
	s := p.s
	s.broadcastTimerEnded(t, ws.TimerEndedExpired)

	deadline := s.now().Add(s.ext.PromptTimeout)
	prompted, err := s.repo.SetPromptDeadline(ctx, t.ID, deadline)
	if errors.Is(err, ErrTaskNotFound) {
		// Completed in the meantime.
		return
	}
	if err != nil {
		// Still index the deadline: ending the prompt does not need it stored.
		slog.Error("failed to store extension prompt deadline", "task_id", t.ID, "error", err)
		prompted = t
	}

	if err := s.index.prompts.schedule(ctx, t.ID, deadline); err != nil {
		// The prompt cannot time out until the index is rebuilt on restart.
		slog.Error("failed to schedule extension prompt deadline", "task_id", t.ID, "error", err)
	}

	allowed := s.ext.allowedMinutes(prompted)
	left := max(s.ext.MaxCount-prompted.ExtendedCount, 0)
	if len(allowed) == 0 {
		left = 0
	}
	s.sendToUser(prompted.UserID, ws.NewEvent(ws.TypeTaskExtensionPrompt, prompted.RoomID, ws.TaskExtensionPromptData{
		TaskID:         prompted.ID,
		Deadline:       ws.FormatTime(deadline),
		AllowedMinutes: allowed,
		ExtensionsLeft: left,
		ServerTime:     ws.FormatTime(s.now()),
	}))
	s.publishTask(ctx, ws.TypeTaskUpdated, prompted)
}

// ExtendTimer answers the prompt with "more time": the timer runs again for
// the given minutes from now.
func (s *Service) ExtendTimer(ctx context.Context, taskID, userID uuid.UUID, minutes int) (*Task, error) {
	if !slices.Contains(s.ext.Options, minutes) {
		return nil, ErrInvalidExtension
	}

	existing, err := s.promptedTask(ctx, taskID, userID)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(s.ext.allowedMinutes(existing), minutes) {
		return nil, ErrExtensionLimit
	}
	now := s.now()
	if existing.PromptDeadlineAt != nil && !now.Before(*existing.PromptDeadlineAt) {
		return nil, ErrPromptExpired
	}

	// Taking the deadline entry makes this answer the only actor on the
	// prompt; the scheduler's claim on the same entry then fails.
	taken, err := s.index.prompts.take(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !taken {
		return nil, ErrPromptExpired
	}

	endsAt := now.Add(time.Duration(minutes) * time.Minute)
	t, err := s.repo.ExtendTimer(ctx, taskID, minutes, now, endsAt, s.ext.MaxCount, s.ext.MaxTotalMinutes)
	if err != nil {
		// Hand the deadline back so the prompt still ends.
		s.reschedulePrompt(ctx, existing)
		if errors.Is(err, ErrTaskNotFound) {
			return nil, ErrPromptExpired
		}
		return nil, err
	}

	if err := s.index.timers.schedule(ctx, t.ID, endsAt); err != nil {
		// The extension is stored; the timer is re-indexed on restart.
		slog.Error("failed to schedule extended timer", "task_id", t.ID, "error", err)
	}

	s.broadcast(t.RoomID, ws.NewEvent(ws.TypeTaskExtended, t.RoomID, ws.TaskExtendedData{
		TaskID:           t.ID,
		UserID:           t.UserID,
		AddedMinutes:     minutes,
		ExtendedCount:    t.ExtendedCount,
		ExtraMinutesUsed: t.ExtraMinutesUsed,
		EndsAt:           ws.FormatTime(endsAt),
		ServerTime:       ws.FormatTime(s.now()),
	}))
	s.publishTask(ctx, ws.TypeTaskUpdated, t)
	return t, nil
}

// DeclineExtension answers the prompt with "no": the timer ends and the user
// is removed from the room, exactly as when the prompt times out.
func (s *Service) DeclineExtension(ctx context.Context, taskID, userID uuid.UUID) (*Task, error) {
	if _, err := s.promptedTask(ctx, taskID, userID); err != nil {
		return nil, err
	}

	taken, err := s.index.prompts.take(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !taken {
		return nil, ErrPromptExpired
	}

	t, err := s.endPrompt(ctx, taskID, nil)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrNotPrompting
	}
	return t, nil
}

// promptedTask loads the task for an answer to its prompt.
func (s *Service) promptedTask(ctx context.Context, taskID, userID uuid.UUID) (*Task, error) {
	existing, err := s.repo.FindByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if existing.UserID != userID {
		return nil, ErrForbidden
	}
	if existing.TimerStatus != TimerPrompting {
		return nil, ErrNotPrompting
	}
	return existing, nil
}

// endPrompt ends a prompting timer and removes the owner from the task's
// room (left_reason 'kicked_no_extension'): the hub sends kicked to their sockets, closes
// them and tells the room. With dueBy set, the prompt must have expired by
// then. It returns nil when the timer was no longer prompting, so racing
// actors never both act.
func (s *Service) endPrompt(ctx context.Context, taskID uuid.UUID, dueBy *time.Time) (*Task, error) {
	t, err := s.repo.EndPrompt(ctx, taskID, dueBy)
	if errors.Is(err, ErrTaskNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Published before the kick: the owner's tasks leave the room's list then.
	s.publishTask(ctx, ws.TypeTaskUpdated, t)

	if err := s.rooms.KickNoExtension(ctx, t.RoomID, t.UserID); err != nil && !errors.Is(err, room.ErrNotInRoom) {
		slog.Error("failed to remove user after extension prompt", "task_id", t.ID, "room_id", t.RoomID, "error", err)
	}
	return t, nil
}

func (s *Service) reschedulePrompt(ctx context.Context, t *Task) {
	at := s.now()
	if t.PromptDeadlineAt != nil {
		at = *t.PromptDeadlineAt
	}
	if err := s.index.prompts.schedule(context.WithoutCancel(ctx), t.ID, at); err != nil {
		slog.Error("failed to reschedule extension prompt", "task_id", t.ID, "error", err)
	}
}

func (s *Service) sendToUser(userID uuid.UUID, ev ws.Event) {
	if s.events != nil {
		s.events.SendToUser(userID, ev)
	}
}
