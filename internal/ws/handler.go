package ws

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/platform/httpx"
)

const (
	DefaultPingInterval = 25 * time.Second
	DefaultWriteTimeout = 10 * time.Second

	// cleanupTimeout bounds Redis work done after a connection is gone, when
	// the request context can no longer be relied on.
	cleanupTimeout = 5 * time.Second
)

// errNotInRoom means the user's participation closed between the handshake
// check and registration.
var errNotInRoom = errors.New("not in room")

type TokenParser interface {
	ParseAccessToken(token string) (*auth.AccessClaims, error)
}

type ParticipationChecker interface {
	HasOpenParticipation(ctx context.Context, roomID, userID uuid.UUID) (bool, error)
}

type Config struct {
	// AllowedOrigins are exact browser origins, e.g. https://app.example.com.
	AllowedOrigins []string
	// AllowAnyOrigin skips the origin check. Only for ENV=development.
	AllowAnyOrigin bool
	// PingInterval and WriteTimeout default to DefaultPingInterval and
	// DefaultWriteTimeout when zero.
	PingInterval time.Duration
	WriteTimeout time.Duration
}

type HandlerDeps struct {
	Hub      *Hub
	Presence *PresenceStore
	Tokens   TokenParser
	Rooms    ParticipationChecker
	Config   Config
}

type Handler struct {
	hub      *Hub
	presence *PresenceStore
	tokens   TokenParser
	rooms    ParticipationChecker

	allowedOrigins map[string]struct{}
	allowAnyOrigin bool
	pingInterval   time.Duration
	writeTimeout   time.Duration

	// lifecycle serialises connect/disconnect so the hub's per-user
	// connection count and the Redis presence set change together: a tab
	// closing while another opens can't leave Redis out of sync, and only the
	// first connect / last disconnect emits user_joined / user_left.
	lifecycle sync.Mutex
}

func NewHandler(deps HandlerDeps) *Handler {
	origins := make(map[string]struct{}, len(deps.Config.AllowedOrigins))
	for _, o := range deps.Config.AllowedOrigins {
		origins[normalizeOrigin(o)] = struct{}{}
	}

	h := &Handler{
		hub:            deps.Hub,
		presence:       deps.Presence,
		tokens:         deps.Tokens,
		rooms:          deps.Rooms,
		allowedOrigins: origins,
		allowAnyOrigin: deps.Config.AllowAnyOrigin,
		pingInterval:   deps.Config.PingInterval,
		writeTimeout:   deps.Config.WriteTimeout,
	}
	if h.pingInterval <= 0 {
		h.pingInterval = DefaultPingInterval
	}
	if h.writeTimeout <= 0 {
		h.writeTimeout = DefaultWriteTimeout
	}
	return h
}

// RegisterRoutes mounts the WebSocket endpoint. It sits outside RequireAuth
// because browsers cannot set headers on a WebSocket handshake; the token
// comes from ?access_token= instead.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/rooms/{id}/ws", h.connect)
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	roomID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid room id")
		return
	}

	if !h.originAllowed(r.Header.Get("Origin")) {
		httpx.Error(w, http.StatusForbidden, "origin not allowed")
		return
	}

	userID, ok := h.authenticate(r)
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "missing or invalid access token")
		return
	}

	ctx := r.Context()
	inRoom, err := h.rooms.HasOpenParticipation(ctx, roomID, userID)
	if err != nil {
		slog.Error("websocket participation check failed", "room_id", roomID, "user_id", userID, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !inRoom {
		httpx.Error(w, http.StatusForbidden, "join the room before connecting")
		return
	}

	// net/http clears the server's Read/WriteTimeout deadlines on hijack;
	// liveness of the socket is handled by pings from here on.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The origin was already checked above against WS_ALLOWED_ORIGINS.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Debug("websocket accept failed", "error", err)
		return
	}

	c := newClient(conn, roomID, userID)
	if err := h.join(ctx, c); errors.Is(err, errNotInRoom) {
		_ = conn.Close(StatusRemovedFromRoom, "not in room")
		return
	} else if err != nil {
		slog.Error("websocket join failed", "room_id", roomID, "user_id", userID, "error", err)
		_ = conn.Close(websocket.StatusInternalError, "presence unavailable")
		return
	}

	c.serve(ctx, h.pingInterval, h.writeTimeout)
	h.leave(c)
}

