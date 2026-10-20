package ws

import (
	"time"

	"github.com/google/uuid"
)

// Event types sent from server to client. docs/ws-protocol.md is the
// contract for the frontend; keep it in sync with this list.
const (
	TypePresenceSnapshot = "presence_snapshot"
	TypeUserJoined       = "user_joined"
	TypeUserLeft         = "user_left"
	TypePong             = "pong"

	// Emitted by later phases.
	TypeTimerStarted        = "timer_started"
	TypeTimerEnded          = "timer_ended"
	TypeTaskExtensionPrompt = "task_extension_prompt"
	TypeTaskExtended        = "task_extended"
	TypeKicked              = "kicked"
	TypeChatMessage         = "chat_message"
)

// LeftReasonDisconnect is the user_left reason when the user's last
// connection to the room closed, cleanly or not.
const LeftReasonDisconnect = "disconnect"

// tsLayout is RFC 3339 in UTC with fixed millisecond precision.
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// Event is the envelope for every server-to-client message.
type Event struct {
	Type   string    `json:"type"`
	RoomID uuid.UUID `json:"room_id"`
	TS     string    `json:"ts"`
	Data   any       `json:"data"`
}

// NewEvent stamps an event with the current time. A nil data becomes {} so
// clients can always read event.data.
func NewEvent(eventType string, roomID uuid.UUID, data any) Event {
	if data == nil {
		data = struct{}{}
	}
	return Event{
		Type:   eventType,
		RoomID: roomID,
		TS:     time.Now().UTC().Format(tsLayout),
		Data:   data,
	}
}

type PresenceUser struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
}

type PresenceSnapshotData struct {
	Users []PresenceUser `json:"users"`
}

type UserJoinedData struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
}

type UserLeftData struct {
	UserID uuid.UUID `json:"user_id"`
	Reason string    `json:"reason"`
}

// clientMessage is the only shape the server reads from clients; anything
// other than {"type":"ping"} is ignored.
type clientMessage struct {
	Type string `json:"type"`
}
