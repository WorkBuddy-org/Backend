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
}
