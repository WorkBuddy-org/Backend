package task_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTaskTextLength(t *testing.T) {
	e := newTaskEnv(t)
	roomID := createTestRoom(t, e.db, "Task Text Room "+uuid.NewString())
	owner := e.newUser()
	tasksPath := "/rooms/" + roomID.String() + "/tasks"

	body := func(text string) string {
		b, _ := json.Marshal(map[string]string{"text": text})
		return string(b)
	}

	// 200 runes of a multi-byte character, padded with spaces that are
	// trimmed before counting.
	maxText := strings.Repeat("ж", 200)
	code, resp := e.do(http.MethodPost, tasksPath, owner, body("  "+maxText+"  "))
	if code != http.StatusCreated {
		t.Fatalf("create with 200 runes: expected 201, got %d: %s", code, resp)
	}
	var created struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(resp), &created); err != nil || created.Text != maxText {
		t.Fatalf("expected trimmed 200-rune text, got %s", resp)
	}

	tooLong := strings.Repeat("a", 201)
	for _, tc := range []struct {
		method, path, body, wantErr string
	}{
		{http.MethodPost, tasksPath, body(tooLong), "text must be at most 200 characters"},
		{http.MethodPost, tasksPath, body(strings.Repeat("ж", 201)), "text must be at most 200 characters"},
		{http.MethodPost, tasksPath, body("   "), "text is required"},
		{http.MethodPatch, "/tasks/" + created.ID, body(tooLong), "text must be at most 200 characters"},
		{http.MethodPatch, "/tasks/" + created.ID, body(" \t "), "text is required"},
	} {
		code, resp := e.do(tc.method, tc.path, owner, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(resp, `"code":"VALIDATION_ERROR","message":"`+tc.wantErr+`"`) {
			t.Fatalf("%s %s: expected 400 %q, got %d: %s", tc.method, tc.path, tc.wantErr, code, resp)
		}
	}

	if code, resp := e.do(http.MethodPatch, "/tasks/"+created.ID, owner, body(strings.Repeat("b", 200))); code != http.StatusOK {
		t.Fatalf("patch with 200 chars: expected 200, got %d: %s", code, resp)
	}
}
