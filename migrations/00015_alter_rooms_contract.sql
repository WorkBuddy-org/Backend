-- +goose Up
-- The agreed room types are focus and discussion. Rooms of the old collab and
-- break types are conversation rooms, so they become discussion rooms.
ALTER TABLE rooms DROP CONSTRAINT rooms_type_check;
UPDATE rooms SET type = 'discussion' WHERE type IN ('collab', 'break');
ALTER TABLE rooms ALTER COLUMN type SET DEFAULT 'focus';
ALTER TABLE rooms ADD CONSTRAINT rooms_type_check CHECK (type IN ('focus', 'discussion'));

ALTER TABLE rooms
    ADD COLUMN capacity  INT NOT NULL DEFAULT 6,
    ADD COLUMN starts_at TIMESTAMPTZ,
    ADD COLUMN ends_at   TIMESTAMPTZ,
    ADD CONSTRAINT rooms_capacity_check CHECK (capacity > 0);

ALTER TABLE users ADD COLUMN avatar_url TEXT;

-- +goose Down
ALTER TABLE users DROP COLUMN avatar_url;

ALTER TABLE rooms
    DROP CONSTRAINT rooms_capacity_check,
    DROP COLUMN ends_at,
    DROP COLUMN starts_at,
    DROP COLUMN capacity;

ALTER TABLE rooms DROP CONSTRAINT rooms_type_check;
ALTER TABLE rooms ALTER COLUMN type DROP DEFAULT;
UPDATE rooms SET type = 'break' WHERE type = 'discussion' AND name = 'Chill Break';
UPDATE rooms SET type = 'collab' WHERE type = 'discussion';
ALTER TABLE rooms ADD CONSTRAINT rooms_type_check CHECK (type IN ('focus', 'collab', 'break'));
