package call

import "errors"

var (
	ErrNotConfigured = errors.New("call service not configured")
	ErrNotInRoom     = errors.New("join the room before joining its call")
)
