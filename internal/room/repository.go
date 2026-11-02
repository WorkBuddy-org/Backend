package room

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

// Filter narrows ListRoomViews results. A zero value matches every room.
type Filter struct {
	ID   *uuid.UUID
	Type string
	Tags []string
}

const roomColumns = `r.id, r.name, r.type, r.tags, r.capacity, r.starts_at, r.ends_at, r.created_at`

func roomScanTargets(rm *Room) []any {
	return []any{&rm.ID, &rm.Name, &rm.Type, &rm.Tags, &rm.Capacity, &rm.StartsAt, &rm.EndsAt, &rm.CreatedAt}
}

// ListRoomViews returns the matching rooms by name, with their participant
// count and first participants. Status is left for the service to compute.
func (r *Repository) ListRoomViews(ctx context.Context, filter Filter) ([]View, error) {
	query := `
		SELECT ` + roomColumns + `,
			(SELECT count(DISTINCT p.user_id) FROM room_participations p
			 WHERE p.room_id = r.id AND p.left_at IS NULL)
		FROM rooms r
		WHERE ($1::uuid IS NULL OR r.id = $1)
		  AND ($2::text IS NULL OR r.type = $2)
		  AND ($3::text[] IS NULL OR r.tags && $3)
		ORDER BY r.name
	`

	var typeParam *string
	if filter.Type != "" {
		typeParam = &filter.Type
	}
	var tagsParam []string
	if len(filter.Tags) > 0 {
		tagsParam = filter.Tags
	}

	rows, err := r.db.Query(ctx, query, filter.ID, typeParam, tagsParam)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	defer rows.Close()

	views := []View{}
	ids := []uuid.UUID{}
	for rows.Next() {
		var v View
		if err := rows.Scan(append(roomScanTargets(&v.Room), &v.ParticipantsCount)...); err != nil {
			return nil, fmt.Errorf("scan room: %w", err)
		}
		v.Participants = []ParticipantPreview{}
		views = append(views, v)
		ids = append(ids, v.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}

	previews, err := r.participantPreviews(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range views {
		if p, ok := previews[views[i].ID]; ok {
			views[i].Participants = p
		}
	}
	return views, nil
}

// participantPreviews returns each room's earliest joiners, at most
// MaxPreviewParticipants per room.
func (r *Repository) participantPreviews(ctx context.Context, roomIDs []uuid.UUID) (map[uuid.UUID][]ParticipantPreview, error) {
	const query = `
		SELECT room_id, user_id, name, avatar_url
		FROM (
			SELECT o.*, row_number() OVER (PARTITION BY o.room_id ORDER BY o.joined_at, o.user_id) AS position
			FROM (
				SELECT DISTINCT ON (p.room_id, p.user_id) p.room_id, p.user_id, u.name, u.avatar_url, p.joined_at
				FROM room_participations p
				JOIN users u ON u.id = p.user_id
				WHERE p.room_id = ANY($1) AND p.left_at IS NULL
				ORDER BY p.room_id, p.user_id, p.joined_at
			) o
		) ranked
		WHERE position <= $2
		ORDER BY room_id, position
	`

	rows, err := r.db.Query(ctx, query, roomIDs, MaxPreviewParticipants)
	if err != nil {
		return nil, fmt.Errorf("list participant previews: %w", err)
	}
	defer rows.Close()

	previews := make(map[uuid.UUID][]ParticipantPreview)
	for rows.Next() {
		var roomID uuid.UUID
		var p ParticipantPreview
		if err := rows.Scan(&roomID, &p.UserID, &p.Name, &p.AvatarURL); err != nil {
			return nil, fmt.Errorf("scan participant preview: %w", err)
		}
		previews[roomID] = append(previews[roomID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list participant previews: %w", err)
	}
	return previews, nil
}

func (r *Repository) FindRoomByID(ctx context.Context, id uuid.UUID) (*Room, error) {
	query := `SELECT ` + roomColumns + ` FROM rooms r WHERE r.id = $1`

	var rm Room
	err := r.db.QueryRow(ctx, query, id).Scan(roomScanTargets(&rm)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRoomNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find room: %w", err)
	}

	return &rm, nil
}

// JoinRoom admits the user to the room in one transaction holding the room
// row's lock, so the capacity check can't race with another join. A user who
// already has an open participation gets it back with created=false, without
// consulting check. Otherwise check decides, given the room and how many users
// are in it.
func (r *Repository) JoinRoom(ctx context.Context, userID, roomID uuid.UUID, check func(rm *Room, count int) error) (*Participation, bool, error) {
	var participation *Participation
	created := false

	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		var rm Room
		err := tx.QueryRow(ctx, `SELECT `+roomColumns+` FROM rooms r WHERE r.id = $1 FOR UPDATE`, roomID).
			Scan(roomScanTargets(&rm)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRoomNotFound
		}
		if err != nil {
			return fmt.Errorf("lock room: %w", err)
		}

		existing, err := findOpenParticipation(ctx, tx, userID, roomID)
		if err != nil {
			return err
		}
		if existing != nil {
			participation = existing
			return nil
		}

		var count int
		err = tx.QueryRow(ctx,
			`SELECT count(DISTINCT user_id) FROM room_participations WHERE room_id = $1 AND left_at IS NULL`,
			roomID).Scan(&count)
		if err != nil {
			return fmt.Errorf("count participants: %w", err)
		}
		if err := check(&rm, count); err != nil {
			return err
		}

		var sessionID *uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM room_sessions WHERE room_id = $1 AND ended_at IS NULL`, roomID).Scan(&sessionID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find active session: %w", err)
		}

		var p Participation
		err = tx.QueryRow(ctx, `
			INSERT INTO room_participations (user_id, room_id, session_id)
			VALUES ($1, $2, $3)
			RETURNING id, user_id, room_id, session_id, joined_at, left_at, left_reason
		`, userID, roomID, sessionID).Scan(
			&p.ID, &p.UserID, &p.RoomID, &p.SessionID, &p.JoinedAt, &p.LeftAt, &p.LeftReason,
		)
		if err != nil {
			return fmt.Errorf("create participation: %w", err)
		}
		participation = &p
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return participation, created, nil
}

// FindActiveSession returns the room's currently open session, or (nil, nil)
// if the room has none.
func (r *Repository) FindActiveSession(ctx context.Context, roomID uuid.UUID) (*Session, error) {
	const query = `
		SELECT id, room_id, started_at, ended_at, status
		FROM room_sessions
		WHERE room_id = $1 AND ended_at IS NULL
	`

	var s Session
	err := r.db.QueryRow(ctx, query, roomID).Scan(&s.ID, &s.RoomID, &s.StartedAt, &s.EndedAt, &s.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find active session: %w", err)
	}

	return &s, nil
}

// rowQuerier is what the pool and a transaction have in common.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// FindOpenParticipation returns the user's currently open participation in
// the room, or (nil, nil) if they have none.
func (r *Repository) FindOpenParticipation(ctx context.Context, userID, roomID uuid.UUID) (*Participation, error) {
	return findOpenParticipation(ctx, r.db, userID, roomID)
}

func findOpenParticipation(ctx context.Context, q rowQuerier, userID, roomID uuid.UUID) (*Participation, error) {
	const query = `
		SELECT id, user_id, room_id, session_id, joined_at, left_at, left_reason
		FROM room_participations
		WHERE user_id = $1 AND room_id = $2 AND left_at IS NULL
		ORDER BY joined_at DESC
		LIMIT 1
	`

	var p Participation
	err := q.QueryRow(ctx, query, userID, roomID).Scan(
		&p.ID, &p.UserID, &p.RoomID, &p.SessionID, &p.JoinedAt, &p.LeftAt, &p.LeftReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find open participation: %w", err)
	}

	return &p, nil
}

// ListParticipants returns the users with an open participation in the room,
// earliest joiner first. A user with several open rows appears once, with the
// earliest joined_at.
func (r *Repository) ListParticipants(ctx context.Context, roomID uuid.UUID) ([]Participant, error) {
	const query = `
		SELECT user_id, name, joined_at
		FROM (
			SELECT DISTINCT ON (p.user_id) p.user_id, u.name, p.joined_at
			FROM room_participations p
			JOIN users u ON u.id = p.user_id
			WHERE p.room_id = $1 AND p.left_at IS NULL
			ORDER BY p.user_id, p.joined_at
		) open
		ORDER BY joined_at, user_id
	`

	rows, err := r.db.Query(ctx, query, roomID)
	if err != nil {
		return nil, fmt.Errorf("list participants: %w", err)
	}
	defer rows.Close()

	participants := []Participant{}
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.UserID, &p.Name, &p.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		participants = append(participants, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list participants: %w", err)
	}
	return participants, nil
}

func (r *Repository) CloseParticipation(ctx context.Context, id uuid.UUID, reason string) error {
	const query = `
		UPDATE room_participations
		SET left_at = now(), left_reason = $2
		WHERE id = $1 AND left_at IS NULL
	`

	tag, err := r.db.Exec(ctx, query, id, reason)
	if err != nil {
		return fmt.Errorf("close participation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInRoom
	}

	return nil
}
