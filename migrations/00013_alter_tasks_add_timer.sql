-- +goose Up
ALTER TABLE tasks
    ADD COLUMN timer_status TEXT NOT NULL DEFAULT 'idle',
    ADD COLUMN timer_duration_minutes INT,
    ADD COLUMN timer_started_at TIMESTAMPTZ,
    ADD COLUMN timer_ends_at TIMESTAMPTZ,
    ADD COLUMN prompt_deadline_at TIMESTAMPTZ;

ALTER TABLE tasks
    ADD CONSTRAINT tasks_timer_status_check
        CHECK (timer_status IN ('idle', 'running', 'prompting', 'ended')),
    ADD CONSTRAINT tasks_timer_duration_minutes_check
        CHECK (timer_duration_minutes IS NULL OR timer_duration_minutes > 0);

-- At most one running/prompting timer per user. Also serves the boot-time
-- recovery scan of active timers.
CREATE UNIQUE INDEX idx_tasks_one_active_timer_per_user
    ON tasks (user_id) WHERE timer_status IN ('running', 'prompting');

-- +goose Down
DROP INDEX IF EXISTS idx_tasks_one_active_timer_per_user;

ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_timer_status_check,
    DROP CONSTRAINT IF EXISTS tasks_timer_duration_minutes_check;

ALTER TABLE tasks
    DROP COLUMN timer_status,
    DROP COLUMN timer_duration_minutes,
    DROP COLUMN timer_started_at,
    DROP COLUMN timer_ends_at,
    DROP COLUMN prompt_deadline_at;
