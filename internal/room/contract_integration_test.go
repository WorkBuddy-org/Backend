package room_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/ws"
)

func TestComputeStatus(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	tests := []struct {
		name  string
		room  room.Room
		count int
		want  string
	}{
		{"focus with free places", room.Room{Type: room.TypeFocus, Capacity: 4}, 1, room.StatusLive},
		{"discussion with free places", room.Room{Type: room.TypeDiscussion, Capacity: 4}, 1, room.StatusOpen},
		{"empty focus", room.Room{Type: room.TypeFocus, Capacity: 4}, 0, room.StatusLive},
		{"full focus", room.Room{Type: room.TypeFocus, Capacity: 4}, 4, room.StatusFull},
		{"full discussion", room.Room{Type: room.TypeDiscussion, Capacity: 4}, 4, room.StatusFull},
		{"over capacity", room.Room{Type: room.TypeFocus, Capacity: 4}, 5, room.StatusFull},
		{"ended", room.Room{Type: room.TypeFocus, Capacity: 4, EndsAt: &past}, 0, room.StatusEnded},
		{"ended beats full", room.Room{Type: room.TypeFocus, Capacity: 1, EndsAt: &past}, 1, room.StatusEnded},
		{"ended beats starting soon", room.Room{Type: room.TypeFocus, Capacity: 4, StartsAt: &future, EndsAt: &past}, 0, room.StatusEnded},
		{"starting soon", room.Room{Type: room.TypeFocus, Capacity: 4, StartsAt: &future}, 0, room.StatusStartingSoon},
		{"starting soon beats full", room.Room{Type: room.TypeFocus, Capacity: 1, StartsAt: &future}, 1, room.StatusStartingSoon},
		{"started, not ended", room.Room{Type: room.TypeFocus, Capacity: 4, StartsAt: &past, EndsAt: &future}, 1, room.StatusLive},
		{"started discussion", room.Room{Type: room.TypeDiscussion, Capacity: 4, StartsAt: &past}, 1, room.StatusOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := room.ComputeStatus(&tt.room, tt.count, now); got != tt.want {
				t.Fatalf("expected %s, got %s", tt.want, got)
			}
		})
	}
}

// createContractRoom inserts a room with the given columns and removes it at
// the end of the test.
func (e *roomEnv) createContractRoom(roomType string, capacity int, startsAt, endsAt *time.Time) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := e.db.QueryRow(ctx, `
		INSERT INTO rooms (name, type, capacity, starts_at, ends_at, tags)
		VALUES ($1, $2, $3, $4, $5, ARRAY['contract'])
		RETURNING id
	`, "Contract Room "+uuid.NewString(), roomType, capacity, startsAt, endsAt).Scan(&id)
	if err != nil {
		e.t.Fatalf("create room: %v", err)
	}
	e.t.Cleanup(func() { _, _ = e.db.Exec(context.Background(), "DELETE FROM rooms WHERE id = $1", id) })
	return id
}

type roomResponse struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	Tags         []string
	Participants []struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		AvatarURL *string `json:"avatar_url"`
	} `json:"participants"`
	ParticipantsCount int        `json:"participants_count"`
	Capacity          int        `json:"capacity"`
	StartsAt          *time.Time `json:"starts_at"`
	EndsAt            *time.Time `json:"ends_at"`
	ReservedByMe      *bool      `json:"reserved_by_me"`
}

func (e *roomEnv) getRoom(id uuid.UUID, token string) roomResponse {
	e.t.Helper()
	code, body := e.do(http.MethodGet, "/rooms/"+id.String(), token)
	if code != http.StatusOK {
		e.t.Fatalf("get room: expected 200, got %d: %s", code, body)
	}
	var out roomResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatalf("decode room %s: %v", body, err)
	}
	return out
}

