package task

import (
	"time"

	"github.com/google/uuid"
)

type Task struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	RoomID           uuid.UUID
	SessionID        *uuid.UUID
	Text             string
	CompletedAt      *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ExtendedCount    int
	ExtraMinutesUsed int

	TimerStatus          string
	TimerDurationMinutes *int
	TimerStartedAt       *time.Time
	TimerEndsAt          *time.Time
	PromptDeadlineAt     *time.Time
}

// Values of tasks.timer_status.
const (
	TimerIdle      = "idle"
	TimerRunning   = "running"
	TimerPrompting = "prompting"
	TimerEnded     = "ended"
)

// TimerActive reports whether the task's timer counts as the user's one
// running timer.
func (t *Task) TimerActive() bool {
	return t.TimerStatus == TimerRunning || t.TimerStatus == TimerPrompting
}
