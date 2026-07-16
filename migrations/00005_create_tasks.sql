-- +goose Up
CREATE TABLE tasks (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    room_id      UUID NOT NULL REFERENCES rooms(id) ON DELETE RESTRICT,
    session_id   UUID REFERENCES room_sessions(id) ON DELETE SET NULL,
    text         TEXT NOT NULL,
    completed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_tasks_user_id_created_at ON tasks (user_id, created_at DESC);
CREATE INDEX idx_tasks_room_id ON tasks (room_id);
CREATE INDEX idx_tasks_session_id ON tasks (session_id);

-- +goose Down
DROP TABLE IF EXISTS tasks;
