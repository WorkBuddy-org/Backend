package task_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/task"
	"github.com/workbuddy/backend/internal/ws"
)

func TestExtensionHappyPath(t *testing.T) {
	e := newTimerEnv(t)
	owner, ownerToken := e.createUser()
	_, otherToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, owner)
	taskID := e.createTask(roomID, ownerToken)
	e.start(taskID, ownerToken, 25)

	// Answering before the timer ends is a conflict.
	if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 15}`); code != http.StatusConflict {
		t.Fatalf("extend while running: expected 409, got %d", code)
	}

	expiredAt := e.expire()
	prompt := e.events.waitFor(t, ws.TypeTaskExtensionPrompt, roomID)
	if prompt.Broadcast || prompt.UserID != owner {
		t.Fatalf("prompt must target only the owner, got %+v", prompt)
	}
	var promptData ws.TaskExtensionPromptData
	decodeEventData(t, prompt, &promptData)
	wantDeadline := expiredAt.Add(task.DefaultExtensionPromptTimeout)
	if promptData.TaskID.String() != taskID || promptData.Deadline != ws.FormatTime(wantDeadline) ||
		len(promptData.AllowedMinutes) != 2 || promptData.AllowedMinutes[0] != 15 || promptData.AllowedMinutes[1] != 30 ||
		promptData.ExtensionsLeft != task.DefaultExtensionMaxCount || promptData.ServerTime == "" {
		t.Fatalf("unexpected prompt payload %+v", promptData)
	}
	if got := e.getTask(taskID); got.TimerStatus != task.TimerPrompting || got.PromptDeadlineAt == nil || !got.PromptDeadlineAt.Equal(wantDeadline) {
		t.Fatalf("expected prompting until %s, got %+v", wantDeadline, got)
	}
	e.expectIndexed(e.prefix+"prompts:due", taskID, wantDeadline)

	for _, body := range []string{`{"minutes": 20}`, `{"minutes": 0}`, `{}`, `{"minutes": 15, "x": 1}`} {
		if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, body); code != http.StatusBadRequest {
			t.Fatalf("extend %s: expected 400, got %d", body, code)
		}
	}
	if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", otherToken, `{"minutes": 15}`); code != http.StatusForbidden {
		t.Fatalf("extend by non-owner: expected 403, got %d", code)
	}

	e.clock.Advance(10 * time.Second)
	extendedAt := e.clock.Now().UTC().Truncate(time.Millisecond)
	code, body := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 30}`)
	if code != http.StatusOK {
		t.Fatalf("extend: expected 200, got %d: %s", code, body)
	}
	got := decodeTask(t, body)
	wantEnds := extendedAt.Add(30 * time.Minute)
	if got.TimerStatus != task.TimerRunning || got.TimerEndsAt == nil || !got.TimerEndsAt.Equal(wantEnds) ||
		got.ExtendedCount != 1 || got.ExtraMinutesUsed != 30 || got.PromptDeadlineAt != nil {
		t.Fatalf("unexpected task after extend %+v", got)
	}
	e.expectIndexed(e.timersKey, taskID, wantEnds)
	e.expectNotIndexed(e.prefix+"prompts:due", taskID)

	ev := e.events.waitFor(t, ws.TypeTaskExtended, roomID)
	var extended ws.TaskExtendedData
	decodeEventData(t, ev, &extended)
	if !ev.Broadcast || extended.TaskID.String() != taskID || extended.UserID != owner || extended.AddedMinutes != 30 ||
		extended.ExtendedCount != 1 || extended.ExtraMinutesUsed != 30 || extended.EndsAt != ws.FormatTime(wantEnds) {
		t.Fatalf("unexpected task_extended %+v", extended)
	}

	// The extended timer expires again and prompts again, with one fewer left.
	e.clock.Advance(31 * time.Minute)
	if err := e.service.FireDue(context.Background()); err != nil {
		t.Fatalf("fire due: %v", err)
	}
	if n := e.events.count(ws.TypeTaskExtensionPrompt); n != 2 {
		t.Fatalf("expected a second prompt, got %d", n)
	}
	if left := e.lastPrompt(t).ExtensionsLeft; left != task.DefaultExtensionMaxCount-1 {
		t.Fatalf("expected %d extensions left, got %d", task.DefaultExtensionMaxCount-1, left)
	}
	if e.participationLeftReason(roomID, owner) != "" {
		t.Fatal("extending must keep the user in the room")
	}
}

