package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"example.com/rock-paper-money/internal/worker"
)

var ErrOutboxClaimLost = errors.New("outbox claim is no longer owned")

func (r *Repository) OutboxPendingStats(ctx context.Context, now time.Time) (worker.OutboxPendingStats, error) {
	var stats worker.OutboxPendingStats
	var oldestCreatedAt *time.Time
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*),min(created_at) FROM room_outbox WHERE published_at IS NULL`).Scan(&stats.BacklogDepth, &oldestCreatedAt); err != nil {
		return worker.OutboxPendingStats{}, err
	}
	if oldestCreatedAt != nil && now.After(*oldestCreatedAt) {
		stats.OldestPendingAge = now.Sub(*oldestCreatedAt)
	}
	return stats, nil
}

func (r *Repository) ClaimOutbox(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]worker.OutboxRecord, error) {
	if limit <= 0 || lease <= 0 {
		return nil, errors.New("outbox claim limit and lease must be positive")
	}
	claimToken, err := newClaimToken()
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		WITH candidates AS (
			SELECT outbox_id FROM room_outbox
			WHERE published_at IS NULL AND next_attempt_at<=$1
			  AND (claimed_until IS NULL OR claimed_until<=$1)
			ORDER BY next_attempt_at,outbox_id
			LIMIT $2 FOR UPDATE SKIP LOCKED
		)
		UPDATE room_outbox AS item
		SET claim_token=$3,claimed_until=$1+$4::interval,attempts=item.attempts+1
		FROM candidates WHERE item.outbox_id=candidates.outbox_id
		RETURNING item.outbox_id,item.room_code,item.revision,item.claim_token,item.attempts`,
		now, limit, claimToken, intervalString(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []worker.OutboxRecord
	for rows.Next() {
		var record worker.OutboxRecord
		if err = rows.Scan(&record.ID, &record.RoomID, &record.Revision, &record.ClaimToken, &record.Attempts); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r *Repository) MarkOutboxPublished(ctx context.Context, record worker.OutboxRecord, publishedAt time.Time) error {
	result, err := r.pool.Exec(ctx, `
		UPDATE room_outbox SET published_at=$3,claim_token=NULL,claimed_until=NULL
		WHERE outbox_id=$1 AND claim_token=$2 AND published_at IS NULL`, record.ID, record.ClaimToken, publishedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrOutboxClaimLost
	}
	return nil
}

func (r *Repository) ReleaseOutbox(ctx context.Context, record worker.OutboxRecord, retryAt time.Time) error {
	result, err := r.pool.Exec(ctx, `
		UPDATE room_outbox SET next_attempt_at=$3,claim_token=NULL,claimed_until=NULL
		WHERE outbox_id=$1 AND claim_token=$2 AND published_at IS NULL`, record.ID, record.ClaimToken, retryAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrOutboxClaimLost
	}
	return nil
}

func (r *Repository) CleanupPublishedOutbox(ctx context.Context, before time.Time) (int64, error) {
	result, err := r.pool.Exec(ctx, "DELETE FROM room_outbox WHERE published_at IS NOT NULL AND published_at<$1", before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

func newClaimToken() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

func intervalString(duration time.Duration) string {
	return fmt.Sprintf("%f seconds", duration.Seconds())
}
