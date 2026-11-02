package ws_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/user"
	"github.com/workbuddy/backend/internal/ws"
)

const allowedOrigin = "https://app.example.com"

func TestWebSocketHandshakeRejections(t *testing.T) {
	e := newTestEnv(t, ws.Config{AllowedOrigins: []string{allowedOrigin}})

	roomID := e.createRoom()
	member, memberToken := e.createUser("Member")
	e.joinREST(roomID, memberToken)
	_, outsiderToken := e.createUser("Outsider")

	otherSecret := auth.NewTokenManager("some-other-secret", time.Minute)
	forged, err := otherSecret.IssueAccessToken(member.ID, member.Email)
	if err != nil {
		t.Fatalf("issue forged token: %v", err)
	}

	cases := []struct {
		name   string
		path   string
		token  string
		origin string
		want   int
	}{
		{"invalid room id", "/rooms/not-a-uuid/ws", memberToken, "", http.StatusBadRequest},
		{"missing token", "/rooms/" + roomID.String() + "/ws", "", "", http.StatusUnauthorized},
		{"garbage token", "/rooms/" + roomID.String() + "/ws", "garbage", "", http.StatusUnauthorized},
		{"token signed with another secret", "/rooms/" + roomID.String() + "/ws", forged, "", http.StatusUnauthorized},
		{"no open participation", "/rooms/" + roomID.String() + "/ws", outsiderToken, "", http.StatusForbidden},
		{"room does not exist", "/rooms/" + uuid.NewString() + "/ws", memberToken, "", http.StatusForbidden},
		{"origin not allowed", "/rooms/" + roomID.String() + "/ws", memberToken, "https://evil.example.com", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, resp, err := e.dialPath(tc.path, tc.token, tc.origin)
			if err == nil {
				_ = conn.CloseNow()
				t.Fatalf("expected handshake to fail with %d, it succeeded", tc.want)
			}
			if resp == nil || resp.StatusCode != tc.want {
				t.Fatalf("expected status %d, got %+v (err %v)", tc.want, resp, err)
			}
		})
	}

	if got := e.presence(roomID); len(got) != 0 {
		t.Fatalf("rejected handshakes must not touch presence, got %v", got)
	}

	// An allowed origin passes, case- and trailing-slash-insensitively.
	conn, resp, err := e.dialPath("/rooms/"+roomID.String()+"/ws", memberToken, "https://APP.example.com/")
	if err != nil {
		t.Fatalf("expected allowed origin to connect: %v (%+v)", err, resp)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// Leaving the room via REST revokes the right to connect.
	e.leaveREST(roomID, memberToken)
	_, resp, err = e.dialPath("/rooms/"+roomID.String()+"/ws", memberToken, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 after leaving, got %+v (err %v)", resp, err)
	}
}

