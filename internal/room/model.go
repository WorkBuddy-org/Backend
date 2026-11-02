package room

import (
	"time"

	"github.com/google/uuid"
)

// Values of rooms.type.
const (
	TypeFocus      = "focus"
	TypeDiscussion = "discussion"
)

// Room statuses, computed by ComputeStatus and never stored.
const (
	StatusLive         = "live"
	StatusStartingSoon = "starting_soon"
	StatusOpen         = "open"
	StatusFull         = "full"
	StatusEnded        = "ended"
)

// MaxPreviewParticipants is how many participants a room view carries.
const MaxPreviewParticipants = 4

type Room struct {
	ID        uuid.UUID
	Name      string
	Type      string
	Tags      []string
	Capacity  int
	StartsAt  *time.Time
	EndsAt    *time.Time
	CreatedAt time.Time
}

// ParticipantPreview is one of the first participants shown on a room card.
type ParticipantPreview struct {
	UserID    uuid.UUID
	Name      string
	AvatarURL *string
}

// View is a room with what the room screens show about it right now.
type View struct {
	Room
	Status            string
	Participants      []ParticipantPreview
	ParticipantsCount int
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
