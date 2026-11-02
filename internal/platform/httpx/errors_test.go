package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/workbuddy/backend/internal/platform/httpx"
)

type errorEnvelope struct {
	Error struct {
		Code    string                     `json:"code"`
		Message string                     `json:"message"`
		Details map[string]json.RawMessage `json:"details"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return env
}

func TestErrorOmitsEmptyDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.Error(rec, http.StatusNotFound, httpx.CodeNotFound, "room not found")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected application/json, got %q", got)
	}
	want := `{"error":{"code":"NOT_FOUND","message":"room not found"}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestValidationWithFields(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.Validation(rec, "invalid email address", httpx.Fields{"email": httpx.FieldInvalidFormat})

	want := `{"error":{"code":"VALIDATION_ERROR","message":"invalid email address","details":{"fields":{"email":"INVALID_FORMAT"}}}}`
	if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("expected 400 %s, got %d %s", want, rec.Code, rec.Body.String())
	}
}

func TestRateLimitedSetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.RateLimited(rec, 1500*time.Millisecond, "slow down")

	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("expected 429 with Retry-After 2, got %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if env := decodeEnvelope(t, rec); env.Error.Code != "RATE_LIMITED" {
		t.Fatalf("expected RATE_LIMITED, got %+v", env)
	}
}

func TestInternalHidesError(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.Internal(rec, "db failed", errSecret{})

	env := decodeEnvelope(t, rec)
	if rec.Code != http.StatusInternalServerError || env.Error.Code != "INTERNAL" {
		t.Fatalf("expected 500 INTERNAL, got %d %+v", rec.Code, env)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("internal error leaked: %s", rec.Body.String())
	}
}

type errSecret struct{}

func (errSecret) Error() string { return "pq: password secret rejected" }

func TestDecodeError(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	tests := []struct {
		name       string
		body       string
		wantFields string
	}{
		{name: "empty body", body: ``, wantFields: ``},
		{name: "malformed", body: `{"name":`, wantFields: ``},
		{name: "unknown field", body: `{"nope":1}`, wantFields: `{"nope":"UNKNOWN_FIELD"}`},
		{name: "wrong type", body: `{"count":"x"}`, wantFields: `{"count":"INVALID_TYPE"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			var dst payload
			err := httpx.DecodeJSON(req, &dst)
			if err == nil {
				t.Fatal("expected a decode error")
			}
			rec := httptest.NewRecorder()
			httpx.DecodeError(rec, err)

			env := decodeEnvelope(t, rec)
			if rec.Code != http.StatusBadRequest || env.Error.Code != "VALIDATION_ERROR" {
				t.Fatalf("expected 400 VALIDATION_ERROR, got %d %+v", rec.Code, env)
			}
			if got := string(env.Error.Details["fields"]); got != tt.wantFields {
				t.Fatalf("expected fields %q, got %q", tt.wantFields, got)
			}
		})
	}
}

func TestRecovererWritesInternal(t *testing.T) {
	h := httpx.Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if env := decodeEnvelope(t, rec); rec.Code != http.StatusInternalServerError || env.Error.Code != "INTERNAL" {
		t.Fatalf("expected 500 INTERNAL, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestFallbackHandlers(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.NotFoundHandler(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if env := decodeEnvelope(t, rec); rec.Code != http.StatusNotFound || env.Error.Code != "NOT_FOUND" {
		t.Fatalf("expected 404 NOT_FOUND, got %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	httpx.MethodNotAllowedHandler(rec, httptest.NewRequest(http.MethodPut, "/", nil))
	if env := decodeEnvelope(t, rec); rec.Code != http.StatusMethodNotAllowed || env.Error.Code != "METHOD_NOT_ALLOWED" {
		t.Fatalf("expected 405 METHOD_NOT_ALLOWED, got %d %s", rec.Code, rec.Body.String())
	}
}
