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

// Filter narrows ListRooms results. A zero value matches every room.
type Filter struct {
	Type string
	Tags []string
}

func (r *Repository) ListRooms(ctx context.Context, filter Filter) ([]Room, error) {
	const query = `
		SELECT id, name, type, tags, created_at
		FROM rooms
		WHERE ($1::text IS NULL OR type = $1)
		  AND ($2::text[] IS NULL OR tags && $2)
		ORDER BY name
	`

	var typeParam *string
	if filter.Type != "" {
		typeParam = &filter.Type
	}
	var tagsParam []string
	if len(filter.Tags) > 0 {
		tagsParam = filter.Tags
	}

	rows, err := r.db.Query(ctx, query, typeParam, tagsParam)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	defer rows.Close()

	var rooms []Room
	for rows.Next() {
		var rm Room
		if err := rows.Scan(&rm.ID, &rm.Name, &rm.Type, &rm.Tags, &rm.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan room: %w", err)
		}
		rooms = append(rooms, rm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}

	return rooms, nil
}

func (r *Repository) FindRoomByID(ctx context.Context, id uuid.UUID) (*Room, error) {
	const query = `
		SELECT id, name, type, tags, created_at
		FROM rooms
		WHERE id = $1
	`

	var rm Room
	err := r.db.QueryRow(ctx, query, id).Scan(&rm.ID, &rm.Name, &rm.Type, &rm.Tags, &rm.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRoomNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find room: %w", err)
	}

	return &rm, nil
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

// FindOpenParticipation returns the user's currently open participation in
// the room, or (nil, nil) if they have none.
func (r *Repository) FindOpenParticipation(ctx context.Context, userID, roomID uuid.UUID) (*Participation, error) {
	const query = `
		SELECT id, user_id, room_id, session_id, joined_at, left_at, left_reason
		FROM room_participations
		WHERE user_id = $1 AND room_id = $2 AND left_at IS NULL
		ORDER BY joined_at DESC
		LIMIT 1
	`

	var p Participation
	err := r.db.QueryRow(ctx, query, userID, roomID).Scan(
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

func (r *Repository) CreateParticipation(ctx context.Context, userID, roomID uuid.UUID, sessionID *uuid.UUID) (*Participation, error) {
	const query = `
		INSERT INTO room_participations (user_id, room_id, session_id)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, room_id, session_id, joined_at, left_at, left_reason
	`

	var p Participation
	err := r.db.QueryRow(ctx, query, userID, roomID, sessionID).Scan(
		&p.ID, &p.UserID, &p.RoomID, &p.SessionID, &p.JoinedAt, &p.LeftAt, &p.LeftReason,
	)
	if err != nil {
		return nil, fmt.Errorf("create participation: %w", err)
	}

	return &p, nil
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
