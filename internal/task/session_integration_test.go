package task_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/task"
	"github.com/workbuddy/backend/internal/ws"
)

type roomTaskItem struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Author struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		AvatarURL *string `json:"avatar_url"`
	} `json:"author"`
	Completed       bool       `json:"completed"`
	TimerStatus     string     `json:"timer_status"`
	DurationMinutes *int       `json:"duration_minutes"`
	TimerStartedAt  *time.Time `json:"timer_started_at"`
	EndsAt          *time.Time `json:"ends_at"`
	ExtendedCount   int        `json:"extended_count"`
}

func (e *timerEnv) listRoomTasks(roomID uuid.UUID, token string) []roomTaskItem {
	e.t.Helper()
	code, body := e.do(http.MethodGet, "/rooms/"+roomID.String()+"/tasks", token, "")
	if code != http.StatusOK {
		e.t.Fatalf("list room tasks: expected 200, got %d: %s", code, body)
	}
	var out struct {
		Items []roomTaskItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatalf("decode %s: %v", body, err)
	}
	if out.Items == nil {
		e.t.Fatalf("items must be an array, got %s", body)
	}
	return out.Items
}

func (e *timerEnv) taskEvents(eventType, taskID string) []ws.TaskEventData {
	e.t.Helper()
	var out []ws.TaskEventData
	for _, ev := range e.events.all() {
		if ev.Type != eventType {
			continue
		}
		data, ok := ev.Data.(ws.TaskEventData)
		if !ok {
			e.t.Fatalf("%s: unexpected payload %T", eventType, ev.Data)
		}
		if !ev.Broadcast {
			e.t.Fatalf("%s must be a broadcast", eventType)
		}
		if data.TaskID.String() == taskID {
			out = append(out, data)
		}
	}
	return out
}

func TestListRoomTasks(t *testing.T) {
	e := newTimerEnv(t)
	roomID := e.createRoom()
	alice, aliceToken := e.createUser()
	bob, bobToken := e.createUser()
	_, outsiderToken := e.createUser()
	leaver, leaverToken := e.createUser()
	e.join(roomID, alice)
	e.join(roomID, bob)
	e.join(roomID, leaver)

	if got := e.listRoomTasks(roomID, aliceToken); len(got) != 0 {
		t.Fatalf("expected no tasks, got %+v", got)
	}

	first := e.createTask(roomID, aliceToken)
	second := e.createTask(roomID, bobToken)
	e.start(second, bobToken, 25)
	e.createTask(roomID, leaverToken)
	if err := e.rooms.Leave(context.Background(), roomID, leaver); err != nil {
		t.Fatalf("leave: %v", err)
	}

	got := e.listRoomTasks(roomID, aliceToken)
	if len(got) != 2 || got[0].ID != second || got[1].ID != first {
		t.Fatalf("expected the two participants' tasks, newest first, got %+v", got)
	}
	running := got[0]
	if running.Author.ID != bob.String() || running.Author.Name == "" || running.Completed ||
		running.TimerStatus != task.TimerRunning || running.DurationMinutes == nil || *running.DurationMinutes != 25 ||
		running.TimerStartedAt == nil || running.EndsAt == nil || running.ExtendedCount != 0 {
		t.Fatalf("unexpected running task %+v", running)
	}
	idle := got[1]
	if idle.TimerStatus != task.TimerIdle || idle.DurationMinutes != nil || idle.TimerStartedAt != nil || idle.EndsAt != nil {
		t.Fatalf("unexpected idle task %+v", idle)
	}

	e.complete(first, aliceToken)
	if got := e.listRoomTasks(roomID, bobToken); !got[1].Completed {
		t.Fatalf("expected a completed task, got %+v", got[1])
	}

	for name, path := range map[string]string{
		"outsider":     "/rooms/" + roomID.String() + "/tasks",
		"no such room": "/rooms/" + uuid.NewString() + "/tasks",
	} {
		code, body := e.do(http.MethodGet, path, outsiderToken, "")
		if code != http.StatusForbidden || !strings.Contains(body, `"code":"FORBIDDEN"`) {
			t.Fatalf("%s: expected 403, got %d: %s", name, code, body)
		}
	}
	if code, _ := e.do(http.MethodGet, "/rooms/not-a-uuid/tasks", aliceToken, ""); code != http.StatusBadRequest {
		t.Fatalf("bad id: expected 400, got %d", code)
	}
}

