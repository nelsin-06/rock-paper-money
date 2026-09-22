package operations

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"example.com/rock-paper-money/internal/worker"
)

type Runtime struct {
	deadlines *worker.DeadlineWorker
	outbox    *worker.OutboxWorker
	cleanup   *worker.CleanupWorker
	metrics   *Metrics
	logger    *slog.Logger
	intervals Intervals
	wait      sync.WaitGroup
}

type Intervals struct {
	Deadlines time.Duration
	Outbox    time.Duration
	Cleanup   time.Duration
}

func NewRuntime(deadlines *worker.DeadlineWorker, outbox *worker.OutboxWorker, cleanup *worker.CleanupWorker, metrics *Metrics, logger *slog.Logger, intervals Intervals) *Runtime {
	if intervals.Deadlines <= 0 {
		intervals.Deadlines = time.Second
	}
	if intervals.Outbox <= 0 {
		intervals.Outbox = 250 * time.Millisecond
	}
	if intervals.Cleanup <= 0 {
		intervals.Cleanup = time.Hour
	}
	if metrics == nil {
		metrics = NewMetrics()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{deadlines: deadlines, outbox: outbox, cleanup: cleanup, metrics: metrics, logger: logger, intervals: intervals}
}

func (r *Runtime) Start(ctx context.Context) {
	r.startLoop(ctx, "deadlines", r.intervals.Deadlines, r.runDeadlines)
	r.startLoop(ctx, "outbox", r.intervals.Outbox, r.runOutbox)
	r.startLoop(ctx, "cleanup", r.intervals.Cleanup, r.runCleanup)
}

func (r *Runtime) Wait() { r.wait.Wait() }

func (r *Runtime) startLoop(ctx context.Context, name string, interval time.Duration, run func(context.Context)) {
	r.wait.Add(1)
	go func() {
		defer r.wait.Done()
		run(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				r.logger.Info("worker stopped", "worker", name)
				return
			case <-ticker.C:
				run(ctx)
			}
		}
	}()
}

func (r *Runtime) runDeadlines(ctx context.Context) {
	report, err := r.deadlines.RunOnce(ctx)
	r.metrics.Deadline(report.ProcessedDeadlines, report.SettledRounds, report.NoopDeadlines, report.StaleClaims, report.MaxClaimLag)
	if err != nil {
		r.logger.Error("deadline worker failed", "error", err)
		return
	}
	if report.ExpiredConnections+report.ProcessedDeadlines+report.CleanedRows > 0 {
		r.logger.Info("deadline worker completed", "expired_connections", report.ExpiredConnections, "processed", report.ProcessedDeadlines, "settled", report.SettledRounds, "noops", report.NoopDeadlines, "stale_claims", report.StaleClaims, "max_claim_lag_seconds", report.MaxClaimLag.Seconds(), "cleaned", report.CleanedRows)
	}
}

func (r *Runtime) runOutbox(ctx context.Context) {
	report, err := r.outbox.RunOnce(ctx)
	r.metrics.Outbox(report)
	if err != nil {
		r.logger.Warn("outbox worker retrying", "error", err, "claimed", report.Claimed, "published", report.Published, "attempts", report.Attempts, "retries", report.Retries, "publish_failures", report.PublishFailures, "backlog_depth", report.BacklogDepth, "oldest_pending_age_seconds", report.OldestPendingAge.Seconds())
		return
	}
	if report.Claimed > 0 || report.Cleaned > 0 {
		r.logger.Info("outbox worker completed", "claimed", report.Claimed, "published", report.Published, "attempts", report.Attempts, "retries", report.Retries, "publish_failures", report.PublishFailures, "backlog_depth", report.BacklogDepth, "oldest_pending_age_seconds", report.OldestPendingAge.Seconds(), "cleaned", report.Cleaned)
	}
}

func (r *Runtime) runCleanup(ctx context.Context) {
	report, err := r.cleanup.RunOnce(ctx)
	r.metrics.Cleanup("sessions", report.Sessions)
	r.metrics.Cleanup("receipts", report.Receipts)
	if err != nil {
		r.logger.Error("cleanup worker failed", "error", err)
		return
	}
	r.logger.Info("cleanup worker completed", "sessions", report.Sessions, "receipts", report.Receipts)
}
