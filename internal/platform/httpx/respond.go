package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

func JSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("failed to encode json response", "error", err)
	}
}

// RetryAfter sets the Retry-After header to d in whole seconds, rounded up
// and at least 1, as the header takes an integer. Call it before writing a
// 429.
func RetryAfter(w http.ResponseWriter, d time.Duration) {
	secs := max(int((d+time.Second-1)/time.Second), 1)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}
