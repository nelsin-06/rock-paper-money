package operations

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/worker"
)

func TestRuntimeRunsEveryWorkerAndStopsGracefully(t *testing.T) {
	store := &runtimeStore{calls: make(map[string]int)}
	metrics := NewMetrics()
	runtime := NewRuntime(
		worker.NewDeadlineWorker(store, time.Now),
		worker.NewOutboxWorker(store, runtimePublisher{}, worker.OutboxConfig{}),
		worker.NewCleanupWorker(store, store, time.Now),
		metrics,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Intervals{Deadlines: time.Millisecond, Outbox: time.Millisecond, Cleanup: time.Millisecond},
	)
	runtime.SetReconciliation(worker.NewReconciliationWorker(store, nil, worker.ReconciliationConfig{PeriodicInterval: time.Millisecond, IdleInterval: time.Millisecond}))
	ctx, cancel := context.WithCancel(context.Background())
	runtime.Start(ctx)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !store.called("deadline", "outbox", "sessions", "receipts", "reconciliation") {
		time.Sleep(time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { runtime.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime did not stop after cancellation")
	}
	if !store.called("deadline", "outbox", "sessions", "receipts", "reconciliation") {
		t.Fatalf("worker calls = %#v", store.snapshot())
	}
}

func TestMetricsExposeOnlyBoundedOperationalLabels(t *testing.T) {
	metrics := NewMetrics()
	metrics.SocketOpened()
	metrics.QueueEviction()
	metrics.AuthClose()
	metrics.RedisFailure()
	metrics.Receipt("conflict")
	metrics.Deadline(2, 1, 1, 1, 7*time.Second)
	metrics.Outbox(worker.OutboxRunReport{Claimed: 3, Published: 2, Attempts: 3, Retries: 1, PublishFailures: 1, BacklogDepth: 5, OldestPendingAge: 11 * time.Second})
	metrics.Reconciliation(worker.ReconciliationPeriodic, worker.ReconciliationReport{ClaimAttempts: 2, ClaimWins: 1, ClaimNoops: 1, SettledRounds: 1, EligibleBacklog: 3})
	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, metric := range []string{
		"rpm_active_sockets 1",
		"rpm_backpressure_eviction_total 1",
		"rpm_auth_close_total 1",
		"rpm_redis_failure_total 1",
		"rpm_receipt_total{outcome=\"conflict\"} 1",
		"rpm_deadline_lag_seconds 7",
		"rpm_deadline_noop_total 1",
		"rpm_deadline_stale_claim_total 1",
		"rpm_outbox_oldest_pending_age_seconds 11",
		"rpm_outbox_backlog_depth 5",
		"rpm_outbox_attempt_total 3",
		"rpm_outbox_retry_total 1",
		"rpm_outbox_publish_failure_total 1",
		"rpm_reconciliation_claim_attempt_total{source=\"periodic\"} 2",
		"rpm_reconciliation_claim_win_total{source=\"periodic\"} 1",
		"rpm_reconciliation_backlog 3",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("metrics missing %q: %s", metric, body)
		}
	}
}

type runtimeStore struct {
	mu    sync.Mutex
	calls map[string]int
}

func (s *runtimeStore) mark(name string) { s.mu.Lock(); s.calls[name]++; s.mu.Unlock() }
func (s *runtimeStore) called(names ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		if s.calls[name] == 0 {
			return false
		}
	}
	return true
}
func (s *runtimeStore) snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]int, len(s.calls))
	for name, count := range s.calls {
		result[name] = count
	}
	return result
}
func (s *runtimeStore) ReapExpiredConnections(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (s *runtimeStore) ProcessNextDeadline(context.Context, time.Time) (application.DeadlineResult, error) {
	s.mark("deadline")
	return application.DeadlineResult{}, nil
}
func (s *runtimeStore) CleanupDeadlines(context.Context, time.Time) (int64, error) { return 0, nil }
func (s *runtimeStore) ClaimOutbox(context.Context, time.Time, int, time.Duration) ([]worker.OutboxRecord, error) {
	s.mark("outbox")
	return nil, nil
}
func (s *runtimeStore) OutboxPendingStats(context.Context, time.Time) (worker.OutboxPendingStats, error) {
	return worker.OutboxPendingStats{}, nil
}
func (s *runtimeStore) MarkOutboxPublished(context.Context, worker.OutboxRecord, time.Time) error {
	return nil
}
func (s *runtimeStore) ReleaseOutbox(context.Context, worker.OutboxRecord, time.Time) error {
	return nil
}
func (s *runtimeStore) CleanupPublishedOutbox(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (s *runtimeStore) Cleanup(context.Context) (int64, error) { s.mark("sessions"); return 1, nil }
func (s *runtimeStore) CleanupCommandReceipts(context.Context, time.Time) (int64, error) {
	s.mark("receipts")
	return 1, nil
}
func (s *runtimeStore) ReconcileRoom(context.Context, worker.RoomHint) (worker.ReconciliationReport, error) {
	s.mark("reconciliation")
	return worker.ReconciliationReport{}, nil
}
func (s *runtimeStore) ReconcileBatch(context.Context, int) (worker.ReconciliationReport, error) {
	s.mark("reconciliation")
	return worker.ReconciliationReport{}, nil
}

type runtimePublisher struct{}

func (runtimePublisher) PublishRevision(context.Context, string, uint64) error { return nil }
