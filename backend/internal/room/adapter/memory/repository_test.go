package memory_test

import (
	"context"
	"errors"
	"testing"

	"example.com/rock-paper-money/internal/room/adapter/memory"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

func TestRepositoryRollsBackFailedMutation(t *testing.T) {
	repository, _ := memory.New()
	aggregate, err := domain.New("ABC234", "host-id")
	if err != nil {
		t.Fatal(err)
	}
	digest := application.DigestToken("host-token")
	if _, err = repository.Create(context.Background(), aggregate, digest); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.Mutate(context.Background(), "ABC234", digest, func(room *domain.Room, playerID string) error {
		if err := room.Join("guest-id"); err != nil {
			return err
		}
		return errors.New("forced failure")
	}); err == nil {
		t.Fatal("mutation error = nil")
	}
	snapshot, err := repository.Snapshot(context.Background(), "ABC234")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || len(snapshot.State.Players) != 1 {
		t.Fatalf("failed mutation changed snapshot: %#v", snapshot)
	}
}
