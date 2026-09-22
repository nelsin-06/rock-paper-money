DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM room_seats WHERE auth_user_id IS NULL) THEN
        RAISE EXCEPTION 'account seat migration requires disposable rooms to be reset first';
    END IF;
END;
$$;

ALTER TABLE room_seats ALTER COLUMN auth_user_id SET NOT NULL;
DROP INDEX IF EXISTS room_seats_one_account_per_room;
ALTER TABLE room_seats ADD CONSTRAINT room_seats_one_account_per_room UNIQUE (room_code, auth_user_id);
ALTER TABLE room_seats DROP COLUMN credential_digest;

CREATE INDEX command_receipts_key_idx ON command_receipts(idempotency_key);
