-- +goose Up
ALTER TABLE users ADD COLUMN password_hash TEXT;

ALTER TABLE users DROP CONSTRAINT users_oauth_provider_check;
ALTER TABLE users ADD CONSTRAINT users_oauth_provider_check CHECK (oauth_provider IN ('google', 'github', 'email'));

-- +goose Down
ALTER TABLE users DROP CONSTRAINT users_oauth_provider_check;
ALTER TABLE users ADD CONSTRAINT users_oauth_provider_check CHECK (oauth_provider IN ('google', 'github'));

ALTER TABLE users DROP COLUMN password_hash;
