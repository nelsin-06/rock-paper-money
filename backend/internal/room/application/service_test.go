package application_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/room/adapter/memory"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

func TestServiceUsesOpaqueCredentialsAndRevisions(t *testing.T) {
	service := testService()
	host, err := service.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(context.Background(), host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if host.PlayerToken == guest.PlayerToken {
		t.Fatal("tokens reused")
	}
	if _, _, err = service.Authenticate(context.Background(), host.RoomCode, host.PlayerToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.Authenticate(context.Background(), host.RoomCode, "wrong"); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("error = %v", err)
	}
	if err = service.SubmitMove(context.Background(), host.RoomCode, host.PlayerToken, domain.Rock); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(context.Background(), host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 3 || len(snapshot.State.Moves) != 0 || !snapshot.State.Players[0].Submitted {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	for _, p := range snapshot.State.Players {
		if p.ID == host.PlayerToken || p.ID == guest.PlayerToken {
			t.Fatal("credential used as player identity")
		}
	}
}

func TestConcurrentMovesResolveExactlyOnce(t *testing.T) {
	service := testService()
	host, _ := service.Create(context.Background())
	guest, _ := service.Join(context.Background(), host.RoomCode)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- service.SubmitMove(context.Background(), host.RoomCode, host.PlayerToken, domain.Rock)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs <- service.SubmitMove(context.Background(), host.RoomCode, guest.PlayerToken, domain.Scissors)
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := service.Snapshot(context.Background(), host.RoomCode)
	if snapshot.Revision != 4 || snapshot.State.Players[0].Wins != 1 || !snapshot.State.Resolved {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestSubscriptionCoalescesChanges(t *testing.T) {
	service := testService()
	host, _ := service.Create(context.Background())
	initial, changes, unsubscribe, err := service.Subscribe(context.Background(), host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	if initial.Revision != 1 {
		t.Fatalf("revision = %d", initial.Revision)
	}
	_, _ = service.Join(context.Background(), host.RoomCode)
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("missing notification")
	}
}

func testService() *application.Service {
	repository, events := memory.New()
	values := []string{"HOSTTOKEN", "host-id", "GUESTTOKEN", "guest-id"}
	var mu sync.Mutex
	generator := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		value := values[0]
		values = values[1:]
		return value, nil
	}
	return application.NewServiceWithGenerators(repository, events, func() (string, error) { return "ABC234", nil }, generator, generator)
}