func TestExtensionLimitReached(t *testing.T) {
	e := newTimerEnvWith(t, task.ExtensionConfig{PromptTimeout: time.Minute, MaxCount: 1})
	owner, ownerToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, owner)
	taskID := e.createTask(roomID, ownerToken)
	e.start(taskID, ownerToken, 25)

	e.expire()
	if code, body := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 15}`); code != http.StatusOK {
		t.Fatalf("first extend: expected 200, got %d: %s", code, body)
	}

	e.clock.Advance(16 * time.Minute)
	if err := e.service.FireDue(context.Background()); err != nil {
		t.Fatalf("fire due: %v", err)
	}
	last := e.lastPrompt(t)
	if last.ExtensionsLeft != 0 || len(last.AllowedMinutes) != 0 {
		t.Fatalf("expected no extensions offered, got %+v", last)
	}

	code, body := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 15}`)
	if code != http.StatusConflict {
		t.Fatalf("extend over the limit: expected 409, got %d: %s", code, body)
	}
	got := e.getTask(taskID)
	if got.ExtendedCount != 1 || got.ExtraMinutesUsed != 15 || got.TimerStatus != task.TimerPrompting {
		t.Fatalf("rejected extend must not change the task, got %+v", got)
	}
	// The prompt still times out normally.
	e.expectNotIndexed(e.timersKey, taskID)
	if err := e.redis.ZScore(context.Background(), e.prefix+"prompts:due", taskID).Err(); err != nil {
		t.Fatalf("prompt deadline must stay indexed after a rejected extend: %v", err)
	}
}

func TestExtendAfterDeadline(t *testing.T) {
	e := newTimerEnv(t)
	owner, ownerToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, owner)
	taskID := e.createTask(roomID, ownerToken)
	e.start(taskID, ownerToken, 25)
	e.expire()

	// Deadline passed but the scheduler has not run yet.
	e.clock.Advance(task.DefaultExtensionPromptTimeout + time.Second)
	if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 15}`); code != http.StatusConflict {
		t.Fatalf("extend after deadline: expected 409, got %d", code)
	}

	// Once the scheduler has ended the prompt, answering is still a conflict.
	if err := e.service.FireDue(context.Background()); err != nil {
		t.Fatalf("fire due: %v", err)
	}
	if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 15}`); code != http.StatusConflict {
		t.Fatalf("extend after kick: expected 409, got %d", code)
	}
	if got := e.getTask(taskID); got.TimerStatus != task.TimerEnded || got.ExtendedCount != 0 {
		t.Fatalf("expected ended timer without extension, got %+v", got)
	}
}

func TestExtensionTimeoutKicks(t *testing.T) {
	e := newTimerEnv(t)
	owner, ownerToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, owner)
	taskID := e.createTask(roomID, ownerToken)
	e.start(taskID, ownerToken, 25)
	scheduler := e.startScheduler()

	e.clock.Advance(25*time.Minute + time.Second)
	e.events.waitFor(t, ws.TypeTaskExtensionPrompt, roomID)

	// Just before the deadline nothing happens.
	e.clock.Advance(task.DefaultExtensionPromptTimeout - 2*time.Second)
	time.Sleep(100 * time.Millisecond)
	if e.participationLeftReason(roomID, owner) != "" {
		t.Fatal("user must stay in the room before the deadline")
	}

	e.clock.Advance(2 * time.Second)
	e.waitForLeftReason(roomID, owner, "kicked_no_extension")
	scheduler.stop()

	if got := e.getTask(taskID); got.TimerStatus != task.TimerEnded || got.PromptDeadlineAt != nil {
		t.Fatalf("expected ended timer, got %+v", got)
	}
	e.expectNotIndexed(e.prefix+"prompts:due", taskID)
	if n := e.kickedParticipations(roomID, owner); n != 1 {
		t.Fatalf("expected exactly one kicked participation, got %d", n)
	}
}

