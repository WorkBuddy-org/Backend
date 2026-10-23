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

// Participant is a user currently in a room.
type Participant struct {
	UserID   uuid.UUID
	Name     string
	JoinedAt time.Time
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
