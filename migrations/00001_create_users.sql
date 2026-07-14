-- +goose Up
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    oauth_provider  TEXT NOT NULL,
    oauth_id        TEXT NOT NULL,
    role            TEXT,
    stack           TEXT,
    timezone        TEXT,
    avatar_status   TEXT NOT NULL DEFAULT 'none',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_oauth_unique UNIQUE (oauth_provider, oauth_id),
    CONSTRAINT users_email_unique UNIQUE (email),
    CONSTRAINT users_oauth_provider_check CHECK (oauth_provider IN ('google', 'github')),
    CONSTRAINT users_avatar_status_check CHECK (avatar_status IN ('none', 'pending', 'ready'))
);

CREATE INDEX idx_users_email ON users (email);

-- +goose Down
DROP TABLE IF EXISTS users;
