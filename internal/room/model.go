package room

import (
	"time"

	"github.com/google/uuid"
)

type Room struct {
	ID        uuid.UUID
	Name      string
	Type      string
	Tags      []string
	CreatedAt time.Time
}

type Session struct {
	ID        uuid.UUID
	RoomID    uuid.UUID
	StartedAt time.Time
	EndedAt   *time.Time
	Status    string
}

type Participation struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	RoomID     uuid.UUID
	SessionID  *uuid.UUID
	JoinedAt   time.Time
	LeftAt     *time.Time
	LeftReason *string
}
