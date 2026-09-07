package room_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/game"
	"example.com/rock-paper-money/internal/room"
)

func TestStoreSubscriptionsReportSuccessfulRoomChanges(t *testing.T) {
	store := newStoreWithRoom(t)
	initial, changes, unsubscribe, err := store.Subscribe("ABCD")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer unsubscribe()
	if initial.Revision != 1 || len(initial.State.Players) != 1 {
		t.Fatalf("initial snapshot = %#v, want revision 1 with host", initial)
	}

	if err := store.Join("ABCD", "guest"); err != nil {
		t.Fatalf("Join() error = %v", err)
	}
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("successful Join() did not notify subscriber")
	}

	updated, err := store.Snapshot("ABCD")
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if updated.Revision != 2 || !updated.State.Ready {
		t.Errorf("updated snapshot = %#v, want revision 2 ready room", updated)
	}

	if err := store.Join("ABCD", "third"); !errors.Is(err, room.ErrRoomFull) {
		t.Fatalf("Join(third) error = %v, want %v", err, room.ErrRoomFull)
	}
	select {
	case <-changes:
		t.Fatal("failed mutation notified subscriber")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestStoreSubscriptionsCoalesceAndUnsubscribeSafely(t *testing.T) {
	store := readyStore(t)
	_, changes, unsubscribe, err := store.Subscribe("ABCD")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	if err := store.SubmitMove("ABCD", "host", game.Rock); err != nil {
		t.Fatalf("host SubmitMove() error = %v", err)
	}
	if err := store.SubmitMove("ABCD", "guest", game.Scissors); err != nil {
		t.Fatalf("guest SubmitMove() error = %v", err)
	}
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("room changes were not reported")
	}
	select {
	case <-changes:
		t.Fatal("notifications were not coalesced for a slow subscriber")
	default:
	}

	unsubscribe()
	unsubscribe()
	if err := store.StartNextRound("ABCD"); err != nil {
		t.Fatalf("StartNextRound() error = %v", err)
	}
	select {
	case <-changes:
		t.Fatal("unsubscribed listener received a notification")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestStoreCreateAndFindState(t *testing.T) {
	store := room.NewStore()

	created, err := store.Create("ABCD", "host")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	found, err := store.State("ABCD")
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}

	if created.Code != "ABCD" || len(created.Players) != 1 || created.Players[0].ID != "host" {
		t.Errorf("Create() state = %#v, want room ABCD with host", created)
	}
	if found.Code != created.Code || len(found.Players) != 1 || found.Players[0] != created.Players[0] {
		t.Errorf("State() = %#v, want %#v", found, created)
	}
}

func TestStoreRejectsDuplicateRoomCode(t *testing.T) {
	store := room.NewStore()
	if _, err := store.Create("ABCD", "host"); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	state, err := store.Create("ABCD", "other-host")
	if !errors.Is(err, room.ErrDuplicateRoom) {
		t.Fatalf("second Create() error = %v, want %v", err, room.ErrDuplicateRoom)
	}
	if state.Code != "" || len(state.Players) != 0 || state.Ready || state.Resolved || len(state.Moves) != 0 || state.Result != "" {
		t.Errorf("second Create() state = %#v, want zero state", state)
	}
}

func TestStoreCreateDelegatesRoomValidation(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		hostID string
		want   error
	}{
		{name: "missing code", hostID: "host", want: room.ErrEmptyCode},
		{name: "missing host identity", code: "ABCD", want: room.ErrMissingPlayerID},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := room.NewStore()
			if _, err := store.Create(test.code, test.hostID); !errors.Is(err, test.want) {
				t.Errorf("Create() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestStoreReturnsNotFoundForEveryRoomOperation(t *testing.T) {
	store := room.NewStore()
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "state",
			call: func() error {
				_, err := store.State("NONE")
				return err
			},
		},
		{name: "join", call: func() error { return store.Join("NONE", "guest") }},
		{name: "submit move", call: func() error { return store.SubmitMove("NONE", "host", game.Rock) }},
		{name: "start next round", call: func() error { return store.StartNextRound("NONE") }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, room.ErrRoomNotFound) {
				t.Errorf("operation error = %v, want %v", err, room.ErrRoomNotFound)
			}
		})
	}
}