func TestWebSocketHandshakeWithCookie(t *testing.T) {
	e := newTestEnv(t, ws.Config{AllowedOrigins: []string{allowedOrigin}})

	roomID := e.createRoom()
	member, memberToken := e.createUser("Cookie Member")
	e.joinREST(roomID, memberToken)
	path := "/rooms/" + roomID.String() + "/ws"

	// Browsers send the access_token cookie on the upgrade; no query needed.
	conn, resp, err := e.dialCookie(path, memberToken, allowedOrigin)
	if err != nil {
		t.Fatalf("expected cookie handshake to connect: %v (%+v)", err, resp)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// The cookie wins over the query parameter.
	conn, resp, err = e.dialCookieAndQuery(path, memberToken, "garbage")
	if err != nil {
		t.Fatalf("expected cookie to take precedence over ?access_token=: %v (%+v)", err, resp)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	expired, err := auth.NewTokenManager(os.Getenv("JWT_SECRET"), -time.Minute).IssueAccessToken(member.ID, member.Email)
	if err != nil {
		t.Fatalf("issue expired token: %v", err)
	}
	if _, resp, err := e.dialCookie(path, expired, allowedOrigin); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an expired cookie, got %+v (err %v)", resp, err)
	}
}

func TestWebSocketPresenceSnapshotOnConnect(t *testing.T) {
	e := newTestEnv(t, ws.Config{})

	roomID := e.createRoom()
	alice, aliceToken := e.createUser("Alice")
	e.joinREST(roomID, aliceToken)

	conn := e.dial(roomID, aliceToken)
	ev := readEvent(t, conn)

	if ev.Type != ws.TypePresenceSnapshot {
		t.Fatalf("first event must be presence_snapshot, got %s", ev.Type)
	}
	if ev.RoomID != roomID {
		t.Fatalf("expected room_id %s, got %s", roomID, ev.RoomID)
	}
	if _, err := time.Parse(time.RFC3339, ev.TS); err != nil {
		t.Fatalf("ts %q is not RFC 3339: %v", ev.TS, err)
	}

	var data ws.PresenceSnapshotData
	decodeData(t, ev, &data)
	if len(data.Users) != 1 || data.Users[0].UserID != alice.ID || data.Users[0].Name != alice.Name {
		t.Fatalf("expected snapshot [%s %s], got %+v", alice.ID, alice.Name, data.Users)
	}

	if got := e.presence(roomID); !equalIDs(got, alice.ID) {
		t.Fatalf("expected redis presence {%s}, got %v", alice.ID, got)
	}
}

func TestWebSocketJoinAndLeaveBroadcast(t *testing.T) {
	e := newTestEnv(t, ws.Config{})

	roomID := e.createRoom()
	alice, aliceToken := e.createUser("Alice")
	bob, bobToken := e.createUser("Bob")
	e.joinREST(roomID, aliceToken)
	e.joinREST(roomID, bobToken)

	connA := e.dial(roomID, aliceToken)
	expectType(t, readEvent(t, connA), ws.TypePresenceSnapshot)

	connB := e.dial(roomID, bobToken)
	snapshot := readEvent(t, connB)
	expectType(t, snapshot, ws.TypePresenceSnapshot)
	var snapData ws.PresenceSnapshotData
	decodeData(t, snapshot, &snapData)
	if len(snapData.Users) != 2 || snapData.Users[0].UserID != alice.ID || snapData.Users[1].UserID != bob.ID {
		t.Fatalf("expected snapshot [Alice, Bob], got %+v", snapData.Users)
	}

	joined := readEvent(t, connA)
	expectType(t, joined, ws.TypeUserJoined)
	var joinedData ws.UserJoinedData
	decodeData(t, joined, &joinedData)
	if joinedData.UserID != bob.ID || joinedData.Name != "Bob" {
		t.Fatalf("expected user_joined Bob, got %+v", joinedData)
	}

	if err := connB.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close bob: %v", err)
	}

	expectUserLeft(t, readEvent(t, connA), bob.ID)
	if got := e.presence(roomID); !equalIDs(got, alice.ID) {
		t.Fatalf("expected only alice in presence, got %v", got)
	}
}

func TestWebSocketPresenceCleanupOnAbruptClose(t *testing.T) {
	t.Run("connection dropped without close frame", func(t *testing.T) {
		e := newTestEnv(t, ws.Config{})
		roomID := e.createRoom()
		alice, aliceToken := e.createUser("Alice")
		bob, bobToken := e.createUser("Bob")
		e.joinREST(roomID, aliceToken)
		e.joinREST(roomID, bobToken)

		connA := e.dial(roomID, aliceToken)
		expectType(t, readEvent(t, connA), ws.TypePresenceSnapshot)
		connB := e.dial(roomID, bobToken)
		expectType(t, readEvent(t, connB), ws.TypePresenceSnapshot)
		expectType(t, readEvent(t, connA), ws.TypeUserJoined)

		_ = connB.CloseNow() // TCP close, no WebSocket close handshake

		expectUserLeft(t, readEvent(t, connA), bob.ID)
		if got := e.presence(roomID); !equalIDs(got, alice.ID) {
			t.Fatalf("expected only alice in presence, got %v", got)
		}
	})

	t.Run("peer stops answering pings", func(t *testing.T) {
		e := newTestEnv(t, ws.Config{PingInterval: 100 * time.Millisecond, WriteTimeout: 200 * time.Millisecond})
		roomID := e.createRoom()
		alice, aliceToken := e.createUser("Alice")
		bob, bobToken := e.createUser("Bob")
		e.joinREST(roomID, aliceToken)
		e.joinREST(roomID, bobToken)

		connA := e.dial(roomID, aliceToken)
		expectType(t, readEvent(t, connA), ws.TypePresenceSnapshot)
		connB := e.dial(roomID, bobToken)
		expectType(t, readEvent(t, connB), ws.TypePresenceSnapshot)
		expectType(t, readEvent(t, connA), ws.TypeUserJoined)

		// Bob never reads again, so his client never answers pings; the
		// server must notice and drop him. Alice keeps reading (and ponging).
		expectUserLeft(t, readEvent(t, connA), bob.ID)
		if got := e.presence(roomID); !equalIDs(got, alice.ID) {
			t.Fatalf("expected only alice in presence, got %v", got)
		}
	})
}

