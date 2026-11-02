-- +goose Up
ALTER TABLE users
    ADD COLUMN first_name           TEXT NOT NULL DEFAULT '',
    ADD COLUMN last_name            TEXT NOT NULL DEFAULT '',
    ADD COLUMN goal                 TEXT,
    ADD COLUMN experience           TEXT,
    ADD COLUMN profession           TEXT,
    ADD COLUMN profession_other     TEXT,
    ADD COLUMN onboarding_completed BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN plan                 TEXT NOT NULL DEFAULT 'free',
    ADD CONSTRAINT users_goal_check
        CHECK (goal IS NULL OR goal IN ('improve_focus', 'share_experience', 'improve_skills')),
    ADD CONSTRAINT users_experience_check
        CHECK (experience IS NULL OR experience IN ('lt_1', '1_3', '3_5', '5_plus')),
    ADD CONSTRAINT users_profession_check
        CHECK (profession IS NULL OR profession IN ('uxui_designer', 'qa', 'project_manager', 'backend_dev', 'frontend_dev', 'other')),
    ADD CONSTRAINT users_plan_check
        CHECK (plan IN ('free', 'pro'));

UPDATE users SET first_name = name;

-- +goose Down
ALTER TABLE users
    DROP CONSTRAINT users_plan_check,
    DROP CONSTRAINT users_profession_check,
    DROP CONSTRAINT users_experience_check,
    DROP CONSTRAINT users_goal_check,
    DROP COLUMN plan,
    DROP COLUMN onboarding_completed,
    DROP COLUMN profession_other,
    DROP COLUMN profession,
    DROP COLUMN experience,
    DROP COLUMN goal,
    DROP COLUMN last_name,
    DROP COLUMN first_name;
