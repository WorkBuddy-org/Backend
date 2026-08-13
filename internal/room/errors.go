package room

import "errors"

var (
	ErrRoomNotFound = errors.New("room not found")
	ErrNotInRoom    = errors.New("not currently in this room")
)
