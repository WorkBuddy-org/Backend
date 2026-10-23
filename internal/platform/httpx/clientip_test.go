package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/workbuddy/backend/internal/platform/httpx"
)

func TestTrustedProxy(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
		want   string
	}{
		{name: "no header configured ignores X-Forwarded-For", header: "", value: "203.0.113.7", want: "192.0.2.1"},
		{name: "single value", header: "X-Forwarded-For", value: "203.0.113.7", want: "203.0.113.7"},
		{name: "first of several values", header: "X-Forwarded-For", value: " 203.0.113.7 , 10.0.0.2", want: "203.0.113.7"},
		{name: "ipv6", header: "X-Forwarded-For", value: "2001:db8::1", want: "2001:db8::1"},
		{name: "missing header keeps connection address", header: "X-Forwarded-For", value: "", want: "192.0.2.1"},
		{name: "garbage keeps connection address", header: "X-Forwarded-For", value: "not-an-ip, 203.0.113.7", want: "192.0.2.1"},
		{name: "custom header", header: "X-Real-IP", value: "198.51.100.4", want: "198.51.100.4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			h := httpx.TrustedProxy(tt.header)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = httpx.ClientIP(r)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "192.0.2.1:4321"
			if tt.value != "" {
				name := tt.header
				if name == "" {
					name = "X-Forwarded-For"
				}
				req.Header.Set(name, tt.value)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got != tt.want {
				t.Fatalf("client ip = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPWithoutPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.9"
	if got := httpx.ClientIP(req); got != "192.0.2.9" {
		t.Fatalf("client ip = %q, want 192.0.2.9", got)
	}
}
