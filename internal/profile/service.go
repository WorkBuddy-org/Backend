package profile

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/user"
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

	return s.repo.Update(ctx, userID, update)
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}
