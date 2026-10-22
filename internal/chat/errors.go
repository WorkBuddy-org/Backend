package chat

import "errors"

var (
	ErrTextRequired  = errors.New("text is required")
	ErrTextTooLong   = errors.New("text must be at most 2000 characters")
	ErrNotInRoom     = errors.New("join the room before using its chat")
	ErrInvalidCursor = errors.New("before must be a message id from this room")
)