func (h *Handler) authenticate(r *http.Request) (uuid.UUID, bool) {
	token := r.URL.Query().Get("access_token")
	if token == "" {
		return uuid.Nil, false
	}
	claims, err := h.tokens.ParseAccessToken(token)
	if err != nil {
		return uuid.Nil, false
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, false
	}
	return userID, true
}

// join records presence, queues the snapshot as the connection's first
// message, then registers the connection so later broadcasts follow it.
func (h *Handler) join(ctx context.Context, c *client) error {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	// Re-check under the lock: a REST leave that committed after the
	// handshake check has already run removeFromRoom, which would not have
	// seen this connection.
	inRoom, err := h.rooms.HasOpenParticipation(ctx, c.roomID, c.userID)
	if err != nil {
		return err
	}
	if !inRoom {
		return errNotInRoom
	}

	if err := h.presence.Add(ctx, c.roomID, c.userID); err != nil {
		return err
	}

	users, err := h.presence.List(ctx, c.roomID)
	if err != nil {
		if !h.hub.userInRoom(c.userID, c.roomID) {
			h.removePresence(c)
		}
		return err
	}

	if msg, ok := marshalEvent(NewEvent(TypePresenceSnapshot, c.roomID, PresenceSnapshotData{Users: users})); ok {
		c.enqueue(msg)
	}

	if first := h.hub.add(c); first {
		h.hub.broadcastExcept(c.roomID, NewEvent(TypeUserJoined, c.roomID, UserJoinedData{
			UserID: c.userID,
			Name:   nameOf(users, c.userID),
		}), c)
	}
	return nil
}

func (h *Handler) leave(c *client) {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	if last := h.hub.remove(c); !last {
		return
	}
	h.removePresence(c)
	h.hub.Broadcast(c.roomID, NewEvent(TypeUserLeft, c.roomID, UserLeftData{
		UserID: c.userID,
		Reason: LeftReasonDisconnect,
	}))
}

// UserLeft implements room.LeaveListener: after a user's participation in
// the room is closed, their sockets to it are closed, presence is cleared and
// the room gets a single user_left with the given reason. A voluntary leave
// closes with 1000; a kick (either kicked reason) first sends kicked to those
// sockets and closes with 4001.
func (h *Handler) UserLeft(roomID, userID uuid.UUID, reason string) {
	if reason == LeftReasonKicked || reason == LeftReasonKickedNoExtension {
		kicked := NewEvent(TypeKicked, roomID, KickedData{Reason: reason})
		h.removeFromRoom(roomID, userID, reason, StatusRemovedFromRoom, "removed from room", &kicked)
		return
	}
	h.removeFromRoom(roomID, userID, reason, websocket.StatusNormalClosure, "left room", nil)
}

// removeFromRoom drops every connection the user has to the room, sending
// last (if not nil) to each of them before the close frame. The connections'
// own leave() becomes a no-op because the hub no longer knows them, so
// user_left is broadcast exactly once, here.
func (h *Handler) removeFromRoom(roomID, userID uuid.UUID, reason string, code websocket.StatusCode, closeReason string, last *Event) {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	var lastMsg []byte
	if last != nil {
		lastMsg, _ = marshalEvent(*last)
	}

	removed := h.hub.removeUserFromRoom(userID, roomID)
	for _, c := range removed {
		if lastMsg != nil {
			c.enqueue(lastMsg)
		}
		c.closeAfterFlush(code, closeReason)
	}
	if len(removed) == 0 {
		return
	}

	h.removePresence(removed[0])
	h.hub.Broadcast(roomID, NewEvent(TypeUserLeft, roomID, UserLeftData{
		UserID: userID,
		Reason: reason,
	}))
}

func (h *Handler) removePresence(c *client) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := h.presence.Remove(ctx, c.roomID, c.userID); err != nil {
		slog.Error("failed to remove presence", "room_id", c.roomID, "user_id", c.userID, "error", err)
	}
}

// originAllowed accepts requests without an Origin header (non-browser
// clients) and browser origins on the allow-list.
func (h *Handler) originAllowed(origin string) bool {
	if origin == "" || h.allowAnyOrigin {
		return true
	}
	_, ok := h.allowedOrigins[normalizeOrigin(origin)]
	return ok
}

func normalizeOrigin(origin string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(origin)), "/")
}

func nameOf(users []PresenceUser, userID uuid.UUID) string {
	for _, u := range users {
		if u.UserID == userID {
			return u.Name
		}
	}
	return unknownUserName
}
