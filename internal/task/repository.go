package task

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	Text      *string
	Completed *bool
}

func (r *Repository) Create(ctx context.Context, userID, roomID uuid.UUID, sessionID *uuid.UUID, text string) (*Task, error) {
	const query = `
		INSERT INTO tasks (user_id, room_id, session_id, text)
		VALUES ($1, $2, $3, $4)
		RETURNING id, user_id, room_id, session_id, text, completed_at, created_at, updated_at, extended_count, extra_minutes_used
	`

	row := r.db.QueryRow(ctx, query, userID, roomID, sessionID, text)
	return scanTask(row)
}

func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*Task, error) {
	const query = `
		SELECT id, user_id, room_id, session_id, text, completed_at, created_at, updated_at, extended_count, extra_minutes_used
		FROM tasks
		WHERE id = $1
	`

	row := r.db.QueryRow(ctx, query, id)
	return scanTask(row)
}

func (r *Repository) ListByUser(ctx context.Context, userID uuid.UUID) ([]Task, error) {
	const query = `
		SELECT id, user_id, room_id, session_id, text, completed_at, created_at, updated_at, extended_count, extra_minutes_used
		FROM tasks
		WHERE user_id = $1
		ORDER BY created_at DESC
	`

	rows, err := r.db.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	return tasks, nil
}

func (r *Repository) Update(ctx context.Context, id uuid.UUID, update Update) (*Task, error) {
	const query = `
		UPDATE tasks
		SET
			text = COALESCE($2::text, text),
			completed_at = CASE
				WHEN $3::boolean IS NULL THEN completed_at
				WHEN $3::boolean THEN now()
				ELSE NULL
			END,
			updated_at = now()
		WHERE id = $1
		RETURNING id, user_id, room_id, session_id, text, completed_at, created_at, updated_at, extended_count, extra_minutes_used
	`

	row := r.db.QueryRow(ctx, query, id, update.Text, update.Completed)
	return scanTask(row)
}

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	err := row.Scan(
		&t.ID, &t.UserID, &t.RoomID, &t.SessionID, &t.Text, &t.CompletedAt,
		&t.CreatedAt, &t.UpdatedAt, &t.ExtendedCount, &t.ExtraMinutesUsed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan task: %w", err)
	}

	return &t, nil
}
