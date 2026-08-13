package invite

import "errors"

var (
	ErrInviteNotFound = errors.New("invite not found")

	// errCodeCollision signals a unique-constraint collision on the
	// generated code; callers retry with a freshly generated code.
	errCodeCollision = errors.New("invite code collision")
)
