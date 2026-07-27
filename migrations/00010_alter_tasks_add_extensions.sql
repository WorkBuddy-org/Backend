-- +goose Up
ALTER TABLE tasks
    ADD COLUMN extended_count INT NOT NULL DEFAULT 0,
    ADD COLUMN extra_minutes_used INT NOT NULL DEFAULT 0;

ALTER TABLE tasks
    ADD CONSTRAINT tasks_extended_count_check CHECK (extended_count >= 0),
    ADD CONSTRAINT tasks_extra_minutes_used_check CHECK (extra_minutes_used >= 0);

-- +goose Down
ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_extended_count_check,
    DROP CONSTRAINT IF EXISTS tasks_extra_minutes_used_check;

ALTER TABLE tasks
    DROP COLUMN extended_count,
    DROP COLUMN extra_minutes_used;
