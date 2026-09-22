package worker

import (
	"context"
	"time"
)

type SessionCleaner interface {
	Cleanup(context.Context) (int64, error)
}

type ReceiptCleaner interface {
	CleanupCommandReceipts(context.Context, time.Time) (int64, error)
}

type CleanupReport struct {
	Sessions int64
	Receipts int64
}

type CleanupWorker struct {
	sessions SessionCleaner
	receipts ReceiptCleaner
	now      func() time.Time
}

func NewCleanupWorker(sessions SessionCleaner, receipts ReceiptCleaner, now func() time.Time) *CleanupWorker {
	if now == nil {
		now = time.Now
	}
	return &CleanupWorker{sessions: sessions, receipts: receipts, now: now}
}

func (w *CleanupWorker) RunOnce(ctx context.Context) (CleanupReport, error) {
	report := CleanupReport{}
	var err error
	if report.Sessions, err = w.sessions.Cleanup(ctx); err != nil {
		return report, err
	}
	report.Receipts, err = w.receipts.CleanupCommandReceipts(ctx, w.now().UTC())
	return report, err
}
