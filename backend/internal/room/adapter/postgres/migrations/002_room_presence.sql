ALTER TABLE room_rounds
    ADD COLUMN forfeited_role text CHECK (forfeited_role IN ('host', 'guest'));

CREATE TABLE room_presence (
    room_code text NOT NULL,
    player_id text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    deadline timestamptz NOT NULL,
    PRIMARY KEY (room_code, player_id),
    FOREIGN KEY (room_code, player_id) REFERENCES room_seats(room_code, player_id) ON DELETE CASCADE
);
