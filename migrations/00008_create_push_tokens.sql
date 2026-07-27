-- +goose Up
CREATE TABLE push_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token       TEXT NOT NULL,
    platform    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT push_tokens_platform_check CHECK (platform IN ('ios', 'android', 'web'))
);

CREATE INDEX idx_push_tokens_user_id ON push_tokens (user_id);

-- +goose Down
DROP TABLE IF EXISTS push_tokens;
