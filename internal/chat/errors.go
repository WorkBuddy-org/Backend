package chat

import (
	"errors"
	"time"
)

var (
	ErrTextRequired  = errors.New("text is required")
	ErrTextTooLong   = errors.New("text must be at most 2000 characters")
	ErrNotInRoom     = errors.New("join the room before using its chat")
	ErrInvalidCursor = errors.New("before must be a message id from this room")
)

// RateLimitedError means the user sent too many messages; RetryAfter is how
// long until the next one is accepted.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return "too many messages, slow down"
}
