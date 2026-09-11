ALTER TABLE room_presence
    ADD COLUMN refreshed_at timestamptz NOT NULL DEFAULT now();