func TestNoExtendKicks(t *testing.T) {
	e := newTimerEnv(t)
	owner, ownerToken := e.createUser()
	_, otherToken := e.createUser()
	roomID := e.createRoom()
	e.join(roomID, owner)
	taskID := e.createTask(roomID, ownerToken)
	e.start(taskID, ownerToken, 25)

	if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/no-extend", ownerToken, ""); code != http.StatusConflict {
		t.Fatalf("no-extend while running: expected 409, got %d", code)
	}

	e.expire()
	if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/no-extend", otherToken, ""); code != http.StatusForbidden {
		t.Fatalf("no-extend by non-owner: expected 403, got %d", code)
	}

	code, body := e.do(http.MethodPost, "/tasks/"+taskID+"/no-extend", ownerToken, "")
	if code != http.StatusOK {
		t.Fatalf("no-extend: expected 200, got %d: %s", code, body)
	}
	if got := decodeTask(t, body); got.TimerStatus != task.TimerEnded {
		t.Fatalf("expected ended timer, got %+v", got)
	}
	if reason := e.participationLeftReason(roomID, owner); reason != "kicked_no_extension" {
		t.Fatalf("expected participation closed as kicked_no_extension, got %q", reason)
	}
	e.expectNotIndexed(e.prefix+"prompts:due", taskID)

	// Answering twice, or the deadline passing afterwards, does nothing more.
	if code, _ := e.do(http.MethodPost, "/tasks/"+taskID+"/no-extend", ownerToken, ""); code != http.StatusConflict {
		t.Fatalf("second no-extend: expected 409, got %d", code)
	}
	e.clock.Advance(2 * task.DefaultExtensionPromptTimeout)
	if err := e.service.FireDue(context.Background()); err != nil {
		t.Fatalf("fire due: %v", err)
	}
	if n := e.kickedParticipations(roomID, owner); n != 1 {
		t.Fatalf("expected exactly one kicked participation, got %d", n)
	}

	// The slot is free again once the user rejoins.
	e.join(roomID, owner)
	other := e.createTask(roomID, ownerToken)
	e.start(other, ownerToken, 25)
}

// TestExtendRacesDeadline runs an extend (on an instance whose clock is just
// before the deadline) against the deadline scheduler (on an instance whose
// clock is just after it). Exactly one of them must win, every time.
func TestExtendRacesDeadline(t *testing.T) {
	e := newTimerEnv(t)
	roomID := e.createRoom()
	ctx := context.Background()

	extendWins, deadlineWins := 0, 0
	for i := 0; i < 15; i++ {
		owner, ownerToken := e.createUser()
		e.join(roomID, owner)
		taskID := e.createTask(roomID, ownerToken)
		e.start(taskID, ownerToken, 25)
		deadline := e.expire().Add(task.DefaultExtensionPromptTimeout)

		answerer := e.newService((&fakeClock{now: deadline.Add(-time.Second)}).Now)
		poller := e.newService((&fakeClock{now: deadline.Add(time.Second)}).Now)

		id := uuid.MustParse(taskID)
		var extendErr, fireErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, extendErr = answerer.ExtendTimer(ctx, id, owner, 15)
		}()
		go func() {
			defer wg.Done()
			<-start
			fireErr = poller.FireDue(ctx)
		}()
		close(start)
		wg.Wait()

		if fireErr != nil {
			t.Fatalf("iteration %d: fire due: %v", i, fireErr)
		}
		got := e.getTask(taskID)
		kicked := e.kickedParticipations(roomID, owner)
		switch {
		case extendErr == nil:
			extendWins++
			if got.TimerStatus != task.TimerRunning || got.ExtendedCount != 1 || kicked != 0 {
				t.Fatalf("iteration %d: extend won but state is %+v, kicked=%d", i, got, kicked)
			}
			// Free the user's timer slot for a clean state.
			e.complete(taskID, ownerToken)
		case errors.Is(extendErr, task.ErrPromptExpired):
			deadlineWins++
			if got.TimerStatus != task.TimerEnded || got.ExtendedCount != 0 || kicked != 1 {
				t.Fatalf("iteration %d: deadline won but state is %+v, kicked=%d", i, got, kicked)
			}
		default:
			t.Fatalf("iteration %d: unexpected extend error %v", i, extendErr)
		}
		e.expectNotIndexed(e.prefix+"prompts:due", taskID)
	}
	t.Logf("extend won %d, deadline won %d", extendWins, deadlineWins)
}

