-- +goose Up
CREATE UNIQUE INDEX idx_room_sessions_one_active_per_room
    ON room_sessions (room_id) WHERE ended_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_room_sessions_one_active_per_room;
