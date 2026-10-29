package httpx

import (
	"net"
	"net/http"
	"strings"
)

// TrustedProxy returns middleware that replaces r.RemoteAddr with the first
// value of header when that value is a valid IP, so rate limits and logs see
// the client behind a reverse proxy. An empty header returns a no-op: the
// connection address is kept. Only use it when every request passes through
// a proxy that overwrites the header; otherwise clients can spoof it.
func TrustedProxy(header string) func(http.Handler) http.Handler {
	if header == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := firstHeaderIP(r.Header.Get(header)); ip != "" {
				r.RemoteAddr = net.JoinHostPort(ip, "0")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP returns the IP part of r.RemoteAddr, or RemoteAddr as is when it
// has no port.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// firstHeaderIP returns the first comma-separated entry of value if it
// parses as an IP, normalised, or "" otherwise.
func firstHeaderIP(value string) string {
	first, _, _ := strings.Cut(value, ",")
	ip := net.ParseIP(strings.TrimSpace(first))
	if ip == nil {
		return ""
	}
	return ip.String()
}
