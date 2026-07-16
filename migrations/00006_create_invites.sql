-- +goose Up
CREATE TABLE invites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code        TEXT NOT NULL UNIQUE,
    created_by  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    used_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_invites_created_by ON invites (created_by);

-- +goose Down
DROP TABLE IF EXISTS invites;
