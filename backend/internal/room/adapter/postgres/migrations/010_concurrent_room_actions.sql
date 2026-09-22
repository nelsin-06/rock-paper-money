ALTER TABLE room_rounds
    ADD COLUMN status text NOT NULL DEFAULT 'active';

UPDATE room_rounds
SET status = 'resolved'
WHERE result IS NOT NULL;

ALTER TABLE room_rounds
    ADD CONSTRAINT room_rounds_status_check
    CHECK (status IN ('active', 'settling', 'resolved'));

CREATE INDEX room_rounds_transition_idx
    ON room_rounds(status, created_at, room_code, number);