// --- helpers ---

// expire moves the clock past a 25-minute timer's end and runs one scheduler
// pass synchronously. It returns the clock time of the expiry.
func (e *timerEnv) expire() time.Time {
	e.t.Helper()
	e.clock.Advance(25*time.Minute + time.Second)
	if err := e.service.FireDue(context.Background()); err != nil {
		e.t.Fatalf("fire due: %v", err)
	}
	return e.clock.Now().UTC().Truncate(time.Millisecond)
}

func (e *timerEnv) getTask(taskID string) taskResponse {
	e.t.Helper()
	var r taskResponse
	err := e.db.QueryRow(context.Background(), `
		SELECT id, timer_status, timer_duration_minutes, timer_started_at, timer_ends_at, prompt_deadline_at,
			extended_count, extra_minutes_used
		FROM tasks WHERE id = $1`, taskID).
		Scan(&r.ID, &r.TimerStatus, &r.TimerDurationMinutes, &r.TimerStartedAt, &r.TimerEndsAt, &r.PromptDeadlineAt,
			&r.ExtendedCount, &r.ExtraMinutesUsed)
	if err != nil {
		e.t.Fatalf("read task: %v", err)
	}
	return r
}

func (e *timerEnv) lastPrompt(t *testing.T) ws.TaskExtensionPromptData {
	t.Helper()
	var last *recordedEvent
	for _, ev := range e.events.all() {
		if ev.Type == ws.TypeTaskExtensionPrompt {
			last = &ev
		}
	}
	if last == nil {
		t.Fatal("no task_extension_prompt recorded")
	}
	var data ws.TaskExtensionPromptData
	decodeEventData(t, *last, &data)
	return data
}

func (e *timerEnv) expectIndexed(key, taskID string, at time.Time) {
	e.t.Helper()
	got, err := e.redis.ZScore(context.Background(), key, taskID).Result()
	if err != nil {
		e.t.Fatalf("expected %s in %s: %v", taskID, key, err)
	}
	if want := float64(at.UnixMilli()) / 1000; got != want {
		e.t.Fatalf("expected %s score %f, got %f", key, want, got)
	}
}

func (e *timerEnv) expectNotIndexed(key, taskID string) {
	e.t.Helper()
	if err := e.redis.ZScore(context.Background(), key, taskID).Err(); !errors.Is(err, goredis.Nil) {
		e.t.Fatalf("expected %s not in %s, got %v", taskID, key, err)
	}
}

// participationLeftReason returns the left_reason of the user's latest
// participation in the room ("" while it is open).
func (e *timerEnv) participationLeftReason(roomID, userID uuid.UUID) string {
	e.t.Helper()
	var reason *string
	err := e.db.QueryRow(context.Background(), `
		SELECT left_reason FROM room_participations
		WHERE room_id = $1 AND user_id = $2
		ORDER BY joined_at DESC LIMIT 1`, roomID, userID).Scan(&reason)
	if err != nil {
		e.t.Fatalf("read participation: %v", err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

func (e *timerEnv) waitForLeftReason(roomID, userID uuid.UUID, want string) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.participationLeftReason(roomID, userID) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("participation never closed as %q", want)
}

func (e *timerEnv) kickedParticipations(roomID, userID uuid.UUID) int {
	e.t.Helper()
	var n int
	err := e.db.QueryRow(context.Background(), `
		SELECT count(*) FROM room_participations
		WHERE room_id = $1 AND user_id = $2 AND left_reason = 'kicked_no_extension'`, roomID, userID).Scan(&n)
	if err != nil {
		e.t.Fatalf("count kicked participations: %v", err)
	}
	return n
}
