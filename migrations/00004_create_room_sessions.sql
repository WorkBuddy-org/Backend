-- +goose Up
CREATE TABLE room_sessions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    room_id     UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at    TIMESTAMPTZ,
    status      TEXT NOT NULL,

    CONSTRAINT room_sessions_status_check CHECK (status IN ('focus', 'break'))
);

CREATE INDEX idx_room_sessions_room_id ON room_sessions (room_id);
CREATE INDEX idx_room_sessions_room_id_started_at ON room_sessions (room_id, started_at DESC);

-- +goose Down
DROP TABLE IF EXISTS room_sessions;
