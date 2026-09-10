CREATE TABLE IF NOT EXISTS room_rooms (
    code text PRIMARY KEY,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'closed')),
    current_round bigint NOT NULL DEFAULT 1 CHECK (current_round > 0),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    closed_at timestamptz
);

CREATE TABLE IF NOT EXISTS room_seats (
    room_code text NOT NULL REFERENCES room_rooms(code) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('host', 'guest')),
    player_id text NOT NULL,
    credential_digest bytea NOT NULL CHECK (octet_length(credential_digest) = 32),
    wins integer NOT NULL DEFAULT 0 CHECK (wins >= 0),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_code, role),
    UNIQUE (room_code, player_id),
    UNIQUE (room_code, credential_digest)
);

CREATE TABLE IF NOT EXISTS room_rounds (
    room_code text NOT NULL REFERENCES room_rooms(code) ON DELETE CASCADE,
    number bigint NOT NULL CHECK (number > 0),
    result text CHECK (result IN ('draw', 'player_one_wins', 'player_two_wins')),
    resolved_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_code, number),
    CHECK ((result IS NULL) = (resolved_at IS NULL))
);

CREATE TABLE IF NOT EXISTS room_moves (
    room_code text NOT NULL,
    round_number bigint NOT NULL,
    role text NOT NULL CHECK (role IN ('host', 'guest')),
    move text NOT NULL CHECK (move IN ('rock', 'paper', 'scissors')),
    submitted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_code, round_number, role),
    FOREIGN KEY (room_code, round_number) REFERENCES room_rounds(room_code, number) ON DELETE CASCADE,
    FOREIGN KEY (room_code, role) REFERENCES room_seats(room_code, role) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS room_next_round_requests (
    room_code text NOT NULL,
    round_number bigint NOT NULL,
    role text NOT NULL CHECK (role IN ('host', 'guest')),
    requested_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_code, round_number, role),
    FOREIGN KEY (room_code, round_number) REFERENCES room_rounds(room_code, number) ON DELETE CASCADE,
    FOREIGN KEY (room_code, role) REFERENCES room_seats(room_code, role) ON DELETE CASCADE
);
