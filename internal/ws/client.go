package ws

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	// sendBuffer is how many outgoing messages may queue per connection. A
	// client that falls further behind is disconnected rather than allowed to
	// block broadcasts.
	sendBuffer = 64

	// maxClientMessageBytes bounds what a client may send; the server only
	// understands {"type":"ping"}.
	maxClientMessageBytes = 4096
)

// client is one WebSocket connection. Only writeLoop writes to conn.
type client struct {
	conn   *websocket.Conn
	roomID uuid.UUID
	userID uuid.UUID

	send chan []byte

	done        chan struct{}
	closeOnce   sync.Once
	closeCode   websocket.StatusCode
	closeReason string
}

func newClient(conn *websocket.Conn, roomID, userID uuid.UUID) *client {
	conn.SetReadLimit(maxClientMessageBytes)
	return &client{
		conn:   conn,
		roomID: roomID,
		userID: userID,
		send:   make(chan []byte, sendBuffer),
		done:   make(chan struct{}),
	}
}

// enqueue never blocks: a full buffer means the client is too slow and gets
// disconnected.
func (c *client) enqueue(msg []byte) {
	select {
	case c.send <- msg:
	default:
		c.close(websocket.StatusPolicyViolation, "client too slow")
	}
}

// close asks writeLoop to send a close frame and stop. Safe to call many
// times and from any goroutine; the first call wins.
func (c *client) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.closeCode = code
		c.closeReason = reason
		close(c.done)
	})
}

// serve blocks until the connection is gone: closed by the peer, dropped,
// unresponsive to pings, or closed by the server.
func (c *client) serve(ctx context.Context, pingInterval, writeTimeout time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		c.readLoop(ctx)
		c.close(websocket.StatusNormalClosure, "")
	}()

	c.writeLoop(ctx, pingInterval, writeTimeout)
}

func (c *client) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "ping" {
			continue
		}
		if pong, ok := marshalEvent(NewEvent(TypePong, c.roomID, nil)); ok {
			c.enqueue(pong)
		}
	}
}

func (c *client) writeLoop(ctx context.Context, pingInterval, writeTimeout time.Duration) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			_ = c.conn.Close(c.closeCode, c.closeReason)
			return

		case msg := <-c.send:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				_ = c.conn.CloseNow()
				return
			}

		case <-ticker.C:
			// Ping waits for the pong, so a peer that vanished without a
			// close frame is detected within pingInterval+writeTimeout.
			pingCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				_ = c.conn.CloseNow()
				return
			}
		}
	}
}