func TestWebSocketMultipleTabs(t *testing.T) {
	e := newTestEnv(t, ws.Config{})
	roomID := e.createRoom()
	observer, observerToken := e.createUser("Observer")
	alice, aliceToken := e.createUser("Alice")
	marker, markerToken := e.createUser("Marker")
	for _, token := range []string{observerToken, aliceToken, markerToken} {
		e.joinREST(roomID, token)
	}

	connO := e.dial(roomID, observerToken)
	expectType(t, readEvent(t, connO), ws.TypePresenceSnapshot)

	tab1 := e.dial(roomID, aliceToken)
	expectType(t, readEvent(t, tab1), ws.TypePresenceSnapshot)
	expectJoined(t, readEvent(t, connO), alice.ID)

	tab2 := e.dial(roomID, aliceToken)
	expectType(t, readEvent(t, tab2), ws.TypePresenceSnapshot)

	// The marker user's join must be the observer's very next event, proving
	// the second tab did not emit user_joined.
	connM := e.dial(roomID, markerToken)
	expectType(t, readEvent(t, connM), ws.TypePresenceSnapshot)
	expectJoined(t, readEvent(t, connO), marker.ID)

	if err := tab1.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close tab1: %v", err)
	}
	e.waitForConnections(roomID, 3)
	if got := e.presence(roomID); !equalIDs(got, observer.ID, alice.ID, marker.ID) {
		t.Fatalf("alice still has a tab open and must stay present, got %v", got)
	}

	// Likewise, closing one of two tabs must not emit user_left.
	if err := connM.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close marker: %v", err)
	}
	expectUserLeft(t, readEvent(t, connO), marker.ID)

	if err := tab2.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close tab2: %v", err)
	}
	expectUserLeft(t, readEvent(t, connO), alice.ID)
	if got := e.presence(roomID); !equalIDs(got, observer.ID) {
		t.Fatalf("expected only observer in presence, got %v", got)
	}
}

