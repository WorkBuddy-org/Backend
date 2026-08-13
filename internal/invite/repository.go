package invite

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const uniqueViolationCode = "23505"

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Create(ctx context.Context, code string, createdBy uuid.UUID) (*Invite, error) {
	const query = `
		INSERT INTO invites (code, created_by)
		VALUES ($1, $2)
		RETURNING id, code, created_by, used_by, used_at, created_at
	`

	var inv Invite
	err := r.db.QueryRow(ctx, query, code, createdBy).Scan(
		&inv.ID, &inv.Code, &inv.CreatedBy, &inv.UsedBy, &inv.UsedAt, &inv.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, errCodeCollision
		}
		return nil, fmt.Errorf("create invite: %w", err)
	}

	return &inv, nil
}

func (r *Repository) FindByCode(ctx context.Context, code string) (*Invite, error) {
	const query = `
		SELECT id, code, created_by, used_by, used_at, created_at
		FROM invites
		WHERE code = $1
	`

	var inv Invite
	err := r.db.QueryRow(ctx, query, code).Scan(
		&inv.ID, &inv.Code, &inv.CreatedBy, &inv.UsedBy, &inv.UsedAt, &inv.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInviteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find invite: %w", err)
	}

	return &inv, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode
}
