package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const uniqueViolationCode = "23505"

// taskColumns is the column list scanTask expects, in order.
const taskColumns = `id, user_id, room_id, session_id, text, completed_at, created_at, updated_at,
		extended_count, extra_minutes_used,
		timer_status, timer_duration_minutes, timer_started_at, timer_ends_at, prompt_deadline_at`

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
		RETURNING ` + taskColumns

	row := r.db.QueryRow(ctx, query, userID, roomID, sessionID, text)
	return scanTask(row)
}

// CreateWithTimer inserts a task whose timer is already running, as one
// statement: if the user has another active timer, the one-active-timer unique
// index rejects it with ErrTimerActive and no task is left behind.
func (r *Repository) CreateWithTimer(ctx context.Context, userID, roomID uuid.UUID, sessionID *uuid.UUID, text string, minutes int, startedAt, endsAt time.Time) (*Task, error) {
	const query = `
		INSERT INTO tasks (user_id, room_id, session_id, text, timer_status, timer_duration_minutes, timer_started_at, timer_ends_at)
		VALUES ($1, $2, $3, $4, 'running', $5, $6, $7)
		RETURNING ` + taskColumns

	t, err := scanTask(r.db.QueryRow(ctx, query, userID, roomID, sessionID, text, minutes, startedAt, endsAt))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode {
		return nil, ErrTimerActive
	}
	return t, err
}

// Delete removes the task.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM tasks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// ListByRoom returns the tasks in the room of users who are currently in it,
// newest first.
func (r *Repository) ListByRoom(ctx context.Context, roomID uuid.UUID) ([]RoomTask, error) {
	const query = `
		SELECT t.id, t.user_id, t.room_id, t.session_id, t.text, t.completed_at, t.created_at, t.updated_at,
			t.extended_count, t.extra_minutes_used,
			t.timer_status, t.timer_duration_minutes, t.timer_started_at, t.timer_ends_at, t.prompt_deadline_at,
			u.name, u.avatar_url
		FROM tasks t
		JOIN users u ON u.id = t.user_id
		WHERE t.room_id = $1
			AND EXISTS (
				SELECT 1 FROM room_participations p
				WHERE p.room_id = t.room_id AND p.user_id = t.user_id AND p.left_at IS NULL
			)
		ORDER BY t.created_at DESC, t.id DESC
	`

	rows, err := r.db.Query(ctx, query, roomID)
	if err != nil {
		return nil, fmt.Errorf("list room tasks: %w", err)
	}
	defer rows.Close()

	tasks := []RoomTask{}
	for rows.Next() {
		var t RoomTask
		err := rows.Scan(
			&t.ID, &t.UserID, &t.RoomID, &t.SessionID, &t.Text, &t.CompletedAt,
			&t.CreatedAt, &t.UpdatedAt, &t.ExtendedCount, &t.ExtraMinutesUsed,
			&t.TimerStatus, &t.TimerDurationMinutes, &t.TimerStartedAt, &t.TimerEndsAt, &t.PromptDeadlineAt,
			&t.AuthorName, &t.AuthorAvatarURL,
		)
		if err != nil {
			return nil, fmt.Errorf("scan room task: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list room tasks: %w", err)
	}
	return tasks, nil
}

// FindAuthor returns the display name and avatar of a task's owner.
func (r *Repository) FindAuthor(ctx context.Context, userID uuid.UUID) (string, *string, error) {
	var name string
	var avatarURL *string
	err := r.db.QueryRow(ctx, `SELECT name, avatar_url FROM users WHERE id = $1`, userID).Scan(&name, &avatarURL)
	if err != nil {
		return "", nil, fmt.Errorf("find task author: %w", err)
	}
	return name, avatarURL, nil
}

func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*Task, error) {
	const query = `
		SELECT ` + taskColumns + `
		FROM tasks
		WHERE id = $1
	`

	row := r.db.QueryRow(ctx, query, id)
	return scanTask(row)
}

// ListByUser returns one page of the user's tasks, newest first. id breaks
// ties so pages are stable.
func (r *Repository) ListByUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]Task, error) {
	const query = `
		SELECT ` + taskColumns + `
		FROM tasks
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := r.db.Query(ctx, query, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	return collectTasks(rows, "list tasks")
}

