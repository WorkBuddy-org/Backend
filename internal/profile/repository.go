package profile

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/workbuddy/backend/internal/user"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// Update holds the fields a caller wants to change; nil fields are left
// untouched.
type Update struct {
	Name     *string
	Role     *string
	Stack    *string
	Timezone *string
}

func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*user.User, error) {
	row := r.db.QueryRow(ctx, `SELECT `+user.Columns+` FROM users WHERE id = $1`, id)
	return scanUser(row)
}

func (r *Repository) Update(ctx context.Context, id uuid.UUID, update Update) (*user.User, error) {
	query := `
		UPDATE users
		SET
			name = COALESCE($2, name),
			role = COALESCE($3, role),
			stack = COALESCE($4, stack),
			timezone = COALESCE($5, timezone)
		WHERE id = $1
		RETURNING ` + user.Columns

	row := r.db.QueryRow(ctx, query, id, update.Name, update.Role, update.Stack, update.Timezone)
	return scanUser(row)
}

// OnboardingUpdate holds validated onboarding values. Nil Goal, Experience and
// Profession are left unchanged; ProfessionOther is always written, because
// the service has resolved it against the resulting profession.
type OnboardingUpdate struct {
	Goal            *string
	Experience      *string
	Profession      *string
	ProfessionOther *string
}

// UpdateOnboarding applies the update. Once the user has a profession,
// onboarding counts as completed.
func (r *Repository) UpdateOnboarding(ctx context.Context, id uuid.UUID, update OnboardingUpdate) (*user.User, error) {
	query := `
		UPDATE users
		SET
			goal = COALESCE($2, goal),
			experience = COALESCE($3, experience),
			profession = COALESCE($4, profession),
			profession_other = $5,
			onboarding_completed = onboarding_completed OR COALESCE($4, profession) IS NOT NULL
		WHERE id = $1
		RETURNING ` + user.Columns

	row := r.db.QueryRow(ctx, query, id, update.Goal, update.Experience, update.Profession, update.ProfessionOther)
	return scanUser(row)
}

// CompleteOnboarding marks onboarding completed without touching its fields.
func (r *Repository) CompleteOnboarding(ctx context.Context, id uuid.UUID) (*user.User, error) {
	query := `UPDATE users SET onboarding_completed = true WHERE id = $1 RETURNING ` + user.Columns
	return scanUser(r.db.QueryRow(ctx, query, id))
}

func scanUser(row pgx.Row) (*user.User, error) {
	u, err := user.Scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return u, nil
}