func TestJoinUnderConcurrencyNeverOverbooks(t *testing.T) {
	e := newRoomEnv(t)
	const capacity, joiners = 3, 10
	roomID := e.createContractRoom(room.TypeFocus, capacity, nil, nil)

	tokens := make([]string, joiners)
	for i := range tokens {
		_, tokens[i] = e.newUser("Joiner")
	}

	type result struct {
		code int
		body string
	}
	results := make([]result, joiners)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, body := e.do(http.MethodPost, "/rooms/"+roomID.String()+"/join", tokens[i])
			results[i] = result{code, body}
		}()
	}
	close(start)
	wg.Wait()

	joined, full := 0, 0
	for _, r := range results {
		switch {
		case r.code == http.StatusOK:
			joined++
		case r.code == http.StatusConflict && strings.Contains(r.body, `"code":"ROOM_FULL"`):
			full++
		default:
			t.Fatalf("unexpected join result %d: %s", r.code, r.body)
		}
	}
	if joined != capacity || full != joiners-capacity {
		t.Fatalf("expected %d joined and %d ROOM_FULL, got %d and %d", capacity, joiners-capacity, joined, full)
	}

	var open int
	err := e.db.QueryRow(context.Background(),
		`SELECT count(*) FROM room_participations WHERE room_id = $1 AND left_at IS NULL`, roomID).Scan(&open)
	if err != nil || open != capacity {
		t.Fatalf("expected %d open participations, got %d (%v)", capacity, open, err)
	}

	got := e.getRoom(roomID, tokens[0])
	if got.Status != room.StatusFull || got.ParticipantsCount != capacity || len(got.Participants) != capacity {
		t.Fatalf("expected a full room with %d participants, got %+v", capacity, got)
	}
}

func TestRejoinIsIdempotentAndDoesNotCountTwice(t *testing.T) {
	e := newRoomEnv(t)
	roomID := e.createContractRoom(room.TypeFocus, 1, nil, nil)
	_, token := e.newUser("Only One")
	_, otherToken := e.newUser("Too Late")
	path := "/rooms/" + roomID.String() + "/join"

	if code, body := e.do(http.MethodPost, path, token); code != http.StatusOK {
		t.Fatalf("join: expected 200, got %d: %s", code, body)
	}
	// The room is full now, but a rejoin is not a new participant.
	if code, body := e.do(http.MethodPost, path, token); code != http.StatusOK {
		t.Fatalf("rejoin: expected 200, got %d: %s", code, body)
	}
	if got := e.getRoom(roomID, token); got.ParticipantsCount != 1 {
		t.Fatalf("rejoin must not count twice, got %d", got.ParticipantsCount)
	}
	if code, body := e.do(http.MethodPost, path, otherToken); code != http.StatusConflict || !strings.Contains(body, `"code":"ROOM_FULL"`) {
		t.Fatalf("expected 409 ROOM_FULL, got %d: %s", code, body)
	}

	// Leaving frees the place.
	if code, body := e.do(http.MethodPost, "/rooms/"+roomID.String()+"/leave", token); code != http.StatusNoContent {
		t.Fatalf("leave: expected 204, got %d: %s", code, body)
	}
	if code, body := e.do(http.MethodPost, path, otherToken); code != http.StatusOK {
		t.Fatalf("join after leave: expected 200, got %d: %s", code, body)
	}
}

func TestJoinEndedRoom(t *testing.T) {
	e := newRoomEnv(t)
	past := time.Now().Add(-time.Hour)
	roomID := e.createContractRoom(room.TypeFocus, 4, nil, &past)
	_, token := e.newUser("Late")

	code, body := e.do(http.MethodPost, "/rooms/"+roomID.String()+"/join", token)
	if code != http.StatusConflict || !strings.Contains(body, `"code":"ROOM_ENDED"`) {
		t.Fatalf("expected 409 ROOM_ENDED, got %d: %s", code, body)
	}
}