// Update applies the change. Completing a task also ends its timer if it is
// running or prompting.
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
			timer_status = CASE
				WHEN $3::boolean AND timer_status IN ('running', 'prompting') THEN 'ended'
				ELSE timer_status
			END,
			prompt_deadline_at = CASE
				WHEN $3::boolean AND timer_status IN ('running', 'prompting') THEN NULL
				ELSE prompt_deadline_at
			END,
			updated_at = now()
		WHERE id = $1
		RETURNING ` + taskColumns

	row := r.db.QueryRow(ctx, query, id, update.Text, update.Completed)
	return scanTask(row)
}

// StartTimer moves the task's timer to running. It returns ErrTimerActive
// when the task's own timer or another of the user's timers is already
// running or prompting (the latter via the one-active-timer unique index),
// and ErrTaskCompleted when the task is completed.
func (r *Repository) StartTimer(ctx context.Context, id uuid.UUID, minutes int, startedAt, endsAt time.Time) (*Task, error) {
	const query = `
		UPDATE tasks
		SET timer_status = 'running',
			timer_duration_minutes = $2,
			timer_started_at = $3,
			timer_ends_at = $4,
			prompt_deadline_at = NULL,
			updated_at = now()
		WHERE id = $1
			AND completed_at IS NULL
			AND timer_status NOT IN ('running', 'prompting')
		RETURNING ` + taskColumns

	t, err := scanTask(r.db.QueryRow(ctx, query, id, minutes, startedAt, endsAt))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolationCode {
		return nil, ErrTimerActive
	}
	if !errors.Is(err, ErrTaskNotFound) {
		return t, err
	}

	// No row matched: tell the caller which condition failed.
	existing, err := r.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.CompletedAt != nil {
		return nil, ErrTaskCompleted
	}
	return nil, ErrTimerActive
}

// ResetTimer puts a running timer back to idle. Used when the timer could not
// be scheduled after it was stored.
func (r *Repository) ResetTimer(ctx context.Context, id uuid.UUID) error {
	const query = `
		UPDATE tasks
		SET timer_status = 'idle',
			timer_duration_minutes = NULL,
			timer_started_at = NULL,
			timer_ends_at = NULL,
			updated_at = now()
		WHERE id = $1 AND timer_status = 'running'
	`

	if _, err := r.db.Exec(ctx, query, id); err != nil {
		return fmt.Errorf("reset timer: %w", err)
	}
	return nil
}

// ExpireTimer moves a running timer whose end has passed to prompting. It
// returns ErrTaskNotFound when there is nothing to expire: the timer is no
// longer running, or it was extended past now.
func (r *Repository) ExpireTimer(ctx context.Context, id uuid.UUID, now time.Time) (*Task, error) {
	const query = `
		UPDATE tasks
		SET timer_status = 'prompting', updated_at = now()
		WHERE id = $1 AND timer_status = 'running' AND timer_ends_at <= $2
		RETURNING ` + taskColumns

	return scanTask(r.db.QueryRow(ctx, query, id, now))
}

// SetPromptDeadline records when the extension prompt of a prompting timer
// runs out.
func (r *Repository) SetPromptDeadline(ctx context.Context, id uuid.UUID, deadline time.Time) (*Task, error) {
	const query = `
		UPDATE tasks
		SET prompt_deadline_at = $2, updated_at = now()
		WHERE id = $1 AND timer_status = 'prompting'
		RETURNING ` + taskColumns

	return scanTask(r.db.QueryRow(ctx, query, id, deadline))
}

// ExtendTimer puts a prompting timer back to running until endsAt, counting
// the extension. It returns ErrTaskNotFound when the timer is not prompting,
// the prompt deadline is not after now, maxCount is reached, or the extension
// would take the task over maxTotalMinutes.
func (r *Repository) ExtendTimer(ctx context.Context, id uuid.UUID, minutes int, now, endsAt time.Time, maxCount, maxTotalMinutes int) (*Task, error) {
	const query = `
		UPDATE tasks
		SET timer_status = 'running',
			timer_ends_at = $4,
			prompt_deadline_at = NULL,
			extended_count = extended_count + 1,
			extra_minutes_used = extra_minutes_used + $2,
			updated_at = now()
		WHERE id = $1
			AND timer_status = 'prompting'
			AND (prompt_deadline_at IS NULL OR prompt_deadline_at > $3)
			AND extended_count < $5
			AND extra_minutes_used + $2 <= $6
		RETURNING ` + taskColumns

	return scanTask(r.db.QueryRow(ctx, query, id, minutes, now, endsAt, maxCount, maxTotalMinutes))
}

// EndPrompt ends a prompting timer (the user declined or did not answer).
// With dueBy set, it only acts if the prompt deadline is at or before dueBy.
// It returns ErrTaskNotFound when there is nothing to end.
func (r *Repository) EndPrompt(ctx context.Context, id uuid.UUID, dueBy *time.Time) (*Task, error) {
	const query = `
		UPDATE tasks
		SET timer_status = 'ended', prompt_deadline_at = NULL, updated_at = now()
		WHERE id = $1
			AND timer_status = 'prompting'
			AND ($2::timestamptz IS NULL OR prompt_deadline_at IS NULL OR prompt_deadline_at <= $2)
		RETURNING ` + taskColumns

	return scanTask(r.db.QueryRow(ctx, query, id, dueBy))
}

// ListActiveTimers returns every task whose timer is running or prompting.
func (r *Repository) ListActiveTimers(ctx context.Context) ([]Task, error) {
	const query = `
		SELECT ` + taskColumns + `
		FROM tasks
		WHERE timer_status IN ('running', 'prompting')
	`

	rows, err := r.db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list active timers: %w", err)
	}
	return collectTasks(rows, "list active timers")
}

func collectTasks(rows pgx.Rows, op string) ([]Task, error) {
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
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	return tasks, nil
}

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	err := row.Scan(
		&t.ID, &t.UserID, &t.RoomID, &t.SessionID, &t.Text, &t.CompletedAt,
		&t.CreatedAt, &t.UpdatedAt, &t.ExtendedCount, &t.ExtraMinutesUsed,
		&t.TimerStatus, &t.TimerDurationMinutes, &t.TimerStartedAt, &t.TimerEndsAt, &t.PromptDeadlineAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan task: %w", err)
	}

	return &t, nil
}
