package task

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/ws"
)

// TimerDurations are the allowed duration_minutes for POST /tasks/{id}/start.
var TimerDurations = []int{25, 50, 90}

// DefaultSchedulerInterval is how often the scheduler polls for due timers.
const DefaultSchedulerInterval = time.Second

// cleanupTimeout bounds best-effort Redis work that must not be cut short by
// a cancelled request.
const cleanupTimeout = 5 * time.Second

// ExpiryHandler decides what happens when a task's timer runs out. It is
// called exactly once per expiry, after the task has moved to 'prompting'.
// The extension flow plugs in here.
type ExpiryHandler interface {
	TimerExpired(ctx context.Context, t *Task)
}

// broadcastExpiry is the default ExpiryHandler: it only tells the room.
type broadcastExpiry struct {
	s *Service
}

func (b broadcastExpiry) TimerExpired(_ context.Context, t *Task) {
	b.s.broadcastTimerEnded(t, ws.TimerEndedExpired)
}

// StartTimer starts the task's timer for the given number of minutes. Only
// the owner may start it, only while they are in the task's room, and only
// if they have no other running or prompting timer.
func (s *Service) StartTimer(ctx context.Context, taskID, userID uuid.UUID, minutes int) (*Task, error) {
	if !slices.Contains(TimerDurations, minutes) {
		return nil, ErrInvalidDuration
	}

	existing, err := s.repo.FindByID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if existing.UserID != userID {
		return nil, ErrForbidden
	}
	if existing.CompletedAt != nil {
		return nil, ErrTaskCompleted
	}

	inRoom, err := s.rooms.HasOpenParticipation(ctx, existing.RoomID, userID)
	if err != nil {
		return nil, err
	}
	if !inRoom {
		return nil, ErrNotInRoom
	}

	startedAt := s.now()
	endsAt := startedAt.Add(time.Duration(minutes) * time.Minute)
	t, err := s.repo.StartTimer(ctx, taskID, minutes, startedAt, endsAt)
	if err != nil {
		return nil, err
	}

	if err := s.index.timers.schedule(ctx, t.ID, endsAt); err != nil {
		// Without an index entry the timer would never fire; undo it.
		resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if resetErr := s.repo.ResetTimer(resetCtx, t.ID); resetErr != nil {
			slog.Error("failed to reset unscheduled timer", "task_id", t.ID, "error", resetErr)
		}
		return nil, err
	}

	now := s.now()
	s.broadcast(t.RoomID, ws.NewEvent(ws.TypeTimerStarted, t.RoomID, ws.TimerStartedData{
		TaskID:          t.ID,
		UserID:          t.UserID,
		StartedAt:       ws.FormatTime(startedAt),
		EndsAt:          ws.FormatTime(endsAt),
		DurationMinutes: minutes,
		ServerTime:      ws.FormatTime(now),
	}))
	return t, nil
}

// stopTimer drops a timer that was ended by completing its task.
func (s *Service) stopTimer(ctx context.Context, t *Task) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.index.timers.unschedule(cleanupCtx, t.ID); err != nil {
		// Harmless: the scheduler's conditional expire ignores ended timers.
		slog.Error("failed to unschedule stopped timer", "task_id", t.ID, "error", err)
	}
	s.broadcastTimerEnded(t, ws.TimerEndedCompleted)
}

// RecoverTimers rebuilds the Redis index from Postgres. Timers that ended
// while the server was down are due immediately and fire on the next tick.
func (s *Service) RecoverTimers(ctx context.Context) (int, error) {
	active, err := s.repo.ListActiveTimers(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.index.rebuild(ctx, active); err != nil {
		return 0, err
	}
	return len(active), nil
}

// FireDue expires every timer whose end has passed. Each entry is claimed
// first, so with several pollers only one acts on it.
func (s *Service) FireDue(ctx context.Context) error {
	now := s.now()
	ids, err := s.index.timers.due(ctx, now)
	if err != nil {
		return err
	}

	for _, id := range ids {
		claimed, err := s.index.timers.claim(ctx, id, now)
		if err != nil {
			return err
		}
		if claimed {
			s.expireTimer(ctx, id, now)
		}
	}
	return nil
}

func (s *Service) expireTimer(ctx context.Context, taskID uuid.UUID, now time.Time) {
	t, err := s.repo.ExpireTimer(ctx, taskID, now)
	if errors.Is(err, ErrTaskNotFound) {
		// Completed, extended or deleted since it was indexed.
		return
	}
	if err != nil {
		slog.Error("failed to expire timer", "task_id", taskID, "error", err)
		// Put it back so the next tick retries.
		if err := s.index.timers.schedule(ctx, taskID, now); err != nil {
			slog.Error("failed to reschedule timer", "task_id", taskID, "error", err)
		}
		return
	}
	s.onExpire.TimerExpired(ctx, t)
}

func (s *Service) broadcastTimerEnded(t *Task, reason string) {
	s.broadcast(t.RoomID, ws.NewEvent(ws.TypeTimerEnded, t.RoomID, ws.TimerEndedData{
		TaskID:     t.ID,
		UserID:     t.UserID,
		Reason:     reason,
		ServerTime: ws.FormatTime(s.now()),
	}))
}

func (s *Service) broadcast(roomID uuid.UUID, ev ws.Event) {
	if s.events != nil {
		s.events.Broadcast(roomID, ev)
	}
}

// now is the service clock, truncated to what Postgres and the index store.
func (s *Service) now() time.Time {
	return s.clock().UTC().Truncate(time.Millisecond)
}

// Scheduler polls the timer index and fires due timers. Events are state
// changes only; there are no per-second ticks to clients.
type Scheduler struct {
	service  *Service
	interval time.Duration
	wg       sync.WaitGroup
}

func NewScheduler(service *Service, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = DefaultSchedulerInterval
	}
	return &Scheduler{service: service, interval: interval}
}

// Start rebuilds the index from Postgres, then polls in a goroutine until ctx
// is cancelled. Call Wait after cancelling to let an in-flight tick finish.
func (s *Scheduler) Start(ctx context.Context) error {
	n, err := s.service.RecoverTimers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		slog.Info("recovered task timers", "count", n)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(ctx)
	}()
	return nil
}

func (s *Scheduler) Wait() {
	s.wg.Wait()
}

func (s *Scheduler) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		if err := s.service.FireDue(ctx); err != nil && ctx.Err() == nil {
			slog.Error("timer scheduler tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
