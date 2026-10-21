package ws

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Hub tracks live connections in memory. It is per process: with more than
// one server instance, Broadcast and SendToUser only reach clients connected
// to this instance. That is accepted for the MVP (single instance).
type Hub struct {
	mu    sync.RWMutex
	rooms map[uuid.UUID]map[*client]struct{}
	users map[uuid.UUID]map[*client]struct{}
}

func NewHub() *Hub {
	return &Hub{
		rooms: make(map[uuid.UUID]map[*client]struct{}),
		users: make(map[uuid.UUID]map[*client]struct{}),
	}
}

// Broadcast sends the event to every connection in the room.
func (h *Hub) Broadcast(roomID uuid.UUID, ev Event) {
	h.broadcastExcept(roomID, ev, nil)
}

// SendToUser sends the event to the user's connections. When ev.RoomID is
// set, only connections to that room receive it; otherwise all of them do.
func (h *Hub) SendToUser(userID uuid.UUID, ev Event) {
	msg, ok := marshalEvent(ev)
	if !ok {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.users[userID] {
		if ev.RoomID == uuid.Nil || c.roomID == ev.RoomID {
			c.enqueue(msg)
		}
	}
}

// RoomConnections returns how many live connections the room has on this
// instance.
func (h *Hub) RoomConnections(roomID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms[roomID])
}

// CloseAll closes every connection with 1001 Going Away. Used on shutdown,
// since http.Server.Shutdown does not touch hijacked connections.
func (h *Hub) CloseAll() {
	h.mu.RLock()
	clients := make([]*client, 0)
	for _, conns := range h.rooms {
		for c := range conns {
			clients = append(clients, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range clients {
		c.close(websocket.StatusGoingAway, "server shutting down")
	}
}

func (h *Hub) broadcastExcept(roomID uuid.UUID, ev Event, except *client) {
	msg, ok := marshalEvent(ev)
	if !ok {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[roomID] {
		if c != except {
			c.enqueue(msg)
		}
	}
}

// add registers c and reports whether it is the user's first connection to
// the room.
func (h *Hub) add(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	first := !h.userInRoomLocked(c.userID, c.roomID)

	if h.rooms[c.roomID] == nil {
		h.rooms[c.roomID] = make(map[*client]struct{})
	}
	h.rooms[c.roomID][c] = struct{}{}

	if h.users[c.userID] == nil {
		h.users[c.userID] = make(map[*client]struct{})
	}
	h.users[c.userID][c] = struct{}{}

	return first
}

// remove unregisters c and reports whether it was the user's last connection
// to the room. Removing an unknown client returns false.
func (h *Hub) remove(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.rooms[c.roomID][c]; !ok {
		return false
	}

	delete(h.rooms[c.roomID], c)
	if len(h.rooms[c.roomID]) == 0 {
		delete(h.rooms, c.roomID)
	}
	delete(h.users[c.userID], c)
	if len(h.users[c.userID]) == 0 {
		delete(h.users, c.userID)
	}

	return !h.userInRoomLocked(c.userID, c.roomID)
}

// removeUserFromRoom unregisters all of the user's connections to the room
// and returns them, so the caller can close them. A later remove of any of
// them returns false.
func (h *Hub) removeUserFromRoom(userID, roomID uuid.UUID) []*client {
	h.mu.Lock()
	defer h.mu.Unlock()

	var removed []*client
	for c := range h.users[userID] {
		if c.roomID != roomID {
			continue
		}
		removed = append(removed, c)
		delete(h.users[userID], c)
		delete(h.rooms[roomID], c)
	}
	if len(h.users[userID]) == 0 {
		delete(h.users, userID)
	}
	if len(h.rooms[roomID]) == 0 {
		delete(h.rooms, roomID)
	}
	return removed
}

func (h *Hub) userInRoom(userID, roomID uuid.UUID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.userInRoomLocked(userID, roomID)
}

func (h *Hub) userInRoomLocked(userID, roomID uuid.UUID) bool {
	for c := range h.users[userID] {
		if c.roomID == roomID {
			return true
		}
	}
	return false
}

func marshalEvent(ev Event) ([]byte, bool) {
	msg, err := json.Marshal(ev)
	if err != nil {
		slog.Error("failed to marshal websocket event", "type", ev.Type, "error", err)
		return nil, false
	}
	return msg, true
}
