package worker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/worker"
)

func TestReconciliationWorkerDrainsStartupNotificationsReconnectAndPeriodic(t *testing.T) {
	store := &reconciliationStoreStub{sources: make(chan string, 16)}
	listener := &listenerStub{}
	reconciler := worker.NewReconciliationWorker(store, listener, worker.ReconciliationConfig{
		BatchSize: 2, MaxDrainBatches: 2, PeriodicInterval: time.Millisecond, IdleInterval: 2 * time.Millisecond, ReconnectDelay: time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	observed := make(chan worker.ReconciliationSource, 16)
	go func() {
		reconciler.Run(ctx, func(source worker.ReconciliationSource, _ worker.ReconciliationReport, _ error) { observed <- source })
		close(done)
	}()
	deadline := time.After(time.Second)
	want := map[string]bool{"batch": false, "hint": false}
	reconnected := false
	for !want["batch"] || !want["hint"] || listener.connections() < 2 || !reconnected {
		select {
		case source := <-store.sources:
			want[source] = true
		case source := <-observed:
			reconnected = reconnected || source == worker.ReconciliationReconnect
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatalf("reconciliation calls = %#v", want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconciliation worker did not stop")
	}
	if listener.connections() < 2 {
		t.Fatalf("listener connections = %d, want reconnect", listener.connections())
	}
}

type reconciliationStoreStub struct {
	sources chan string
}

func (s *reconciliationStoreStub) ReconcileRoom(context.Context, worker.RoomHint) (worker.ReconciliationReport, error) {
	s.sources <- "hint"
	return worker.ReconciliationReport{ClaimAttempts: 1, ClaimWins: 1}, nil
}

func (s *reconciliationStoreStub) ReconcileBatch(context.Context, int) (worker.ReconciliationReport, error) {
	s.sources <- "batch"
	return worker.ReconciliationReport{}, nil
}

type listenerStub struct {
	mu    sync.Mutex
	calls int
}

func (l *listenerStub) Listen(ctx context.Context, established func(), notify func(worker.RoomHint)) error {
	l.mu.Lock()
	l.calls++
	call := l.calls
	l.mu.Unlock()
	established()
	if call == 1 {
		notify(worker.RoomHint{RoomCode: "ROOM", Round: 1})
		return errors.New("connection dropped")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (l *listenerStub) connections() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}
