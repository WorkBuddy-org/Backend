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
	const query = `
		SELECT id, email, name, oauth_provider, oauth_id, role, stack, timezone, avatar_status, created_at
		FROM users
		WHERE id = $1
	`

	row := r.db.QueryRow(ctx, query, id)
	return scanUser(row)
}

func (r *Repository) Update(ctx context.Context, id uuid.UUID, update Update) (*user.User, error) {
	const query = `
		UPDATE users
		SET
			name = COALESCE($2, name),
			role = COALESCE($3, role),
			stack = COALESCE($4, stack),
			timezone = COALESCE($5, timezone)
		WHERE id = $1
		RETURNING id, email, name, oauth_provider, oauth_id, role, stack, timezone, avatar_status, created_at
	`

	row := r.db.QueryRow(ctx, query, id, update.Name, update.Role, update.Stack, update.Timezone)
	return scanUser(row)
}

func scanUser(row pgx.Row) (*user.User, error) {
	var u user.User
	var provider string

	err := row.Scan(
		&u.ID,
		&u.Email,
		&u.Name,
		&provider,
		&u.OAuthID,
		&u.Role,
		&u.Stack,
		&u.Timezone,
		&u.AvatarStatus,
		&u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}

	u.OAuthProvider = user.OAuthProvider(provider)
	return &u, nil
}
