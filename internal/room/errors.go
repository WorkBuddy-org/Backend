package room

import "errors"

var (
	ErrRoomNotFound = errors.New("room not found")
	ErrNotInRoom    = errors.New("not currently in this room")
	ErrNotMember    = errors.New("join the room to see its participants")
	ErrRoomFull     = errors.New("the room is full")
	ErrRoomEnded    = errors.New("the room has ended")
)
