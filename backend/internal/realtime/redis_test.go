package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/worker"
	redis "github.com/redis/go-redis/v9"
)

func TestRevisionHintUsesExactChannelAndMinimalPayload(t *testing.T) {
	hint := RevisionHint{RoomID: "ABC123", Revision: 42}
	payload, err := EncodeRevisionHint(hint)
	if err != nil {
		t.Fatal(err)
	}
	if RoomChannel(hint.RoomID) != "room:ABC123" || string(payload) != `{"room_id":"ABC123","revision":42}` {
		t.Fatalf("channel=%q payload=%s", RoomChannel(hint.RoomID), payload)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(payload, &fields); err != nil || len(fields) != 2 {
		t.Fatalf("fields=%#v err=%v", fields, err)
	}
	if _, err = DecodeRevisionHint("room:OTHER", payload); !errors.Is(err, ErrInvalidRevisionHint) {
		t.Fatalf("cross-room decode error = %v", err)
	}
}

func TestHubConfirmsReusesReleasesAndReconcilesAuthoritatively(t *testing.T) {
	subscriber := &subscriberStub{}
	loader := &snapshotLoaderStub{revisions: []uint64{20, 22, 25}}
	hub := NewHub(subscriber, loader)
	var mu sync.Mutex
	var first, second []uint64
	releaseFirst, err := hub.Acquire(context.Background(), "ABC123", func(snapshot application.Snapshot) {
		mu.Lock()
		first = append(first, snapshot.Revision)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if subscriber.subscribeCalls != 1 || !subscriber.confirmedBeforeReturn {
		t.Fatalf("subscribe calls=%d confirmed=%v", subscriber.subscribeCalls, subscriber.confirmedBeforeReturn)
	}
	releaseSecond, err := hub.Acquire(context.Background(), "ABC123", func(snapshot application.Snapshot) {
		mu.Lock()
		second = append(second, snapshot.Revision)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if subscriber.subscribeCalls != 1 {
		t.Fatalf("room subscription was not reused: %d", subscriber.subscribeCalls)
	}

	subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: "ABC123", Revision: 20}})
	subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: "ABC123", Revision: 19}})
	subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: "ABC123", Revision: 22}})
	subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: "ABC123", Revision: 25}})
	if loader.calls != 3 {
		t.Fatalf("authoritative reload calls = %d", loader.calls)
	}
	mu.Lock()
	if !equalRevisions(first, []uint64{20, 22, 25}) || !equalRevisions(second, []uint64{20, 22, 25}) {
		t.Fatalf("first=%v second=%v", first, second)
	}
	mu.Unlock()

	releaseFirst()
	if subscriber.closed != 0 {
		t.Fatalf("shared subscription closed early")
	}
	releaseSecond()
	if subscriber.closed != 1 {
		t.Fatalf("last local release did not close subscription")
	}
}

func TestHubReconcilesAfterRedisReconnectAndRejectsWrongRoomHints(t *testing.T) {
	subscriber := &subscriberStub{}
	loader := &snapshotLoaderStub{revisions: []uint64{7, 9}}
	hub := NewHub(subscriber, loader)
	release, err := hub.Acquire(context.Background(), "ROOM77", func(application.Snapshot) {})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	subscriber.emit(SubscriptionEvent{Reconnected: true})
	if loader.calls != 2 {
		t.Fatalf("reconnect did not reconcile: calls=%d", loader.calls)
	}
	subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: "OTHER", Revision: 100}})
	if loader.calls != 2 {
		t.Fatalf("wrong-room hint triggered authority load")
	}
}