func TestWebSocketRESTLeaveClosesSocketsAndBroadcastsOnce(t *testing.T) {
	e := newTestEnv(t, ws.Config{})
	roomID := e.createRoom()
	otherRoomID := e.createRoom()
	observer, observerToken := e.createUser("Observer")
	alice, aliceToken := e.createUser("Alice")
	marker, markerToken := e.createUser("Marker")
	e.joinREST(roomID, observerToken)
	e.joinREST(roomID, aliceToken)
	e.joinREST(roomID, markerToken)
	e.joinREST(otherRoomID, aliceToken)

	connO := e.dial(roomID, observerToken)
	expectType(t, readEvent(t, connO), ws.TypePresenceSnapshot)
	tab1 := e.dial(roomID, aliceToken)
	expectType(t, readEvent(t, tab1), ws.TypePresenceSnapshot)
	expectJoined(t, readEvent(t, connO), alice.ID)
	tab2 := e.dial(roomID, aliceToken)
	expectType(t, readEvent(t, tab2), ws.TypePresenceSnapshot)
	elsewhere := e.dial(otherRoomID, aliceToken)
	expectType(t, readEvent(t, elsewhere), ws.TypePresenceSnapshot)

	e.leaveREST(roomID, aliceToken)

	// Both tabs are closed by the server with a normal closure.
	for i, conn := range []*websocket.Conn{tab1, tab2} {
		if code := expectClosed(t, conn); code != websocket.StatusNormalClosure {
			t.Fatalf("tab%d: expected close 1000, got %d", i+1, code)
		}
	}

	// Exactly one user_left: the marker's join is the very next event.
	expectUserLeftReason(t, readEvent(t, connO), alice.ID, room.LeftReasonVoluntary)
	connM := e.dial(roomID, markerToken)
	expectType(t, readEvent(t, connM), ws.TypePresenceSnapshot)
	expectJoined(t, readEvent(t, connO), marker.ID)

	e.waitForConnections(roomID, 2)
	if got := e.presence(roomID); !equalIDs(got, observer.ID, marker.ID) {
		t.Fatalf("expected only observer and marker in presence, got %v", got)
	}

	// Her socket to another room is untouched.
	if err := elsewhere.Write(context.Background(), websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("ping other room: %v", err)
	}
	expectType(t, readEvent(t, elsewhere), ws.TypePong)
	if got := e.presence(otherRoomID); !equalIDs(got, alice.ID) {
		t.Fatalf("expected alice still present in the other room, got %v", got)
	}
}

func TestWebSocketKickSendsKickedThenCloses(t *testing.T) {
	testKickSendsKickedThenCloses(t, (*room.Service).Kick, ws.LeftReasonKicked)
}

// The extension-prompt kick records a distinct reason but closes sockets and
// clears presence exactly like any other kick.
func TestWebSocketKickNoExtensionSendsKickedThenCloses(t *testing.T) {
	testKickSendsKickedThenCloses(t, (*room.Service).KickNoExtension, ws.LeftReasonKickedNoExtension)
}

func testKickSendsKickedThenCloses(t *testing.T, kick func(*room.Service, context.Context, uuid.UUID, uuid.UUID) error, reason string) {
	t.Helper()
	e := newTestEnv(t, ws.Config{})
	roomID := e.createRoom()
	observer, observerToken := e.createUser("Observer")
	alice, aliceToken := e.createUser("Alice")
	marker, markerToken := e.createUser("Marker")
	for _, token := range []string{observerToken, aliceToken, markerToken} {
		e.joinREST(roomID, token)
	}

	connO := e.dial(roomID, observerToken)
	expectType(t, readEvent(t, connO), ws.TypePresenceSnapshot)
	tab1 := e.dial(roomID, aliceToken)
	expectType(t, readEvent(t, tab1), ws.TypePresenceSnapshot)
	expectJoined(t, readEvent(t, connO), alice.ID)
	tab2 := e.dial(roomID, aliceToken)
	expectType(t, readEvent(t, tab2), ws.TypePresenceSnapshot)

	if err := kick(e.rooms, context.Background(), roomID, alice.ID); err != nil {
		t.Fatalf("kick: %v", err)
	}

	for i, conn := range []*websocket.Conn{tab1, tab2} {
		ev := readEvent(t, conn)
		expectType(t, ev, ws.TypeKicked)
		var data ws.KickedData
		decodeData(t, ev, &data)
		if data.Reason != reason {
			t.Fatalf("tab%d: expected kicked reason %q, got %+v", i+1, reason, data)
		}
		if code := expectClosed(t, conn); code != ws.StatusRemovedFromRoom {
			t.Fatalf("tab%d: expected close %d, got %d", i+1, ws.StatusRemovedFromRoom, code)
		}
	}

	// The room gets exactly one user_left; the marker's join comes next.
	expectUserLeftReason(t, readEvent(t, connO), alice.ID, reason)
	connM := e.dial(roomID, markerToken)
	expectType(t, readEvent(t, connM), ws.TypePresenceSnapshot)
	expectJoined(t, readEvent(t, connO), marker.ID)

	if got := e.presence(roomID); !equalIDs(got, observer.ID, marker.ID) {
		t.Fatalf("expected alice removed from presence, got %v", got)
	}

	// A kicked user cannot reconnect without joining again.
	_, resp, err := e.dialPath("/rooms/"+roomID.String()+"/ws", aliceToken, "")
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 after kick, got %+v (err %v)", resp, err)
	}
}

