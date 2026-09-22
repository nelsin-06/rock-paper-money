ALTER TABLE room_presence
    ADD COLUMN present boolean NOT NULL DEFAULT false,
    ADD COLUMN disconnected_at timestamptz;

CREATE INDEX room_presence_disconnected_idx
    ON room_presence(room_code, generation)
    WHERE present = false;

CREATE INDEX room_connections_room_role_valid_idx
    ON room_connections(room_code, role, lease_expires_at)
    WHERE closed_at IS NULL;

CREATE INDEX room_deadlines_room_pending_idx
    ON room_deadlines(room_code, round_number, due_at, deadline_id)
    WHERE status = 'pending';
