package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/workbuddy/backend/internal/user"
)

type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) FindUserByOAuth(ctx context.Context, provider user.OAuthProvider, oauthID string) (*user.User, error) {
	const query = `
		SELECT id, email, name, oauth_provider, oauth_id, role, stack, timezone, avatar_status, created_at, password_hash
		FROM users
		WHERE oauth_provider = $1 AND oauth_id = $2
	`

	row := r.db.QueryRow(ctx, query, string(provider), oauthID)
	return scanUser(row)
}

func (r *Repository) FindUserByEmail(ctx context.Context, email string) (*user.User, error) {
	const query = `
		SELECT id, email, name, oauth_provider, oauth_id, role, stack, timezone, avatar_status, created_at, password_hash
		FROM users
		WHERE email = $1
	`

	row := r.db.QueryRow(ctx, query, email)
	return scanUser(row)
}

func (r *Repository) FindUserByID(ctx context.Context, id uuid.UUID) (*user.User, error) {
	const query = `
		SELECT id, email, name, oauth_provider, oauth_id, role, stack, timezone, avatar_status, created_at, password_hash
		FROM users
		WHERE id = $1
	`

	row := r.db.QueryRow(ctx, query, id)
	return scanUser(row)
}

// FindUserNamesByIDs returns display names keyed by user id. Unknown ids are
// absent from the map.
func (r *Repository) FindUserNamesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	names := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}

	const query = `SELECT id, name FROM users WHERE id = ANY($1)`
	rows, err := r.db.Query(ctx, query, ids)
	if err != nil {
		return nil, fmt.Errorf("find user names: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan user name: %w", err)
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user names: %w", err)
	}

	return names, nil
}

func (r *Repository) CreateUser(ctx context.Context, profile *OAuthProfile) (*user.User, error) {
	const query = `
		INSERT INTO users (email, name, oauth_provider, oauth_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, email, name, oauth_provider, oauth_id, role, stack, timezone, avatar_status, created_at, password_hash
	`

	row := r.db.QueryRow(ctx, query, profile.Email, profile.Name, string(profile.Provider), profile.OAuthID)
	return scanUser(row)
}

func (r *Repository) CreateEmailUser(ctx context.Context, email, name, passwordHash string) (*user.User, error) {
	const query = `
		INSERT INTO users (email, name, oauth_provider, oauth_id, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, email, name, oauth_provider, oauth_id, role, stack, timezone, avatar_status, created_at, password_hash
	`

	row := r.db.QueryRow(ctx, query, email, name, string(user.OAuthProviderEmail), email, passwordHash)
	return scanUser(row)
}

func (r *Repository) CreateRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	const query = `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
	`

	_, err := r.db.Exec(ctx, query, userID, tokenHash, expiresAt)
	if err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	return nil
}

func (r *Repository) FindRefreshTokenByHash(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	const query = `
		SELECT id, user_id, token_hash, expires_at, revoked_at
		FROM refresh_tokens
		WHERE token_hash = $1
	`

	var token RefreshToken
	err := r.db.QueryRow(ctx, query, tokenHash).Scan(
		&token.ID,
		&token.UserID,
		&token.TokenHash,
		&token.ExpiresAt,
		&token.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("find refresh token: %w", err)
	}

	return &token, nil
}

func (r *Repository) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	const query = `
		UPDATE refresh_tokens
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`

	tag, err := r.db.Exec(ctx, query, tokenHash)
	if err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInvalidToken
	}
	return nil
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
		&u.PasswordHash,
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