func TestWebSocketPingPongIgnoresOtherMessagesAndOutlivesServerTimeouts(t *testing.T) {
	e := newTestEnv(t, ws.Config{})
	roomID := e.createRoom()
	_, token := e.createUser("Alice")
	e.joinREST(roomID, token)

	conn := e.dial(roomID, token)
	expectType(t, readEvent(t, conn), ws.TypePresenceSnapshot)

	// Longer than the test server's ReadTimeout/WriteTimeout: the socket
	// must not inherit those deadlines.
	time.Sleep(testServerTimeout + 500*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, msg := range []string{`not json`, `{"type":"chat_message","text":"hi"}`, `{"type":"pong"}`} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02}); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	pong := readEvent(t, conn)
	expectType(t, pong, ws.TypePong)
	if pong.RoomID != roomID {
		t.Fatalf("expected pong room_id %s, got %s", roomID, pong.RoomID)
	}
}

func TestWebSocketSendToUserTargetsOnlyThatUserAndRoom(t *testing.T) {
	e := newTestEnv(t, ws.Config{})
	room1 := e.createRoom()
	room2 := e.createRoom()
	alice, aliceToken := e.createUser("Alice")
	_, bobToken := e.createUser("Bob")
	e.joinREST(room1, bobToken)
	e.joinREST(room1, aliceToken)
	e.joinREST(room2, aliceToken)

	connB := e.dial(room1, bobToken)
	expectType(t, readEvent(t, connB), ws.TypePresenceSnapshot)
	connA1 := e.dial(room1, aliceToken)
	expectType(t, readEvent(t, connA1), ws.TypePresenceSnapshot)
	expectJoined(t, readEvent(t, connB), alice.ID)
	connA2 := e.dial(room2, aliceToken)
	expectType(t, readEvent(t, connA2), ws.TypePresenceSnapshot)

	e.hub.SendToUser(alice.ID, ws.NewEvent(ws.TypeKicked, room1, map[string]string{"reason": "test"}))
	e.hub.Broadcast(room1, ws.NewEvent("marker", room1, nil))
	e.hub.Broadcast(room2, ws.NewEvent("marker", room2, nil))

	expectType(t, readEvent(t, connA1), ws.TypeKicked)
	expectType(t, readEvent(t, connA1), "marker")
	expectType(t, readEvent(t, connA2), "marker") // other room: not targeted
	expectType(t, readEvent(t, connB), "marker")  // other user: not targeted
}

func TestPresenceResetAllClearsStaleSets(t *testing.T) {
	e := newTestEnv(t, ws.Config{})
	ctx := context.Background()

	key := "room:" + uuid.NewString() + ":presence"
	if err := e.redis.SAdd(ctx, key, uuid.NewString()).Err(); err != nil {
		t.Fatalf("seed presence: %v", err)
	}

	if _, err := ws.NewPresenceStore(e.redis, auth.NewRepository(e.db)).ResetAll(ctx); err != nil {
		t.Fatalf("reset presence: %v", err)
	}
	if n, err := e.redis.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("expected stale presence key to be deleted, exists=%d err=%v", n, err)
	}
}

// testServerTimeout mirrors server.New's ReadTimeout/WriteTimeout, shortened
// so a test can outlast it.
const testServerTimeout = time.Second

type testEnv struct {
	t        *testing.T
	db       *pgxpool.Pool
	redis    *goredis.Client
	tokens   *auth.TokenManager
	hub      *ws.Hub
	rooms    *room.Service
	server   *httptest.Server
	presence func(roomID uuid.UUID) []string
}

