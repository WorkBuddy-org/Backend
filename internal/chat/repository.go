package chat

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// Create stores a message and returns it with the author's current name.
func (r *Repository) Create(ctx context.Context, roomID, userID uuid.UUID, text string) (*Message, error) {
	const query = `
		WITH ins AS (
			INSERT INTO chat_messages (room_id, user_id, text)
			VALUES ($1, $2, $3)
			RETURNING id, room_id, user_id, text, created_at
		)
		SELECT ins.id, ins.room_id, ins.user_id, u.name, ins.text, ins.created_at
		FROM ins
		JOIN users u ON u.id = ins.user_id
	`

	var m Message
	err := r.db.QueryRow(ctx, query, roomID, userID, text).
		Scan(&m.ID, &m.RoomID, &m.UserID, &m.UserName, &m.Text, &m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create chat message: %w", err)
	}
	return &m, nil
}

// List returns up to limit messages of the room, newest first, ordered by
// (created_at, id). With before set, only messages older than that message
// are returned; ErrInvalidCursor if it is not a message of this room.
func (r *Repository) List(ctx context.Context, roomID uuid.UUID, before *uuid.UUID, limit int) ([]Message, error) {
	if before != nil {
		var exists bool
		err := r.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM chat_messages WHERE id = $1 AND room_id = $2)`,
			*before, roomID).Scan(&exists)
		if err != nil {
			return nil, fmt.Errorf("check chat cursor: %w", err)
		}
		if !exists {
			return nil, ErrInvalidCursor
		}
	}

	const query = `
		SELECT m.id, m.room_id, m.user_id, u.name, m.text, m.created_at
		FROM chat_messages m
		JOIN users u ON u.id = m.user_id
		WHERE m.room_id = $1
			AND ($2::uuid IS NULL OR (m.created_at, m.id) < (
				SELECT c.created_at, c.id FROM chat_messages c WHERE c.id = $2
			))
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT $3
	`

	rows, err := r.db.Query(ctx, query, roomID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list chat messages: %w", err)
	}
	defer rows.Close()

	messages := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.RoomID, &m.UserID, &m.UserName, &m.Text, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan chat message: %w", err)
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chat messages: %w", err)
	}
	return messages, nil
}