func TestRedisTransportResubscribesAndReconcilesAfterRuntimeInterruption(t *testing.T) {
	if os.Getenv("WU4_REDIS_RECOVERY") != "1" {
		t.Skip("WU4_REDIS_RECOVERY is not enabled")
	}
	address := os.Getenv("REDIS_ADDR")
	readyFile := os.Getenv("WU4_REDIS_READY_FILE")
	if address == "" || readyFile == "" {
		t.Fatal("REDIS_ADDR and WU4_REDIS_READY_FILE are required")
	}
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	transport := NewRedisTransport(client, 100*time.Millisecond)
	transport.healthInterval = 100 * time.Millisecond
	loader := &runtimeSnapshotLoader{revision: 1}
	hub := NewHub(transport, loader)
	revisions := make(chan uint64, 8)
	release, err := hub.Acquire(context.Background(), "LIVE77", func(snapshot application.Snapshot) {
		revisions <- snapshot.Revision
	})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := receiveRevision(t, revisions, 5*time.Second); got != 1 {
		t.Fatalf("initial revision = %d", got)
	}
	loader.setRevision(3)
	if err = os.WriteFile(readyFile, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(readyFile) })
	time.Sleep(300 * time.Millisecond)
	outbox := &runtimeOutboxStore{records: []worker.OutboxRecord{{ID: 1, RoomID: "LIVE77", Revision: 2, ClaimToken: "first", Attempts: 1}}}
	dispatcher := worker.NewOutboxWorker(outbox, transport, worker.OutboxConfig{RetryBase: 100 * time.Millisecond})
	if report, dispatchErr := dispatcher.RunOnce(context.Background()); dispatchErr == nil || report.Published != 0 || outbox.releases != 1 {
		t.Fatalf("Redis outage report=%#v err=%v releases=%d", report, dispatchErr, outbox.releases)
	}
	if got := receiveRevision(t, revisions, 20*time.Second); got != 3 {
		t.Fatalf("reconciled revision = %d", got)
	}
	outbox.records = []worker.OutboxRecord{{ID: 1, RoomID: "LIVE77", Revision: 2, ClaimToken: "second", Attempts: 2}}
	restarted := worker.NewOutboxWorker(outbox, transport, worker.OutboxConfig{})
	if report, dispatchErr := restarted.RunOnce(context.Background()); dispatchErr != nil || report.Published != 1 || outbox.marked != 1 {
		t.Fatalf("restored Redis report=%#v err=%v marked=%d", report, dispatchErr, outbox.marked)
	}
	loads := loader.loadCount()
	if err = transport.PublishRevision(context.Background(), "LIVE77", 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if loader.loadCount() != loads {
		t.Fatalf("duplicate revision reloaded PostgreSQL: before=%d after=%d", loads, loader.loadCount())
	}
}

type subscriberStub struct {
	handler               func(SubscriptionEvent)
	subscribeCalls        int
	closed                int
	confirmedBeforeReturn bool
}

func (s *subscriberStub) SubscribeRoom(_ context.Context, _ string, handler func(SubscriptionEvent)) (RoomSubscription, error) {
	s.subscribeCalls++
	s.handler = handler
	s.confirmedBeforeReturn = true
	return subscriptionFunc(func() error { s.closed++; return nil }), nil
}
func (s *subscriberStub) emit(event SubscriptionEvent) { s.handler(event) }

type snapshotLoaderStub struct {
	revisions []uint64
	calls     int
}

type runtimeSnapshotLoader struct {
	mu       sync.Mutex
	revision uint64
	loads    int
}

type runtimeOutboxStore struct {
	records  []worker.OutboxRecord
	releases int
	marked   int
}

func (s *runtimeOutboxStore) ClaimOutbox(context.Context, time.Time, int, time.Duration) ([]worker.OutboxRecord, error) {
	return s.records, nil
}
func (s *runtimeOutboxStore) OutboxPendingStats(context.Context, time.Time) (worker.OutboxPendingStats, error) {
	return worker.OutboxPendingStats{}, nil
}

func (s *runtimeOutboxStore) MarkOutboxPublished(context.Context, worker.OutboxRecord, time.Time) error {
	s.marked++
	return nil
}

func (s *runtimeOutboxStore) ReleaseOutbox(context.Context, worker.OutboxRecord, time.Time) error {
	s.releases++
	return nil
}

func (s *runtimeOutboxStore) CleanupPublishedOutbox(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func (s *runtimeSnapshotLoader) LoadRoom(context.Context, string) (application.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return application.Snapshot{Revision: s.revision}, nil
}

func (s *runtimeSnapshotLoader) setRevision(revision uint64) {
	s.mu.Lock()
	s.revision = revision
	s.mu.Unlock()
}

func (s *runtimeSnapshotLoader) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func receiveRevision(t *testing.T, revisions <-chan uint64, timeout time.Duration) uint64 {
	t.Helper()
	select {
	case revision := <-revisions:
		return revision
	case <-time.After(timeout):
		t.Fatal("timed out waiting for reconciled revision")
		return 0
	}
}

func (s *snapshotLoaderStub) LoadRoom(context.Context, string) (application.Snapshot, error) {
	revision := s.revisions[s.calls]
	s.calls++
	return application.Snapshot{Revision: revision}, nil
}

func equalRevisions(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
