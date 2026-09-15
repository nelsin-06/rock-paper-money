CREATE TABLE wallet_accounts (
    account_id text PRIMARY KEY,
    account_type text NOT NULL CHECK (account_type IN ('user', 'house', 'escrow', 'mint')),
    auth_user_id uuid UNIQUE,
    room_code text,
    round_number bigint,
    balance bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((account_type = 'user') = (auth_user_id IS NOT NULL)),
    CHECK ((account_type = 'escrow') = (room_code IS NOT NULL AND round_number IS NOT NULL)),
    CHECK (account_type = 'mint' OR balance >= 0),
    UNIQUE (room_code, round_number)
);

INSERT INTO wallet_accounts(account_id, account_type)
VALUES ('house', 'house')
ON CONFLICT (account_id) DO NOTHING;

INSERT INTO wallet_accounts(account_id, account_type)
VALUES ('mint', 'mint')
ON CONFLICT (account_id) DO NOTHING;

CREATE TABLE wallet_transactions (
    transaction_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    business_key text NOT NULL UNIQUE,
    transaction_type text NOT NULL CHECK (transaction_type IN ('recharge', 'stake', 'settlement')),
    auth_user_id uuid,
    amount bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((transaction_type = 'recharge') = (auth_user_id IS NOT NULL AND amount IS NOT NULL))
);

CREATE TABLE wallet_postings (
    transaction_id bigint NOT NULL REFERENCES wallet_transactions(transaction_id),
    account_id text NOT NULL REFERENCES wallet_accounts(account_id),
    amount bigint NOT NULL CHECK (amount <> 0),
    PRIMARY KEY (transaction_id, account_id)
);

CREATE OR REPLACE FUNCTION reject_wallet_history_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallet ledger history is immutable';
END;
$$;

CREATE TRIGGER wallet_transactions_immutable
BEFORE UPDATE OR DELETE ON wallet_transactions
FOR EACH ROW EXECUTE FUNCTION reject_wallet_history_mutation();

CREATE TRIGGER wallet_postings_immutable
BEFORE UPDATE OR DELETE ON wallet_postings
FOR EACH ROW EXECUTE FUNCTION reject_wallet_history_mutation();

CREATE OR REPLACE FUNCTION enforce_balanced_wallet_transaction() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT COALESCE(sum(amount), 0) FROM wallet_postings WHERE transaction_id = NEW.transaction_id) <> 0 THEN
        RAISE EXCEPTION 'wallet transaction % is not balanced', NEW.transaction_id;
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wallet_transaction_balanced
AFTER INSERT ON wallet_postings
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION enforce_balanced_wallet_transaction();

ALTER TABLE room_rounds ADD COLUMN funded boolean NOT NULL DEFAULT false;

CREATE TABLE game_round_history (
    room_code text NOT NULL,
    round_number bigint NOT NULL CHECK (round_number > 0),
    host_account_id text NOT NULL,
    guest_account_id text NOT NULL,
    result text NOT NULL CHECK (result IN ('draw', 'player_one_wins', 'player_two_wins')),
    winner_role text CHECK (winner_role IN ('host', 'guest')),
    forfeited boolean NOT NULL,
    house_earnings bigint NOT NULL CHECK (house_earnings >= 0),
    resolved_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_code, round_number),
    CHECK ((result = 'draw') = (winner_role IS NULL))
);

CREATE INDEX game_round_history_resolved_at_idx
    ON game_round_history(resolved_at DESC, room_code, round_number DESC);
