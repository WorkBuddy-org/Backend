-- +goose Up
CREATE TABLE rooms (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    tags        TEXT[] NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT rooms_name_unique UNIQUE (name),
    CONSTRAINT rooms_type_check CHECK (type IN ('focus', 'collab', 'break'))
);

CREATE INDEX idx_rooms_type ON rooms (type);

INSERT INTO rooms (name, type, tags) VALUES
    ('Deep Focus',      'focus',  ARRAY['quiet', 'solo']),
    ('Study Hall',      'focus',  ARRAY['study', 'quiet']),
    ('Pair Programming','collab', ARRAY['pairing', 'code']),
    ('Standup Room',    'collab', ARRAY['meeting']),
    ('Chill Break',     'break',  ARRAY['music', 'relax']);

-- +goose Down
DROP TABLE IF EXISTS rooms;