func TestCreateTaskWithTimerIsAtomic(t *testing.T) {
	e := newTimerEnv(t)
	roomID := e.createRoom()
	owner, ownerToken := e.createUser()
	e.join(roomID, owner)
	path := "/rooms/" + roomID.String() + "/tasks"

	code, body := e.do(http.MethodPost, path, ownerToken, `{"text":"with timer","duration_minutes":25}`)
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", code, body)
	}
	created := decodeTask(t, body)
	if created.TimerStatus != task.TimerRunning || created.TimerDurationMinutes == nil || *created.TimerDurationMinutes != 25 || created.TimerEndsAt == nil {
		t.Fatalf("expected a running timer, got %+v", created)
	}
	if _, err := e.redis.ZScore(context.Background(), e.timersKey, created.ID).Result(); err != nil {
		t.Fatalf("expected the timer in the due index: %v", err)
	}
	if got := e.taskEvents(ws.TypeTaskCreated, created.ID); len(got) != 1 || got[0].TimerStatus != task.TimerRunning {
		t.Fatalf("expected one task_created, got %+v", got)
	}
	if n := e.events.count(ws.TypeTimerStarted); n != 1 {
		t.Fatalf("expected one timer_started, got %d", n)
	}

	count := func() int {
		var n int
		if err := e.db.QueryRow(context.Background(), `SELECT count(*) FROM tasks WHERE user_id = $1`, owner).Scan(&n); err != nil {
			t.Fatalf("count tasks: %v", err)
		}
		return n
	}

	// A second timer is refused and leaves no task behind.
	code, body = e.do(http.MethodPost, path, ownerToken, `{"text":"second","duration_minutes":50}`)
	if code != http.StatusConflict || !strings.Contains(body, `"code":"CONFLICT"`) {
		t.Fatalf("expected 409 CONFLICT, got %d: %s", code, body)
	}
	if n := count(); n != 1 {
		t.Fatalf("a refused create must leave no task, got %d", n)
	}

	for _, tc := range []struct{ name, body, want string }{
		{"bad duration", `{"text":"x","duration_minutes":30}`, `"duration_minutes":"INVALID_VALUE"`},
		{"zero duration", `{"text":"x","duration_minutes":0}`, `"duration_minutes":"INVALID_VALUE"`},
		{"no text", `{"duration_minutes":25}`, `"text":"REQUIRED"`},
	} {
		code, body := e.do(http.MethodPost, path, ownerToken, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Fatalf("%s: expected 400 with %s, got %d: %s", tc.name, tc.want, code, body)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("rejected creates must leave no task, got %d", n)
	}

	// Without a duration the old behaviour stays: no timer, no room needed.
	_, outsiderToken := e.createUser()
	code, body = e.do(http.MethodPost, path, outsiderToken, `{"text":"plain"}`)
	if code != http.StatusCreated || decodeTask(t, body).TimerStatus != task.TimerIdle {
		t.Fatalf("expected an idle task, got %d: %s", code, body)
	}
	code, body = e.do(http.MethodPost, path, outsiderToken, `{"text":"timer","duration_minutes":25}`)
	if code != http.StatusForbidden {
		t.Fatalf("a timer needs participation: expected 403, got %d: %s", code, body)
	}
}

