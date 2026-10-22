package task_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/task"
	"github.com/workbuddy/backend/internal/ws"
)

func TestTimerStartValidation(t *testing.T) {
	e := newTimerEnv(t)
	owner, ownerToken := e.createUser()
	_, otherToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, owner)
	taskID := e.createTask(roomID, ownerToken)

	for _, body := range []string{
		`{"duration_minutes": 0}`,
		`{"duration_minutes": 30}`,
		`{"duration_minutes": 120}`,
		`{}`,
		`{"duration_minutes": 25, "extra": true}`,
		`not json`,
	} {
		if code, resp := e.do(http.MethodPost, "/tasks/"+taskID+"/start", ownerToken, body); code != http.StatusBadRequest {
			t.Fatalf("body %s: expected 400, got %d: %s", body, code, resp)
		}
	}

	if code, _ := e.do(http.MethodPost, "/tasks/"+uuid.NewString()+"/start", ownerToken, `{"duration_minutes": 25}`); code != http.StatusNotFound {
		t.Fatalf("unknown task: expected 404, got %d", code)
	}
	if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/start", otherToken, `{"duration_minutes": 25}`); code != http.StatusForbidden {
		t.Fatalf("non-owner: expected 403, got %d", code)
	}

	// A task in a room the owner has not joined (or has left) cannot start.
	otherRoomID := e.createRoom()
	outsideTaskID := e.createTask(otherRoomID, ownerToken)
	if code, _ := e.do(http.MethodPost, "/tasks/"+outsideTaskID+"/start", ownerToken, `{"duration_minutes": 25}`); code != http.StatusConflict {
		t.Fatalf("not in room: expected 409, got %d", code)
	}

	completedTaskID := e.createTask(roomID, ownerToken)
	e.complete(completedTaskID, ownerToken)
	if code, _ := e.do(http.MethodPost, "/tasks/"+completedTaskID+"/start", ownerToken, `{"duration_minutes": 25}`); code != http.StatusConflict {
		t.Fatalf("completed task: expected 409, got %d", code)
	}

	if len(e.events.all()) != 0 {
		t.Fatalf("rejected starts must not emit events, got %+v", e.events.all())
	}

	startedAt := e.clock.Now().UTC().Truncate(time.Millisecond)
	got := e.start(taskID, ownerToken, 50)
	wantEnds := startedAt.Add(50 * time.Minute)
	if got.TimerStatus != task.TimerRunning || got.TimerDurationMinutes == nil || *got.TimerDurationMinutes != 50 {
		t.Fatalf("expected running 50-minute timer, got %+v", got)
	}
	if got.TimerEndsAt == nil || !got.TimerEndsAt.Equal(wantEnds) {
		t.Fatalf("expected ends_at %s, got %v", wantEnds, got.TimerEndsAt)
	}

	score, err := e.redis.ZScore(context.Background(), e.timersKey, taskID).Result()
	if err != nil {
		t.Fatalf("task must be indexed in timers:due: %v", err)
	}
	if want := float64(wantEnds.UnixMilli()) / 1000; score != want {
		t.Fatalf("expected score %f, got %f", want, score)
	}

	ev := e.events.waitFor(t, ws.TypeTimerStarted, roomID)
	var data ws.TimerStartedData
	decodeEventData(t, ev, &data)
	if data.TaskID.String() != taskID || data.UserID != owner || data.DurationMinutes != 50 ||
		data.EndsAt != ws.FormatTime(wantEnds) || data.ServerTime == "" {
		t.Fatalf("unexpected timer_started payload %+v", data)
	}
}

func TestTimerOnlyOneActivePerUser(t *testing.T) {
	e := newTimerEnv(t)
	owner, ownerToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, owner)
	first := e.createTask(roomID, ownerToken)
	second := e.createTask(roomID, ownerToken)

	e.start(first, ownerToken, 25)

	for _, id := range []string{second, first} {
		if code, resp := e.do(http.MethodPost, "/tasks/"+id+"/start", ownerToken, `{"duration_minutes": 25}`); code != http.StatusConflict {
			t.Fatalf("second timer: expected 409, got %d: %s", code, resp)
		}
	}

	// Completing the task stops its timer and frees the slot.
	completed := e.complete(first, ownerToken)
	if completed.TimerStatus != task.TimerEnded {
		t.Fatalf("expected timer ended on completion, got %s", completed.TimerStatus)
	}
	if err := e.redis.ZScore(context.Background(), e.timersKey, first).Err(); err != goredis.Nil {
		t.Fatalf("completed task must leave timers:due, got %v", err)
	}
	ev := e.events.waitFor(t, ws.TypeTimerEnded, roomID)
	var ended ws.TimerEndedData
	decodeEventData(t, ev, &ended)
	if ended.TaskID.String() != first || ended.Reason != ws.TimerEndedCompleted {
		t.Fatalf("expected timer_ended completed for %s, got %+v", first, ended)
	}

	e.start(second, ownerToken, 90)
}

