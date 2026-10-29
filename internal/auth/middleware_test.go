package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRequireAuthErrorCodes(t *testing.T) {
	const secret = "test-secret"
	valid := NewTokenManager(secret, time.Minute)
	expired := NewTokenManager(secret, -time.Minute)
	otherKeyExpired := NewTokenManager("other-secret", -time.Minute)

	issue := func(m *TokenManager) string {
		tok, err := m.IssueAccessToken(uuid.New(), "dev@example.com")
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		return tok
	}

	mw := NewMiddleware(valid)
	h := mw.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name       string
		header     string
		wantStatus int
		wantCode   string
	}{
		{name: "valid", header: "Bearer " + issue(valid), wantStatus: http.StatusNoContent},
		{name: "missing", header: "", wantStatus: http.StatusUnauthorized, wantCode: "UNAUTHORIZED"},
		{name: "garbage", header: "Bearer not-a-jwt", wantStatus: http.StatusUnauthorized, wantCode: "UNAUTHORIZED"},
		{name: "expired", header: "Bearer " + issue(expired), wantStatus: http.StatusUnauthorized, wantCode: "TOKEN_EXPIRED"},
		// A bad signature wins over expiry: the token is not ours at all.
		{name: "expired with wrong key", header: "Bearer " + issue(otherKeyExpired), wantStatus: http.StatusUnauthorized, wantCode: "UNAUTHORIZED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tt.wantStatus, rec.Code, rec.Body.String())
			}
			if tt.wantCode == "" {
				return
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != tt.wantCode {
				t.Fatalf("expected code %s, got %s", tt.wantCode, rec.Body.String())
			}
		})
	}
}
