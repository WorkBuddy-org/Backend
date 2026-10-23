-- +goose Up
-- 'kicked_no_extension' records removal after declining or ignoring the
-- task extension prompt; 'kicked' stays for other server-side removals.
ALTER TABLE room_participations DROP CONSTRAINT room_participations_left_reason_check;
ALTER TABLE room_participations ADD CONSTRAINT room_participations_left_reason_check
    CHECK (left_reason IS NULL OR left_reason IN ('completed_no_extension', 'kicked', 'kicked_no_extension', 'left_voluntarily'));

-- +goose Down
UPDATE room_participations SET left_reason = 'kicked' WHERE left_reason = 'kicked_no_extension';
ALTER TABLE room_participations DROP CONSTRAINT room_participations_left_reason_check;
ALTER TABLE room_participations ADD CONSTRAINT room_participations_left_reason_check
    CHECK (left_reason IS NULL OR left_reason IN ('completed_no_extension', 'kicked', 'left_voluntarily'));