func TestTimerExpiry(t *testing.T) {
	e := newTimerEnv(t)
	owner, ownerToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, owner)
	taskID := e.createTask(roomID, ownerToken)

	scheduler := e.startScheduler()
	e.start(taskID, ownerToken, 25)

	// Not due yet: a few ticks must leave it running.
	e.clock.Advance(24 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if status := e.timerStatus(taskID); status != task.TimerRunning {
		t.Fatalf("expected running before ends_at, got %s", status)
	}

	e.clock.Advance(time.Minute + time.Second)
	ev := e.events.waitFor(t, ws.TypeTimerEnded, roomID)
	var ended ws.TimerEndedData
	decodeEventData(t, ev, &ended)
	if ended.TaskID.String() != taskID || ended.UserID != owner || ended.Reason != ws.TimerEndedExpired {
		t.Fatalf("unexpected timer_ended payload %+v", ended)
	}
	if status := e.timerStatus(taskID); status != task.TimerPrompting {
		t.Fatalf("expected prompting after expiry, got %s", status)
	}
	if err := e.redis.ZScore(context.Background(), e.timersKey, taskID).Err(); err != goredis.Nil {
		t.Fatalf("expired task must leave timers:due, got %v", err)
	}

	// Fired exactly once even though the scheduler keeps polling.
	time.Sleep(100 * time.Millisecond)
	scheduler.stop()
	if n := e.events.count(ws.TypeTimerEnded); n != 1 {
		t.Fatalf("expected one timer_ended, got %d", n)
	}
}

func TestTimerRecoveryAfterSchedulerRestart(t *testing.T) {
	e := newTimerEnv(t)
	alice, aliceToken := e.createUser()
	bob, bobToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, alice)
	e.join(roomID, bob)
	shortTask := e.createTask(roomID, aliceToken)
	longTask := e.createTask(roomID, bobToken)

	first := e.startScheduler()
	e.start(shortTask, aliceToken, 25)
	e.start(longTask, bobToken, 90)
	first.stop()

	// Lose the whole index, as if Redis was flushed while the server was down,
	// and let the short timer's end pass meanwhile.
	if err := e.redis.Del(context.Background(), e.timersKey).Err(); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	e.clock.Advance(30 * time.Minute)

	second := e.startScheduler()
	defer second.stop()

	ev := e.events.waitFor(t, ws.TypeTimerEnded, roomID)
	var ended ws.TimerEndedData
	decodeEventData(t, ev, &ended)
	if ended.TaskID.String() != shortTask || ended.Reason != ws.TimerEndedExpired {
		t.Fatalf("expected overdue timer to fire on recovery, got %+v", ended)
	}
	if status := e.timerStatus(shortTask); status != task.TimerPrompting {
		t.Fatalf("expected prompting after recovery, got %s", status)
	}

	// The not-yet-due timer is back in the index and still running.
	if err := e.redis.ZScore(context.Background(), e.timersKey, longTask).Err(); err != nil {
		t.Fatalf("running timer must be re-indexed: %v", err)
	}
	if status := e.timerStatus(longTask); status != task.TimerRunning {
		t.Fatalf("expected long timer still running, got %s", status)
	}
}

// --- test environment ---

type timerEnv struct {
	t         *testing.T
	db        *pgxpool.Pool
	redis     *goredis.Client
	tokens    *auth.TokenManager
	rooms     *room.Service
	service   *task.Service
	router    http.Handler
	clock     *fakeClock
	events    *eventRecorder
	prefix    string
	timersKey string
	ext       task.ExtensionConfig
}

func newTimerEnv(t *testing.T) *timerEnv {
	t.Helper()
	return newTimerEnvWith(t, task.ExtensionConfig{})
}

func newTimerEnvWith(t *testing.T, ext task.ExtensionConfig) *timerEnv {
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

	prefix := "test:" + uuid.NewString() + ":"
	t.Cleanup(func() { _ = redisClient.Del(context.Background(), prefix+"timers:due", prefix+"prompts:due").Err() })

	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	roomService := room.NewService(room.NewRepository(db))
	clock := &fakeClock{now: time.Now()}
	events := &eventRecorder{}
	taskService := task.NewService(task.NewRepository(db), roomService, task.TimerDeps{
		Index:     task.NewTimerIndex(redisClient, prefix),
		Events:    events,
		Clock:     clock.Now,
		Extension: ext,
	})

	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:          db,
		Redis:       redisClient,
		AuthMW:      auth.NewMiddleware(tokenManager),
		TaskHandler: task.NewHandler(taskService),
	})

	return &timerEnv{
		t: t, db: db, redis: redisClient, tokens: tokenManager, rooms: roomService,
		service: taskService, router: router, clock: clock, events: events,
		prefix: prefix, timersKey: prefix + "timers:due", ext: ext,
	}
}

// newService returns another service over the same database and index with
// its own clock, standing in for a second server instance.
func (e *timerEnv) newService(clock func() time.Time) *task.Service {
	return task.NewService(task.NewRepository(e.db), e.rooms, task.TimerDeps{
		Index:     task.NewTimerIndex(e.redis, e.prefix),
		Events:    e.events,
		Clock:     clock,
		Extension: e.ext,
	})
}

