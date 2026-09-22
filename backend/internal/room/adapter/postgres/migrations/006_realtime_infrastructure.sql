CREATE TABLE web_sessions (
    session_digest bytea PRIMARY KEY CHECK (octet_length(session_digest) = 32),
    account_id uuid NOT NULL,
    csrf_digest bytea NOT NULL CHECK (octet_length(csrf_digest) = 32),
    created_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    idle_expires_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK (created_at <= last_seen_at),
    CHECK (idle_expires_at <= absolute_expires_at),
    CHECK (created_at < absolute_expires_at)
);

CREATE INDEX web_sessions_account_idx ON web_sessions(account_id);
CREATE INDEX web_sessions_cleanup_idx ON web_sessions(LEAST(idle_expires_at, absolute_expires_at));

CREATE TABLE command_receipts (
    receipt_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id uuid NOT NULL,
    idempotency_key text NOT NULL CHECK (idempotency_key <> ''),
    operation text NOT NULL CHECK (operation <> ''),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    response_status integer CHECK (response_status BETWEEN 100 AND 599),
    response_headers jsonb,
    response_body bytea,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    UNIQUE (account_id, idempotency_key),
    CHECK (expires_at >= created_at)
);

CREATE INDEX command_receipts_cleanup_idx ON command_receipts(expires_at);

CREATE TABLE room_connections (
    connection_id uuid PRIMARY KEY,
    session_digest bytea NOT NULL REFERENCES web_sessions(session_digest) ON DELETE CASCADE,
    room_code text NOT NULL,
    role text NOT NULL,
    connected_at timestamptz NOT NULL DEFAULT now(),
    lease_expires_at timestamptz NOT NULL,
    closed_at timestamptz,
    FOREIGN KEY (room_code, role) REFERENCES room_seats(room_code, role) ON DELETE CASCADE,
    CHECK (lease_expires_at > connected_at),
    CHECK (closed_at IS NULL OR closed_at >= connected_at)
);

CREATE INDEX room_connections_active_lease_idx
    ON room_connections(lease_expires_at)
    WHERE closed_at IS NULL;
CREATE INDEX room_connections_room_role_idx
    ON room_connections(room_code, role)
    WHERE closed_at IS NULL;

CREATE TABLE room_deadlines (
    deadline_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room_code text NOT NULL,
    round_number bigint NOT NULL,
    kind text NOT NULL CHECK (kind IN ('disconnect', 'inactivity')),
    role text CHECK (role IN ('host', 'guest')),
    generation bigint NOT NULL CHECK (generation > 0),
    due_at timestamptz NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'claimed', 'completed', 'cancelled')),
    claimed_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (room_code, round_number) REFERENCES room_rounds(room_code, number) ON DELETE CASCADE,
    CHECK ((kind = 'disconnect') = (role IS NOT NULL))
);

CREATE UNIQUE INDEX room_deadlines_identity_idx
    ON room_deadlines(room_code, round_number, kind, COALESCE(role, ''), generation);
CREATE INDEX room_deadlines_pending_idx
    ON room_deadlines(due_at, deadline_id)
    WHERE status = 'pending';

CREATE TABLE room_outbox (
    outbox_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room_code text NOT NULL REFERENCES room_rooms(code) ON DELETE CASCADE,
    revision bigint NOT NULL CHECK (revision > 0),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (room_code, revision)
);

CREATE INDEX room_outbox_pending_idx
    ON room_outbox(next_attempt_at, outbox_id)
    WHERE published_at IS NULL;
