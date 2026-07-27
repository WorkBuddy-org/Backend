-- +goose Up
CREATE TABLE room_participations (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    room_id      UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    session_id   UUID REFERENCES room_sessions(id) ON DELETE SET NULL,
    joined_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    left_at      TIMESTAMPTZ,
    left_reason  TEXT,

    CONSTRAINT room_participations_left_reason_check
        CHECK (left_reason IS NULL OR left_reason IN ('completed_no_extension', 'kicked', 'left_voluntarily')),
    CONSTRAINT room_participations_left_consistency_check
        CHECK ((left_at IS NULL AND left_reason IS NULL) OR (left_at IS NOT NULL AND left_reason IS NOT NULL))
);

CREATE INDEX idx_room_participations_room_id ON room_participations (room_id);
CREATE INDEX idx_room_participations_user_id ON room_participations (user_id);
CREATE INDEX idx_room_participations_session_id ON room_participations (session_id);

-- +goose Down
DROP TABLE IF EXISTS room_participations;
