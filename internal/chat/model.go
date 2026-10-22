package chat

import (
	"time"

	"github.com/google/uuid"
)

type Message struct {
	ID        uuid.UUID
	RoomID    uuid.UUID
	UserID    uuid.UUID
	UserName  string
	Text      string
	CreatedAt time.Time
}

// Page is one page of a room's history, newest first. NextCursor is the id to
// pass as before for the next (older) page; nil when there are no more.
type Page struct {
	Items      []Message
	NextCursor *uuid.UUID
}
