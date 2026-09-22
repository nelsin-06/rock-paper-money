package worker

import (
	"context"
	"time"

	"example.com/rock-paper-money/internal/room/application"
)

type DeadlineWorker struct {
	store application.DeadlineRepository
	now   func() time.Time
}

type DeadlineRunReport struct {
	ExpiredConnections int64
	ProcessedDeadlines int64
	SettledRounds      int64
	NoopDeadlines      int64
	StaleClaims        int64
	MaxClaimLag        time.Duration
	CleanedRows        int64
}

func NewDeadlineWorker(store application.DeadlineRepository, now func() time.Time) *DeadlineWorker {
	return &DeadlineWorker{store: store, now: now}
}

func (w *DeadlineWorker) RunOnce(ctx context.Context) (DeadlineRunReport, error) {
	now := w.now()
	report := DeadlineRunReport{}
	var err error
	if report.ExpiredConnections, err = w.store.ReapExpiredConnections(ctx, now); err != nil {
		return report, err
	}
	for {
		result, processErr := w.store.ProcessNextDeadline(ctx, now)
		if processErr != nil {
			return report, processErr
		}
		if !result.Processed {
			break
		}
		report.ProcessedDeadlines++
		if !result.DueAt.IsZero() {
			lag := now.Sub(result.DueAt)
			if lag > report.MaxClaimLag {
				report.MaxClaimLag = lag
			}
		}
		if result.Settled {
			report.SettledRounds++
		} else {
			report.NoopDeadlines++
		}
		if result.Stale {
			report.StaleClaims++
		}
	}
	if report.CleanedRows, err = w.store.CleanupDeadlines(ctx, now); err != nil {
		return report, err
	}
	return report, nil
}
