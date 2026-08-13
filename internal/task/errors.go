package task

import "errors"

var (
	ErrTaskNotFound = errors.New("task not found")
	ErrForbidden    = errors.New("not the owner of this task")
	ErrTextRequired = errors.New("text is required")
)
