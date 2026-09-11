package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestSkewedLastHeartbeatsDoNotFabricateWinner(t *testing.T) {
	repository, _ := memory.New()
	aggregate, _ := domain.New("ABC234", "host-id")
	if _, err := repository.Create(context.Background(), aggregate, application.DigestToken("host-token")); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	deadline := observed.Add(13 * time.Second)
	window := application.PresenceWindow{ObservedAt: observed, ProofAfter: deadline, Deadline: deadline, EvaluateAt: deadline.Add(3 * time.Second)}
	_, leases, err := repository.Join(context.Background(), "ABC234", "guest-id", application.DigestToken("guest-token"), window)
	if err != nil {
		t.Fatal(err)
	}
	hostObserved := observed.Add(4 * time.Second)
	hostDeadline := hostObserved.Add(13 * time.Second)
	hostWindow := application.PresenceWindow{ObservedAt: hostObserved, ProofAfter: hostDeadline, Deadline: hostDeadline, EvaluateAt: hostDeadline.Add(3 * time.Second)}
	_, err = repository.RefreshPresence(context.Background(), "ABC234", application.DigestToken("host-token"), hostWindow)
	if err != nil {
		t.Fatal(err)
	}
	guest := leases[1]
	if _, changed, err := repository.ForfeitExpired(context.Background(), guest, guest.EvaluateAt); err != nil || changed {
		t.Fatalf("forfeit without connected opponent changed=%v error=%v", changed, err)
	}
	snapshot, _ := repository.Snapshot(context.Background(), "ABC234")
	if snapshot.State.Resolved {
		t.Fatalf("room resolved without a remaining player: %#v", snapshot)
	}
}

func TestRefreshReconstructsExpiredOpponentAndPreservesGenerationsAcrossRounds(t *testing.T) {
	repository, _ := memory.New()
	aggregate, _ := domain.New("ABC234", "host-id")
	hostDigest := application.DigestToken("host-token")
	guestDigest := application.DigestToken("guest-token")
	if _, err := repository.Create(context.Background(), aggregate, hostDigest); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	deadline := observed.Add(13 * time.Second)
	window := application.PresenceWindow{ObservedAt: observed, ProofAfter: deadline, Deadline: deadline, EvaluateAt: deadline.Add(3 * time.Second)}
	_, initial, err := repository.Join(context.Background(), "ABC234", "guest-id", guestDigest, window)
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range initial {
		if _, changed, expireErr := repository.ForfeitExpired(context.Background(), lease, window.EvaluateAt); expireErr != nil || changed {
			t.Fatalf("both-expired lease changed=%v error=%v", changed, expireErr)
		}
	}

	hostObserved := window.EvaluateAt.Add(time.Second)
	hostDeadline := hostObserved.Add(13 * time.Second)
	hostWindow := application.PresenceWindow{ObservedAt: hostObserved, ProofAfter: hostDeadline, Deadline: hostDeadline, EvaluateAt: hostDeadline.Add(3 * time.Second)}
	refreshed, err := repository.RefreshPresence(context.Background(), "ABC234", hostDigest, hostWindow)
	if err != nil {
		t.Fatal(err)
	}
	guestLease := memoryLeaseForPlayer(t, refreshed, "guest-id")
	if guestLease.Deadline != deadline || guestLease.ProofAfter != deadline || guestLease.EvaluateAt != window.EvaluateAt {
		t.Fatalf("reconstructed guest lease = %#v", guestLease)
	}
	if _, changed, err := repository.ForfeitExpired(context.Background(), guestLease, hostObserved); err != nil || !changed {
		t.Fatalf("reconnected-host forfeit changed=%v error=%v", changed, err)
	}

	if _, err = repository.Mutate(context.Background(), "ABC234", guestDigest, func(room *domain.Room, playerID string) error {
		return room.RequestNextRound(playerID, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.Mutate(context.Background(), "ABC234", hostDigest, func(room *domain.Room, playerID string) error {
		return room.RequestNextRound(playerID, 1)
	}); err != nil {
		t.Fatal(err)
	}
	nextRound, err := repository.RefreshPresence(context.Background(), "ABC234", hostDigest, hostWindow)
	if err != nil {
		t.Fatal(err)
	}
	nextGuestLease := memoryLeaseForPlayer(t, nextRound, "guest-id")
	if nextGuestLease.Round != 2 || nextGuestLease.Generation <= guestLease.Generation || !nextGuestLease.Active {
		t.Fatalf("next-round guest lease = %#v, previous = %#v", nextGuestLease, guestLease)
	}
}

func memoryLeaseForPlayer(t *testing.T, leases []application.PresenceLease, playerID string) application.PresenceLease {
	t.Helper()
	for _, lease := range leases {
		if lease.PlayerID == playerID {
			return lease
		}
	}
	t.Fatalf("lease for %q not found in %#v", playerID, leases)
	return application.PresenceLease{}
}
