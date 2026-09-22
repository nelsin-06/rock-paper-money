ALTER TABLE room_outbox
    ADD COLUMN claim_token text,
    ADD COLUMN claimed_until timestamptz,
    ADD CONSTRAINT room_outbox_claim_pair CHECK ((claim_token IS NULL) = (claimed_until IS NULL));

DROP INDEX room_outbox_pending_idx;

CREATE INDEX room_outbox_pending_idx
    ON room_outbox(next_attempt_at, claimed_until, outbox_id)
    WHERE published_at IS NULL;
