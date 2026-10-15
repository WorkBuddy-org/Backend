package profile

import "errors"

var (
	ErrUserNotFound = errors.New("user not found")
	ErrNoFields     = errors.New("no fields to update")
)