func newTestEnv(t *testing.T, wsCfg ws.Config) *testEnv {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}

	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	db, err := postgres.NewPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(db.Close)

	redisClient, err := redisplatform.NewClient(cfg.RedisURL)
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}
	t.Cleanup(func() { _ = redisClient.Close() })

	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	authRepo := auth.NewRepository(db)
	authService := auth.NewService(authRepo, tokenManager, auth.NewOAuthRegistry(auth.OAuthConfig{}), cfg.JWTRefreshTTL, cfg.FrontendCallbackURL, redisClient)
	authMW := auth.NewMiddleware(tokenManager)
	roomService := room.NewService(room.NewRepository(db))

	hub := ws.NewHub()
	wsHandler := ws.NewHandler(ws.HandlerDeps{
		Hub:      hub,
		Presence: ws.NewPresenceStore(redisClient, authRepo),
		Tokens:   tokenManager,
		Rooms:    roomService,
		Config:   wsCfg,
	})
	roomService.SetLeaveListener(wsHandler)

	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthHandler: auth.NewHandler(authService),
		AuthMW:      authMW,
		RoomHandler: room.NewHandler(roomService),
		WSHandler:   wsHandler,
	})

	server := httptest.NewUnstartedServer(router)
	server.Config.ReadTimeout = testServerTimeout
	server.Config.WriteTimeout = testServerTimeout
	server.Start()
	t.Cleanup(func() {
		hub.CloseAll()
		server.Close()
	})

	e := &testEnv{t: t, db: db, redis: redisClient, tokens: tokenManager, hub: hub, rooms: roomService, server: server}
	e.presence = func(roomID uuid.UUID) []string {
		members, err := redisClient.SMembers(context.Background(), "room:"+roomID.String()+":presence").Result()
		if err != nil {
			t.Fatalf("read presence: %v", err)
		}
		return members
	}
	return e
}

func (e *testEnv) createUser(name string) (*user.User, string) {
	e.t.Helper()

	email := "ws-" + strings.ToLower(name) + "-" + uuid.NewString() + "@example.com"
	u, err := auth.NewRepository(e.db).CreateUser(context.Background(), &auth.OAuthProfile{
		Provider: user.OAuthProviderGoogle,
		OAuthID:  email,
		Email:    email,
		Name:     name,
	})
	if err != nil {
		e.t.Fatalf("create test user: %v", err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.db.Exec(ctx, "DELETE FROM users WHERE id = $1", u.ID)
	})

	token, err := e.tokens.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		e.t.Fatalf("issue access token: %v", err)
	}
	return u, token
}

func (e *testEnv) createRoom() uuid.UUID {
	e.t.Helper()

	ctx := context.Background()
	var id uuid.UUID
	err := e.db.QueryRow(ctx, `INSERT INTO rooms (name, type) VALUES ($1, 'focus') RETURNING id`, "WS Room "+uuid.NewString()).Scan(&id)
	if err != nil {
		e.t.Fatalf("create test room: %v", err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.redis.Del(ctx, "room:"+id.String()+":presence").Err()
		_, _ = e.db.Exec(ctx, "DELETE FROM rooms WHERE id = $1", id)
	})
	return id
}

func (e *testEnv) joinREST(roomID uuid.UUID, token string) {
	e.t.Helper()
	e.postREST("/rooms/"+roomID.String()+"/join", token, http.StatusOK)
}

func (e *testEnv) leaveREST(roomID uuid.UUID, token string) {
	e.t.Helper()
	e.postREST("/rooms/"+roomID.String()+"/leave", token, http.StatusNoContent)
}

func (e *testEnv) postREST(path, token string, want int) {
	e.t.Helper()

	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, nil)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		e.t.Fatalf("POST %s: expected %d, got %d", path, want, resp.StatusCode)
	}
}