func (e *timerEnv) createUser() (uuid.UUID, string) {
	e.t.Helper()
	u := createTestUser(e.t, e.db, "timer-"+uuid.NewString()+"@example.com")
	token, err := e.tokens.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		e.t.Fatalf("issue access token: %v", err)
	}
	return u.ID, token
}

func (e *timerEnv) createRoom() uuid.UUID {
	e.t.Helper()
	return createTestRoom(e.t, e.db, "Timer Room "+uuid.NewString())
}

func (e *timerEnv) join(roomID, userID uuid.UUID) {
	e.t.Helper()
	if _, err := e.rooms.Join(context.Background(), roomID, userID); err != nil {
		e.t.Fatalf("join room: %v", err)
	}
}

func (e *timerEnv) do(method, path, token, body string) (int, string) {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (e *timerEnv) createTask(roomID uuid.UUID, token string) string {
	e.t.Helper()
	code, body := e.do(http.MethodPost, "/rooms/"+roomID.String()+"/tasks", token, `{"text": "focus"}`)
	if code != http.StatusCreated {
		e.t.Fatalf("create task: expected 201, got %d: %s", code, body)
	}
	return decodeTask(e.t, body).ID
}

func (e *timerEnv) start(taskID, token string, minutes int) taskResponse {
	e.t.Helper()
	payload, _ := json.Marshal(map[string]int{"duration_minutes": minutes})
	code, body := e.do(http.MethodPost, "/tasks/"+taskID+"/start", token, string(payload))
	if code != http.StatusOK {
		e.t.Fatalf("start timer: expected 200, got %d: %s", code, body)
	}
	return decodeTask(e.t, body)
}

func (e *timerEnv) complete(taskID, token string) taskResponse {
	e.t.Helper()
	code, body := e.do(http.MethodPatch, "/tasks/"+taskID, token, `{"completed": true}`)
	if code != http.StatusOK {
		e.t.Fatalf("complete task: expected 200, got %d: %s", code, body)
	}
	return decodeTask(e.t, body)
}

func (e *timerEnv) timerStatus(taskID string) string {
	e.t.Helper()
	var status string
	if err := e.db.QueryRow(context.Background(), `SELECT timer_status FROM tasks WHERE id = $1`, taskID).Scan(&status); err != nil {
		e.t.Fatalf("read timer status: %v", err)
	}
	return status
}

type runningScheduler struct {
	cancel    context.CancelFunc
	scheduler *task.Scheduler
	once      sync.Once
}

func (r *runningScheduler) stop() {
	r.once.Do(func() {
		r.cancel()
		r.scheduler.Wait()
	})
}

func (e *timerEnv) startScheduler() *runningScheduler {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := task.NewScheduler(e.service, 10*time.Millisecond)
	if err := s.Start(ctx); err != nil {
		cancel()
		e.t.Fatalf("start scheduler: %v", err)
	}
	r := &runningScheduler{cancel: cancel, scheduler: s}
	e.t.Cleanup(r.stop)
	return r
}

type taskResponse struct {
	ID                   string     `json:"id"`
	TimerStatus          string     `json:"timer_status"`
	TimerDurationMinutes *int       `json:"timer_duration_minutes"`
	TimerStartedAt       *time.Time `json:"timer_started_at"`
	TimerEndsAt          *time.Time `json:"timer_ends_at"`
	PromptDeadlineAt     *time.Time `json:"prompt_deadline_at"`
	ExtendedCount        int        `json:"extended_count"`
	ExtraMinutesUsed     int        `json:"extra_minutes_used"`
}

func decodeTask(t *testing.T, body string) taskResponse {
	t.Helper()
	var r taskResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode task %s: %v", body, err)
	}
	return r
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recordedEvent is one event handed to the sink; UserID is set for targeted
// sends.
type recordedEvent struct {
	ws.Event
	UserID    uuid.UUID
	Broadcast bool
}

type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (r *eventRecorder) Broadcast(_ uuid.UUID, ev ws.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{Event: ev, Broadcast: true})
}

func (r *eventRecorder) SendToUser(userID uuid.UUID, ev ws.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{Event: ev, UserID: userID})
}

func (r *eventRecorder) all() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedEvent(nil), r.events...)
}

func (r *eventRecorder) count(eventType string) int {
	n := 0
	for _, ev := range r.all() {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}

// waitFor returns the first recorded event of the type for the room, waiting
// up to 5s for it.
func (r *eventRecorder) waitFor(t *testing.T, eventType string, roomID uuid.UUID) recordedEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range r.all() {
			if ev.Type == eventType && ev.RoomID == roomID {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s event for room %s; got %+v", eventType, roomID, r.all())
	return recordedEvent{}
}

func decodeEventData(t *testing.T, ev recordedEvent, dst any) {
	t.Helper()
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		t.Fatalf("encode %s data: %v", ev.Type, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode %s data %s: %v", ev.Type, raw, err)
	}
}
