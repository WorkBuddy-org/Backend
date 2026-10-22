package profile

import (
	"context"
	"strings"
	"time"
	// Embedded zone data: the runtime image has no system tzdata, and
	// timezone validation must not depend on the host.
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/user"
)

// Field limits for PATCH /profile, in characters (runes) after trimming.
const (
	MaxNameLength  = 100
	MaxRoleLength  = 100
	MaxStackLength = 200
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Get(ctx context.Context, userID uuid.UUID) (*user.User, error) {
	return s.repo.FindByID(ctx, userID)
}

func (s *Service) Update(ctx context.Context, userID uuid.UUID, update Update) (*user.User, error) {
	if update.Name == nil && update.Role == nil && update.Stack == nil && update.Timezone == nil {
		return nil, ErrNoFields
	}

	update.Name = trimmed(update.Name)
	update.Role = trimmed(update.Role)
	update.Stack = trimmed(update.Stack)
	update.Timezone = trimmed(update.Timezone)

	if err := validate(update); err != nil {
		return nil, err
	}

	return s.repo.Update(ctx, userID, update)
}

// validate checks the trimmed fields that are present. role and stack may be
// set to "" (cleared); name and timezone may not.
func validate(update Update) error {
	if n := update.Name; n != nil && (*n == "" || utf8.RuneCountInString(*n) > MaxNameLength) {
		return ErrInvalidName
	}
	if r := update.Role; r != nil && utf8.RuneCountInString(*r) > MaxRoleLength {
		return ErrRoleTooLong
	}
	if s := update.Stack; s != nil && utf8.RuneCountInString(*s) > MaxStackLength {
		return ErrStackTooLong
	}
	if tz := update.Timezone; tz != nil && !validTimezone(*tz) {
		return ErrInvalidTimezone
	}
	return nil
}

// validTimezone accepts IANA names such as "Europe/Berlin" or "UTC".
// LoadLocation also maps "" to UTC and "Local" to the server's zone; neither
// is a real zone name, so both are rejected.
func validTimezone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}
