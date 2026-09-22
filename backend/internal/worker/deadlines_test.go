package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/room/application"
)

func TestDeadlineWorkerReapsProcessesAndCleansInOrder(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC)
	store := &deadlineStoreStub{
		results: []application.DeadlineResult{
			{Processed: true, Settled: true, Kind: application.DeadlineDisconnect, DueAt: now.Add(-3 * time.Second)},
			{Processed: true, Stale: true, Kind: application.DeadlineInactivity, DueAt: now.Add(-7 * time.Second)},
			{},
		},
	}
	worker := NewDeadlineWorker(store, func() time.Time { return now })
	report, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ExpiredConnections != 2 || report.ProcessedDeadlines != 2 || report.SettledRounds != 1 || report.NoopDeadlines != 1 || report.StaleClaims != 1 || report.MaxClaimLag != 7*time.Second || report.CleanedRows != 3 {
		t.Fatalf("report = %#v", report)
	}
	wantCalls := []string{"reap", "process", "process", "process", "cleanup"}
	if len(store.calls) != len(wantCalls) {
		t.Fatalf("calls = %#v", store.calls)
	}
	for index := range wantCalls {
		if store.calls[index] != wantCalls[index] {
			t.Fatalf("calls = %#v", store.calls)
		}
	}
}

func TestDeadlineWorkerStopsAfterStoreFailure(t *testing.T) {
	store := &deadlineStoreStub{processErr: errors.New("database unavailable")}
	worker := NewDeadlineWorker(store, time.Now)
	if _, err := worker.RunOnce(context.Background()); !errors.Is(err, store.processErr) {
		t.Fatalf("error = %v", err)
	}
	if len(store.calls) != 2 || store.calls[0] != "reap" || store.calls[1] != "process" {
		t.Fatalf("calls = %#v", store.calls)
	}
}

type deadlineStoreStub struct {
	results    []application.DeadlineResult
	processErr error
	calls      []string
}

func (s *deadlineStoreStub) ReapExpiredConnections(context.Context, time.Time) (int64, error) {
	s.calls = append(s.calls, "reap")
	return 2, nil
}

func (s *deadlineStoreStub) ProcessNextDeadline(context.Context, time.Time) (application.DeadlineResult, error) {
	s.calls = append(s.calls, "process")
	if s.processErr != nil {
		return application.DeadlineResult{}, s.processErr
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func (s *deadlineStoreStub) CleanupDeadlines(context.Context, time.Time) (int64, error) {
	s.calls = append(s.calls, "cleanup")
	return 3, nil
}
