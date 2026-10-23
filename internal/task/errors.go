package task

import "errors"

var (
	ErrTaskNotFound = errors.New("task not found")
	ErrForbidden    = errors.New("not the owner of this task")
	ErrTextRequired = errors.New("text is required")
	ErrNoFields     = errors.New("no fields to update")

	ErrInvalidDuration = errors.New("duration_minutes must be one of 25, 50, 90")
	ErrTaskCompleted   = errors.New("task is already completed")
	ErrNotInRoom       = errors.New("join the task's room before starting a timer")
	ErrTimerActive     = errors.New("a timer is already running for this user")

	ErrInvalidExtension = errors.New("minutes must be one of 15, 30")
	ErrNotPrompting     = errors.New("the task's timer is not waiting for an extension answer")
	ErrPromptExpired    = errors.New("the extension prompt was already answered or has expired")
	ErrExtensionLimit   = errors.New("extension limit reached for this task")
)