func TestStoreDelegatesRoomInvariants(t *testing.T) {
	store := newStoreWithRoom(t)

	if err := store.Join("ABCD", ""); !errors.Is(err, room.ErrMissingPlayerID) {
		t.Errorf("Join(empty) error = %v, want %v", err, room.ErrMissingPlayerID)
	}
	if err := store.Join("ABCD", "host"); !errors.Is(err, room.ErrDuplicatePlayer) {
		t.Errorf("Join(host) error = %v, want %v", err, room.ErrDuplicatePlayer)
	}
	if err := store.SubmitMove("ABCD", "host", game.Rock); !errors.Is(err, room.ErrRoomNotReady) {
		t.Errorf("SubmitMove() before guest error = %v, want %v", err, room.ErrRoomNotReady)
	}
	if err := store.Join("ABCD", "guest"); err != nil {
		t.Fatalf("Join(guest) error = %v", err)
	}
	if err := store.Join("ABCD", "third"); !errors.Is(err, room.ErrRoomFull) {
		t.Errorf("Join(third) error = %v, want %v", err, room.ErrRoomFull)
	}
	if err := store.SubmitMove("ABCD", "stranger", game.Rock); !errors.Is(err, room.ErrUnknownPlayer) {
		t.Errorf("SubmitMove(stranger) error = %v, want %v", err, room.ErrUnknownPlayer)
	}
	if err := store.SubmitMove("ABCD", "host", game.Move("lizard")); !errors.Is(err, room.ErrInvalidMove) {
		t.Errorf("SubmitMove(invalid) error = %v, want %v", err, room.ErrInvalidMove)
	}
	if err := store.StartNextRound("ABCD"); !errors.Is(err, room.ErrRoundNotResolved) {
		t.Errorf("StartNextRound() error = %v, want %v", err, room.ErrRoundNotResolved)
	}

	if err := store.SubmitMove("ABCD", "host", game.Rock); err != nil {
		t.Fatalf("host SubmitMove() error = %v", err)
	}
	if err := store.SubmitMove("ABCD", "host", game.Paper); !errors.Is(err, room.ErrDuplicateMove) {
		t.Errorf("second host SubmitMove() error = %v, want %v", err, room.ErrDuplicateMove)
	}
	if err := store.SubmitMove("ABCD", "guest", game.Scissors); err != nil {
		t.Fatalf("guest SubmitMove() error = %v", err)
	}
	if err := store.StartNextRound("ABCD"); err != nil {
		t.Fatalf("StartNextRound() after resolution error = %v", err)
	}

	state, err := store.State("ABCD")
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if !state.Ready || state.Resolved || state.Players[0].Wins != 1 || len(state.Moves) != 0 {
		t.Errorf("next-round state = %#v, want ready unresolved room preserving host win", state)
	}
	if state.Players[0].Submitted || state.Players[1].Submitted {
		t.Errorf("next-round submitted readiness = (%t, %t), want (false, false)", state.Players[0].Submitted, state.Players[1].Submitted)
	}
}

func TestStoreStatesAreDetached(t *testing.T) {
	store := readyStore(t)
	if err := store.SubmitMove("ABCD", "host", game.Rock); err != nil {
		t.Fatalf("host SubmitMove() error = %v", err)
	}
	if err := store.SubmitMove("ABCD", "guest", game.Scissors); err != nil {
		t.Fatalf("guest SubmitMove() error = %v", err)
	}

	state, err := store.State("ABCD")
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	state.Players[0].Wins = 99
	state.Moves[0].Move = game.Paper

	unchanged, err := store.State("ABCD")
	if err != nil {
		t.Fatalf("second State() error = %v", err)
	}
	if unchanged.Players[0].Wins != 1 || unchanged.Moves[0].Move != game.Rock {
		t.Errorf("state after snapshot mutation = %#v, want original room data", unchanged)
	}
}

func TestStoreConcurrentCreateAllowsOneRoom(t *testing.T) {
	store := room.NewStore()
	start := make(chan struct{})
	errorsByCall := make(chan error, 2)

	var workers sync.WaitGroup
	for _, hostID := range []string{"host-one", "host-two"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := store.Create("SAME", hostID)
			errorsByCall <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errorsByCall)

	var successes, duplicates int
	for err := range errorsByCall {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, room.ErrDuplicateRoom):
			duplicates++
		default:
			t.Errorf("Create() error = %v, want nil or %v", err, room.ErrDuplicateRoom)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Errorf("concurrent Create() results = %d successes and %d duplicates, want 1 and 1", successes, duplicates)
	}
}

func TestStoreConcurrentMovesResolveRoundOnce(t *testing.T) {
	store := readyStore(t)
	start := make(chan struct{})
	errorsByPlayer := make(chan error, 2)

	var players sync.WaitGroup
	players.Add(2)
	go func() {
		defer players.Done()
		<-start
		errorsByPlayer <- store.SubmitMove("ABCD", "host", game.Rock)
	}()
	go func() {
		defer players.Done()
		<-start
		errorsByPlayer <- store.SubmitMove("ABCD", "guest", game.Scissors)
	}()
	close(start)
	players.Wait()
	close(errorsByPlayer)

	for err := range errorsByPlayer {
		if err != nil {
			t.Errorf("concurrent SubmitMove() error = %v", err)
		}
	}

	state, err := store.State("ABCD")
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if !state.Resolved || state.Result != game.PlayerOneWins {
		t.Errorf("resolved state = %#v, want one player-one victory", state)
	}
	if len(state.Moves) != 2 || state.Players[0].Wins != 1 || state.Players[1].Wins != 0 {
		t.Errorf("resolved state = %#v, want two moves and scores 1-0", state)
	}
	if !state.Players[0].Submitted || !state.Players[1].Submitted {
		t.Errorf("resolved submitted readiness = (%t, %t), want (true, true)", state.Players[0].Submitted, state.Players[1].Submitted)
	}
}

func newStoreWithRoom(t *testing.T) *room.Store {
	t.Helper()
	store := room.NewStore()
	if _, err := store.Create("ABCD", "host"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return store
}

func readyStore(t *testing.T) *room.Store {
	t.Helper()
	store := newStoreWithRoom(t)
	if err := store.Join("ABCD", "guest"); err != nil {
		t.Fatalf("Join() error = %v", err)
	}
	return store
}
