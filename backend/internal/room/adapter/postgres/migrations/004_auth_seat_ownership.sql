-- Existing anonymous seats remain nullable and cannot pass account-bound authorization.
-- No FK targets auth.users so this schema remains usable in plain PostgreSQL tests.
ALTER TABLE room_seats ADD COLUMN IF NOT EXISTS auth_user_id uuid;

CREATE UNIQUE INDEX IF NOT EXISTS room_seats_one_account_per_room
    ON room_seats(room_code, auth_user_id)
    WHERE auth_user_id IS NOT NULL;