func TestDeleteTaskStopsTimer(t *testing.T) {
	e := newTimerEnv(t)
	roomID := e.createRoom()
	owner, ownerToken := e.createUser()
	_, otherToken := e.createUser()
	e.join(roomID, owner)
	ctx := context.Background()

	taskID := e.createTask(roomID, ownerToken)
	e.start(taskID, ownerToken, 25)

	if code, body := e.do(http.MethodDelete, "/tasks/"+taskID, otherToken, ""); code != http.StatusForbidden || !strings.Contains(body, `"code":"FORBIDDEN"`) {
		t.Fatalf("expected 403 for another user, got %d: %s", code, body)
	}
	if code, body := e.do(http.MethodDelete, "/tasks/"+taskID, ownerToken, ""); code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", code, body)
	}

	if _, err := e.redis.ZScore(ctx, e.timersKey, taskID).Result(); err == nil {
		t.Fatal("the timer must leave the due index")
	}
	if got := e.taskEvents(ws.TypeTaskDeleted, taskID); len(got) != 1 || got[0].TimerStatus != task.TimerRunning {
		t.Fatalf("expected one task_deleted with the last state, got %+v", got)
	}
	if code, _ := e.do(http.MethodDelete, "/tasks/"+taskID, ownerToken, ""); code != http.StatusNotFound {
		t.Fatalf("second delete: expected 404, got %d", code)
	}

	// The timer slot is free again, and the owner is still in the room.
	second := e.createTask(roomID, ownerToken)
	e.start(second, ownerToken, 25)
	e.clock.Advance(26 * time.Minute)
	if err := e.service.FireDue(ctx); err != nil {
		t.Fatalf("fire due: %v", err)
	}
	if status := e.timerStatus(second); status != task.TimerPrompting {
		t.Fatalf("expected the second timer to expire, got %s", status)
	}
	code, _ := e.do(http.MethodDelete, "/tasks/"+second, ownerToken, "")
	if code != http.StatusNoContent {
		t.Fatalf("delete a prompting task: expected 204, got %d", code)
	}
	if score, err := e.redis.ZScore(ctx, e.prefix+"prompts:due", second).Result(); err == nil {
		t.Fatalf("the prompt must leave the due index, got score %v", score)
	}
	e.clock.Advance(2 * time.Minute)
	if err := e.service.FireDue(ctx); err != nil {
		t.Fatalf("fire due: %v", err)
	}
	if e.kickedParticipations(roomID, owner) != 0 {
		t.Fatal("deleting a prompting task must not remove the owner from the room")
	}
}

func TestTaskEventsFire(t *testing.T) {
	e := newTimerEnv(t)
	roomID := e.createRoom()
	owner, ownerToken := e.createUser()
	e.join(roomID, owner)
	ctx := context.Background()

	taskID := e.createTask(roomID, ownerToken)
	created := e.taskEvents(ws.TypeTaskCreated, taskID)
	if len(created) != 1 || created[0].UserID != owner || created[0].ID.String() != taskID || created[0].Text != "focus" ||
		created[0].Author.ID != owner || created[0].Author.Name == "" || created[0].Completed ||
		created[0].TimerStatus != task.TimerIdle || created[0].EndsAt != nil || created[0].DurationMinutes != nil {
		t.Fatalf("unexpected task_created %+v", created)
	}

	updates := func() int { return len(e.taskEvents(ws.TypeTaskUpdated, taskID)) }

	e.start(taskID, ownerToken, 25)
	got := e.taskEvents(ws.TypeTaskUpdated, taskID)
	if len(got) != 1 || got[0].TimerStatus != task.TimerRunning || got[0].EndsAt == nil || got[0].DurationMinutes == nil {
		t.Fatalf("expected task_updated on start, got %+v", got)
	}

	e.clock.Advance(26 * time.Minute)
	if err := e.service.FireDue(ctx); err != nil {
		t.Fatalf("fire due: %v", err)
	}
	got = e.taskEvents(ws.TypeTaskUpdated, taskID)
	if len(got) != 2 || got[1].TimerStatus != task.TimerPrompting {
		t.Fatalf("expected task_updated on expiry, got %+v", got)
	}

	code, body := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 5}`)
	if code != http.StatusOK {
		t.Fatalf("extend: expected 200, got %d: %s", code, body)
	}
	got = e.taskEvents(ws.TypeTaskUpdated, taskID)
	if len(got) != 3 || got[2].TimerStatus != task.TimerRunning || got[2].ExtendedCount != 1 {
		t.Fatalf("expected task_updated on extend, got %+v", got)
	}

	e.complete(taskID, ownerToken)
	got = e.taskEvents(ws.TypeTaskUpdated, taskID)
	if len(got) != 4 || !got[3].Completed || got[3].TimerStatus != task.TimerEnded {
		t.Fatalf("expected task_updated on completion, got %+v", got)
	}

	if code, _ := e.do(http.MethodPatch, "/tasks/"+taskID, ownerToken, `{"text":"renamed"}`); code != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d", code)
	}
	if n := updates(); n != 5 {
		t.Fatalf("expected task_updated on edit, got %d", n)
	}
}

func TestExtensionLimitByTotalMinutes(t *testing.T) {
	e := newTimerEnvWith(t, task.ExtensionConfig{
		PromptTimeout:   time.Minute,
		MaxCount:        5,
		Options:         []int{5, 10, 15},
		MaxTotalMinutes: 20,
	})
	roomID := e.createRoom()
	owner, ownerToken := e.createUser()
	e.join(roomID, owner)
	ctx := context.Background()

	taskID := e.createTask(roomID, ownerToken)
	e.start(taskID, ownerToken, 25)

	expire := func() {
		t.Helper()
		e.clock.Advance(40 * time.Minute)
		if err := e.service.FireDue(ctx); err != nil {
			t.Fatalf("fire due: %v", err)
		}
	}
	extend := func(minutes string) (int, string) {
		t.Helper()
		return e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": `+minutes+`}`)
	}

	expire()
	if p := e.lastPrompt(t); len(p.AllowedMinutes) != 3 || p.ExtensionsLeft != 5 {
		t.Fatalf("expected all options, got %+v", p)
	}
	if code, body := extend("7"); code != http.StatusBadRequest || !strings.Contains(body, `"minutes":"INVALID_VALUE"`) {
		t.Fatalf("a value outside the options: expected 400, got %d: %s", code, body)
	}
	if code, body := extend("15"); code != http.StatusOK {
		t.Fatalf("extend 15: expected 200, got %d: %s", code, body)
	}

	// 15 of 20 used: only 5 still fits.
	expire()
	if p := e.lastPrompt(t); len(p.AllowedMinutes) != 1 || p.AllowedMinutes[0] != 5 {
		t.Fatalf("expected only 5 minutes, got %+v", p)
	}
	if code, body := extend("10"); code != http.StatusConflict || !strings.Contains(body, `"code":"EXTEND_LIMIT_REACHED"`) {
		t.Fatalf("over the total: expected 409 EXTEND_LIMIT_REACHED, got %d: %s", code, body)
	}
	if code, body := extend("5"); code != http.StatusOK {
		t.Fatalf("extend 5: expected 200, got %d: %s", code, body)
	}

	// 20 of 20 used: nothing is allowed.
	expire()
	if p := e.lastPrompt(t); len(p.AllowedMinutes) != 0 || p.ExtensionsLeft != 0 {
		t.Fatalf("expected nothing allowed, got %+v", p)
	}
	if code, body := extend("5"); code != http.StatusConflict || !strings.Contains(body, `"code":"EXTEND_LIMIT_REACHED"`) {
		t.Fatalf("at the total: expected 409 EXTEND_LIMIT_REACHED, got %d: %s", code, body)
	}
}