func (e *testEnv) dialPath(path, token, origin string) (*websocket.Conn, *http.Response, error) {
	url := "ws" + strings.TrimPrefix(e.server.URL, "http") + path
	if token != "" {
		url += "?access_token=" + token
	}
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if origin != "" {
		opts.HTTPHeader.Set("Origin", origin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, opts)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return conn, resp, err
}

func (e *testEnv) dial(roomID uuid.UUID, token string) *websocket.Conn {
	e.t.Helper()

	conn, resp, err := e.dialPath("/rooms/"+roomID.String()+"/ws", token, "")
	if err != nil {
		e.t.Fatalf("dial ws: %v (%+v)", err, resp)
	}
	e.t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func (e *testEnv) waitForConnections(roomID uuid.UUID, want int) {
	e.t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.hub.RoomConnections(roomID) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("expected %d connections in room, have %d", want, e.hub.RoomConnections(roomID))
}

type testEvent struct {
	Type   string          `json:"type"`
	RoomID uuid.UUID       `json:"room_id"`
	TS     string          `json:"ts"`
	Data   json.RawMessage `json:"data"`
}

// readEvent fails the test if no message arrives within 5s. Note that a
// timed-out Read closes a coder/websocket connection, so tests never use
// short reads to assert absence; they use marker events instead.
func readEvent(t *testing.T, conn *websocket.Conn) testEvent {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read ws message: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("expected text message, got %v", typ)
	}

	var ev testEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode ws message %s: %v", data, err)
	}
	return ev
}

func decodeData(t *testing.T, ev testEvent, dst any) {
	t.Helper()
	if err := json.Unmarshal(ev.Data, dst); err != nil {
		t.Fatalf("decode %s data %s: %v", ev.Type, ev.Data, err)
	}
}

func expectType(t *testing.T, ev testEvent, want string) {
	t.Helper()
	if ev.Type != want {
		t.Fatalf("expected %s event, got %s: %s", want, ev.Type, ev.Data)
	}
}

func expectJoined(t *testing.T, ev testEvent, userID uuid.UUID) {
	t.Helper()
	expectType(t, ev, ws.TypeUserJoined)
	var data ws.UserJoinedData
	decodeData(t, ev, &data)
	if data.UserID != userID {
		t.Fatalf("expected user_joined %s, got %s", userID, data.UserID)
	}
}

func expectUserLeft(t *testing.T, ev testEvent, userID uuid.UUID) {
	t.Helper()
	expectUserLeftReason(t, ev, userID, ws.LeftReasonDisconnect)
}

func expectUserLeftReason(t *testing.T, ev testEvent, userID uuid.UUID, reason string) {
	t.Helper()
	expectType(t, ev, ws.TypeUserLeft)
	var data ws.UserLeftData
	decodeData(t, ev, &data)
	if data.UserID != userID || data.Reason != reason {
		t.Fatalf("expected user_left %s/%s, got %+v", userID, reason, data)
	}
}

// expectClosed reads until the connection is closed and returns the close
// status. Any data message before the close fails the test.
func expectClosed(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err == nil {
		t.Fatalf("expected connection to be closed, got message %s", data)
	}
	return websocket.CloseStatus(err)
}

func equalIDs(members []string, ids ...uuid.UUID) bool {
	want := make([]string, len(ids))
	for i, id := range ids {
		want[i] = id.String()
	}
	got := append([]string(nil), members...)
	sort.Strings(got)
	sort.Strings(want)
	return strings.Join(got, ",") == strings.Join(want, ",")
}

// dialCookie opens the socket with the token in the access_token cookie, as a
// browser does after cookie login.
func (e *testEnv) dialCookie(path, token, origin string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	h.Set("Cookie", (&http.Cookie{Name: auth.AccessTokenCookie, Value: token}).String())
	if origin != "" {
		h.Set("Origin", origin)
	}
	return e.dialURL("ws"+strings.TrimPrefix(e.server.URL, "http")+path, h)
}

// dialCookieAndQuery sends one token in the cookie and another in the query.
func (e *testEnv) dialCookieAndQuery(path, cookieToken, queryToken string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	h.Set("Cookie", (&http.Cookie{Name: auth.AccessTokenCookie, Value: cookieToken}).String())
	return e.dialURL("ws"+strings.TrimPrefix(e.server.URL, "http")+path+"?access_token="+queryToken, h)
}

func (e *testEnv) dialURL(url string, h http.Header) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return conn, resp, err
}
