package ws

import (
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Event types sent from server to client. docs/ws-protocol.md is the
// contract for the frontend; keep it in sync with this list.
const (
	TypePresenceSnapshot = "presence_snapshot"
	TypeUserJoined       = "user_joined"
	TypeUserLeft         = "user_left"
	TypePong             = "pong"
	TypeRoomUpdated      = "room_updated"

	// Emitted by later phases.
	TypeTimerStarted        = "timer_started"
	TypeTimerEnded          = "timer_ended"
	TypeTaskExtensionPrompt = "task_extension_prompt"
	TypeTaskExtended        = "task_extended"
	TypeKicked              = "kicked"
	TypeChatMessage         = "chat_message"
	TypeTaskCreated         = "task_created"
	TypeTaskUpdated         = "task_updated"
	TypeTaskDeleted         = "task_deleted"
)

// LeftReasonDisconnect is the user_left reason when the user's last
// connection to the room closed, cleanly or not.
const LeftReasonDisconnect = "disconnect"

// LeftReasonKicked is the user_left reason (and room_participations
// left_reason) when the server removed the user from the room.
const LeftReasonKicked = "kicked"

// LeftReasonKickedNoExtension is the user_left reason (and
// room_participations left_reason) when the server removed the user because
// they declined or ignored the task extension prompt.
const LeftReasonKickedNoExtension = "kicked_no_extension"

// StatusRemovedFromRoom closes sockets of a user the server removed from the
// room (kicked); clients must not reconnect without joining again.
const StatusRemovedFromRoom websocket.StatusCode = 4001

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

// FormatTime renders t the way the envelope's ts is rendered: RFC 3339, UTC,
// millisecond precision.
func FormatTime(t time.Time) string {
	return t.UTC().Format(tsLayout)
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

// RoomUpdatedData is sent to the room when its participants change.
type RoomUpdatedData struct {
	RoomID            uuid.UUID `json:"room_id"`
	Status            string    `json:"status"`
	ParticipantsCount int       `json:"participants_count"`
	Capacity          int       `json:"capacity"`
}

// TimerStartedData announces a task timer. Clients count down locally from
// ends_at; server_time lets them correct for clock skew.
type TimerStartedData struct {
	TaskID          uuid.UUID `json:"task_id"`
	UserID          uuid.UUID `json:"user_id"`
	StartedAt       string    `json:"started_at"`
	EndsAt          string    `json:"ends_at"`
	DurationMinutes int       `json:"duration_minutes"`
	ServerTime      string    `json:"server_time"`
}

// timer_ended reasons.
const (
	TimerEndedExpired   = "expired"
	TimerEndedCompleted = "completed"
)

type TimerEndedData struct {
	TaskID     uuid.UUID `json:"task_id"`
	UserID     uuid.UUID `json:"user_id"`
	Reason     string    `json:"reason"`
	ServerTime string    `json:"server_time"`
}

// TaskExtensionPromptData is sent only to the task owner when their timer
// ends. Not answering by deadline counts as declining.
type TaskExtensionPromptData struct {
	TaskID         uuid.UUID `json:"task_id"`
	Deadline       string    `json:"deadline"`
	AllowedMinutes []int     `json:"allowed_minutes"`
	ExtensionsLeft int       `json:"extensions_left"`
	ServerTime     string    `json:"server_time"`
}

type TaskExtendedData struct {
	TaskID           uuid.UUID `json:"task_id"`
	UserID           uuid.UUID `json:"user_id"`
	AddedMinutes     int       `json:"added_minutes"`
	ExtendedCount    int       `json:"extended_count"`
	ExtraMinutesUsed int       `json:"extra_minutes_used"`
	EndsAt           string    `json:"ends_at"`
	ServerTime       string    `json:"server_time"`
}

// TaskAuthor is who a task belongs to.
type TaskAuthor struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	AvatarURL *string   `json:"avatar_url"`
}

// TaskEventData is the payload of task_created, task_updated and task_deleted:
// a task as GET /rooms/{id}/tasks returns it, plus task_id and user_id.
type TaskEventData struct {
	TaskID          uuid.UUID  `json:"task_id"`
	UserID          uuid.UUID  `json:"user_id"`
	ID              uuid.UUID  `json:"id"`
	Text            string     `json:"text"`
	Author          TaskAuthor `json:"author"`
	Completed       bool       `json:"completed"`
	TimerStatus     string     `json:"timer_status"`
	DurationMinutes *int       `json:"duration_minutes"`
	TimerStartedAt  *string    `json:"timer_started_at"`
	EndsAt          *string    `json:"ends_at"`
	ExtendedCount   int        `json:"extended_count"`
}

type KickedData struct {
	Reason string `json:"reason"`
}

// ChatMessageData is a chat message as stored; created_at uses FormatTime.
type ChatMessageData struct {
	ID        uuid.UUID `json:"id"`
	RoomID    uuid.UUID `json:"room_id"`
	UserID    uuid.UUID `json:"user_id"`
	UserName  string    `json:"user_name"`
	Text      string    `json:"text"`
	CreatedAt string    `json:"created_at"`
}

// clientMessage is the only shape the server reads from clients; anything
// other than {"type":"ping"} is ignored.
type clientMessage struct {
	Type string `json:"type"`
}
