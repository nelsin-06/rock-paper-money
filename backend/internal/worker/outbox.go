package worker

import (
	"context"
	"errors"
	"time"
)

type OutboxRecord struct {
	ID         int64
	RoomID     string
	Revision   uint64
	ClaimToken string
	Attempts   int
}

type OutboxStore interface {
	ClaimOutbox(context.Context, time.Time, int, time.Duration) ([]OutboxRecord, error)
	OutboxPendingStats(context.Context, time.Time) (OutboxPendingStats, error)
	MarkOutboxPublished(context.Context, OutboxRecord, time.Time) error
	ReleaseOutbox(context.Context, OutboxRecord, time.Time) error
	CleanupPublishedOutbox(context.Context, time.Time) (int64, error)
}

type OutboxPendingStats struct {
	BacklogDepth     int64
	OldestPendingAge time.Duration
}

type RevisionPublisher interface {
	PublishRevision(context.Context, string, uint64) error
}

type OutboxConfig struct {
	Now             func() time.Time
	BatchSize       int
	ClaimLease      time.Duration
	RetryBase       time.Duration
	MaxRetry        time.Duration
	PublishedRetain time.Duration
}

type OutboxRunReport struct {
	Claimed          int
	Published        int
	Attempts         int
	Retries          int
	PublishFailures  int
	BacklogDepth     int64
	OldestPendingAge time.Duration
	Cleaned          int64
}

type OutboxWorker struct {
	store     OutboxStore
	publisher RevisionPublisher
	config    OutboxConfig
}

func NewOutboxWorker(store OutboxStore, publisher RevisionPublisher, config OutboxConfig) *OutboxWorker {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 100
	}
	if config.ClaimLease <= 0 {
		config.ClaimLease = 30 * time.Second
	}
	if config.RetryBase <= 0 {
		config.RetryBase = time.Second
	}
	if config.MaxRetry <= 0 {
		config.MaxRetry = time.Minute
	}
	if config.PublishedRetain <= 0 {
		config.PublishedRetain = 24 * time.Hour
	}
	return &OutboxWorker{store: store, publisher: publisher, config: config}
}

func (w *OutboxWorker) RunOnce(ctx context.Context) (OutboxRunReport, error) {
	now := w.config.Now()
	stats, err := w.store.OutboxPendingStats(ctx, now)
	report := OutboxRunReport{BacklogDepth: stats.BacklogDepth, OldestPendingAge: stats.OldestPendingAge}
	if err != nil {
		return report, err
	}
	records, err := w.store.ClaimOutbox(ctx, now, w.config.BatchSize, w.config.ClaimLease)
	report.Claimed = len(records)
	if err != nil {
		return report, err
	}
	for _, record := range records {
		report.Attempts++
		if record.Attempts > 1 {
			report.Retries++
		}
		if err = w.publisher.PublishRevision(ctx, record.RoomID, record.Revision); err != nil {
			report.PublishFailures++
			releaseErr := w.store.ReleaseOutbox(ctx, record, now.Add(w.retryDelay(record.Attempts)))
			return report, errors.Join(err, releaseErr)
		}
		if err = w.store.MarkOutboxPublished(ctx, record, now); err != nil {
			// Publication may have succeeded. Keep the lease until it expires so a
			// later claim can safely publish the same idempotent revision hint again.
			return report, err
		}
		report.Published++
	}
	report.Cleaned, err = w.store.CleanupPublishedOutbox(ctx, now.Add(-w.config.PublishedRetain))
	return report, err
}

func (w *OutboxWorker) retryDelay(attempts int) time.Duration {
	delay := w.config.RetryBase
	for attempt := 1; attempt < attempts && delay < w.config.MaxRetry; attempt++ {
		if delay > w.config.MaxRetry/2 {
			return w.config.MaxRetry
		}
		delay *= 2
	}
	if delay > w.config.MaxRetry {
		return w.config.MaxRetry
	}
	return delay
}
