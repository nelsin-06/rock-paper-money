CREATE TABLE problem_reports (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id uuid NOT NULL,
    description text NOT NULL CHECK (char_length(btrim(description)) BETWEEN 1 AND 5000),
    section text NOT NULL CHECK (section IN (
        'main_menu', 'create_room', 'join_room', 'wallet', 'waiting_room',
        'gameplay', 'round_result', 'next_round', 'connection', 'account'
    )),
    app_version text NOT NULL CHECK (char_length(btrim(app_version)) BETWEEN 1 AND 64),
    created_at timestamptz NOT NULL DEFAULT now()
);
