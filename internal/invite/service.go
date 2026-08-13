package invite

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

const maxCodeGenerationAttempts = 5

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Create(ctx context.Context, createdBy uuid.UUID) (*Invite, error) {
	for attempt := 0; attempt < maxCodeGenerationAttempts; attempt++ {
		code, err := generateCode()
		if err != nil {
			return nil, err
		}

		inv, err := s.repo.Create(ctx, code, createdBy)
		if err == nil {
			return inv, nil
		}
		if !errors.Is(err, errCodeCollision) {
			return nil, err
		}
	}

	return nil, fmt.Errorf("could not generate a unique invite code after %d attempts", maxCodeGenerationAttempts)
}

type ValidityResult struct {
	Valid  bool
	Reason string
}

// CheckValidity reports whether a code exists and hasn't been redeemed. It
// never marks the invite as used — see the product decision documented in
// code.go.
func (s *Service) CheckValidity(ctx context.Context, code string) (*ValidityResult, error) {
	inv, err := s.repo.FindByCode(ctx, code)
	if errors.Is(err, ErrInviteNotFound) {
		return &ValidityResult{Valid: false, Reason: "not_found"}, nil
	}
	if err != nil {
		return nil, err
	}
	if inv.UsedAt != nil {
		return &ValidityResult{Valid: false, Reason: "already_used"}, nil
	}

	return &ValidityResult{Valid: true}, nil
}
