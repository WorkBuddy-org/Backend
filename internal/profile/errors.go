package profile

import "errors"

var (
	ErrUserNotFound = errors.New("user not found")
	ErrNoFields     = errors.New("no fields to update")

	ErrInvalidName     = errors.New("name must be 1-100 characters")
	ErrRoleTooLong     = errors.New("role must be at most 100 characters")
	ErrStackTooLong    = errors.New("stack must be at most 200 characters")
	ErrInvalidTimezone = errors.New("timezone must be a valid IANA time zone name, e.g. Europe/Berlin")
)