func TestExtensionLimitByCount(t *testing.T) {
	e := newTimerEnvWith(t, task.ExtensionConfig{PromptTimeout: time.Minute, MaxCount: 1, Options: []int{5, 10, 15}, MaxTotalMinutes: 30})
	roomID := e.createRoom()
	owner, ownerToken := e.createUser()
	e.join(roomID, owner)
	taskID := e.createTask(roomID, ownerToken)
	e.start(taskID, ownerToken, 25)

	expire := func() {
		t.Helper()
		e.clock.Advance(40 * time.Minute)
		if err := e.service.FireDue(context.Background()); err != nil {
			t.Fatalf("fire due: %v", err)
		}
	}

	expire()
	if code, body := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 5}`); code != http.StatusOK {
		t.Fatalf("extend: expected 200, got %d: %s", code, body)
	}
	expire()
	if p := e.lastPrompt(t); len(p.AllowedMinutes) != 0 || p.ExtensionsLeft != 0 {
		t.Fatalf("expected nothing allowed, got %+v", p)
	}
	code, body := e.do(http.MethodPost, "/tasks/"+taskID+"/extend", ownerToken, `{"minutes": 5}`)
	if code != http.StatusConflict || !strings.Contains(body, `"code":"EXTEND_LIMIT_REACHED"`) {
		t.Fatalf("expected 409 EXTEND_LIMIT_REACHED, got %d: %s", code, body)
	}
}

func TestDeleteForeignOrMissingServiceErrors(t *testing.T) {
	e := newTimerEnv(t)
	owner, _ := e.createUser()
	err := e.service.Delete(context.Background(), uuid.New(), owner)
	if !errors.Is(err, task.ErrTaskNotFound) {
		t.Fatalf("expected ErrTaskNotFound, got %v", err)
	}
}