func TestListRoomsFilterAndShape(t *testing.T) {
	e := newRoomEnv(t)
	_, token := e.newUser("Lister")
	focus := e.createContractRoom(room.TypeFocus, 5, nil, nil)
	discussion := e.createContractRoom(room.TypeDiscussion, 5, nil, nil)

	// More participants than the preview holds.
	for i := 0; i < room.MaxPreviewParticipants+1; i++ {
		_, tok := e.newUser("Participant")
		if code, body := e.do(http.MethodPost, "/rooms/"+focus.String()+"/join", tok); code != http.StatusOK {
			t.Fatalf("join: expected 200, got %d: %s", code, body)
		}
	}

	list := func(query string) map[string]roomResponse {
		t.Helper()
		code, body := e.do(http.MethodGet, "/rooms"+query, token)
		if code != http.StatusOK {
			t.Fatalf("list %q: expected 200, got %d: %s", query, code, body)
		}
		var out struct {
			Items []roomResponse `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode list %s: %v", body, err)
		}
		byID := make(map[string]roomResponse, len(out.Items))
		for _, r := range out.Items {
			byID[r.ID] = r
		}
		return byID
	}

	for _, query := range []string{"", "?type=all"} {
		all := list(query)
		if _, ok := all[focus.String()]; !ok {
			t.Fatalf("%q: expected the focus room", query)
		}
		if _, ok := all[discussion.String()]; !ok {
			t.Fatalf("%q: expected the discussion room", query)
		}
	}

	onlyFocus := list("?type=focus")
	if _, ok := onlyFocus[discussion.String()]; ok {
		t.Fatal("type=focus must not return discussion rooms")
	}
	got, ok := onlyFocus[focus.String()]
	if !ok {
		t.Fatal("type=focus must return the focus room")
	}
	if got.Status != room.StatusFull || got.Type != room.TypeFocus || got.ParticipantsCount != 5 ||
		len(got.Participants) != room.MaxPreviewParticipants || got.Capacity != 5 || got.Title == "" ||
		got.ReservedByMe == nil || *got.ReservedByMe {
		t.Fatalf("unexpected room %+v", got)
	}

	onlyDiscussion := list("?type=discussion")
	if _, ok := onlyDiscussion[focus.String()]; ok {
		t.Fatal("type=discussion must not return focus rooms")
	}
	if d := onlyDiscussion[discussion.String()]; d.Status != room.StatusOpen || d.Participants == nil {
		t.Fatalf("expected an open discussion room with an empty participant array, got %+v", d)
	}

	code, body := e.do(http.MethodGet, "/rooms?type=nope", token)
	if code != http.StatusBadRequest || !strings.Contains(body, `"type":"INVALID_VALUE"`) {
		t.Fatalf("expected 400 for an unknown type, got %d: %s", code, body)
	}
}

func TestSeedRoomsIsIdempotent(t *testing.T) {
	e := newRoomEnv(t)
	ctx := context.Background()

	titles := make([]string, len(room.SeedData))
	for i, rm := range room.SeedData {
		titles[i] = rm.Title
		if rm.Capacity < 4 || rm.Capacity > 8 {
			t.Fatalf("%s: capacity %d is outside 4-8", rm.Title, rm.Capacity)
		}
	}
	count := func() int {
		var n int
		if err := e.db.QueryRow(ctx, `SELECT count(*) FROM rooms WHERE name = ANY($1)`, titles).Scan(&n); err != nil {
			t.Fatalf("count seeded rooms: %v", err)
		}
		return n
	}

	if _, err := room.SeedRooms(ctx, e.db); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if got := count(); got != len(room.SeedData) {
		t.Fatalf("expected %d seeded rooms, got %d", len(room.SeedData), got)
	}
	inserted, err := room.SeedRooms(ctx, e.db)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if inserted != 0 || count() != len(room.SeedData) {
		t.Fatalf("second seed must change nothing, inserted %d, count %d", inserted, count())
	}
}

// recordingPublisher collects published events.
type recordingPublisher struct {
	mu     sync.Mutex
	events []ws.Event
}

func (p *recordingPublisher) Broadcast(_ uuid.UUID, ev ws.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}

func (p *recordingPublisher) take() []ws.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.events
	p.events = nil
	return out
}

func TestRoomUpdatedEvents(t *testing.T) {
	e := newRoomEnv(t)
	pub := &recordingPublisher{}
	e.rooms.SetEventPublisher(pub)
	roomID := e.createContractRoom(room.TypeFocus, 2, nil, nil)
	aliceID, aliceToken := e.newUser("Alice")
	_, bobToken := e.newUser("Bob")
	joinPath := "/rooms/" + roomID.String() + "/join"

	expect := func(step, status string, count int) {
		t.Helper()
		events := pub.take()
		if len(events) != 1 || events[0].Type != ws.TypeRoomUpdated || events[0].RoomID != roomID {
			t.Fatalf("%s: expected one room_updated, got %+v", step, events)
		}
		data, ok := events[0].Data.(ws.RoomUpdatedData)
		if !ok || data.RoomID != roomID || data.Status != status || data.ParticipantsCount != count || data.Capacity != 2 {
			t.Fatalf("%s: unexpected payload %+v", step, events[0].Data)
		}
	}

	e.do(http.MethodPost, joinPath, aliceToken)
	expect("first join", room.StatusLive, 1)

	e.do(http.MethodPost, joinPath, aliceToken)
	if events := pub.take(); len(events) != 0 {
		t.Fatalf("an idempotent rejoin must publish nothing, got %+v", events)
	}

	e.do(http.MethodPost, joinPath, bobToken)
	expect("second join", room.StatusFull, 2)

	e.do(http.MethodPost, "/rooms/"+roomID.String()+"/leave", bobToken)
	expect("leave", room.StatusLive, 1)

	if err := e.rooms.KickNoExtension(context.Background(), roomID, aliceID); err != nil {
		t.Fatalf("kick: %v", err)
	}
	expect("kick", room.StatusLive, 0)
}
