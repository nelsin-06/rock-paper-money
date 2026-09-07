package room_test

import (
	"errors"
	"testing"

	"example.com/rock-paper-money/internal/game"
	"example.com/rock-paper-money/internal/room"
)

func TestNewCreatesRoomWithHost(t *testing.T) {
	r := newRoom(t)

	state := r.State()
	if state.Code != "ABCD" {
		t.Errorf("State().Code = %q, want %q", state.Code, "ABCD")
	}
	if len(state.Players) != 1 || state.Players[0] != (room.Player{ID: "host"}) {
		t.Errorf("State().Players = %#v, want only the host", state.Players)
	}
	if state.Ready {
		t.Error("State().Ready = true, want false before a second player joins")
	}
	if state.Resolved || len(state.Moves) != 0 || state.Result != "" {
		t.Errorf("new room exposes round data: %#v", state)
	}
}

func TestNewRejectsMissingValues(t *testing.T) {
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
			got, err := room.New(test.code, test.hostID)
			if !errors.Is(err, test.want) {
				t.Fatalf("New() error = %v, want %v", err, test.want)
			}
			if got != nil {
				t.Errorf("New() room = %#v, want nil", got)
			}
		})
	}
}

func TestJoinAcceptsExactlyOneDistinctSecondPlayer(t *testing.T) {
	r := newRoom(t)

	if err := r.Join(""); !errors.Is(err, room.ErrMissingPlayerID) {
		t.Errorf("Join(empty) error = %v, want %v", err, room.ErrMissingPlayerID)
	}
	if err := r.Join("host"); !errors.Is(err, room.ErrDuplicatePlayer) {
		t.Errorf("Join(host) error = %v, want %v", err, room.ErrDuplicatePlayer)
	}
	if err := r.Join("guest"); err != nil {
		t.Fatalf("Join(guest) error = %v", err)
	}
	if !r.State().Ready {
		t.Error("State().Ready = false, want true after the guest joins")
	}
	if err := r.Join("guest"); !errors.Is(err, room.ErrDuplicatePlayer) {
		t.Errorf("joining guest twice error = %v, want %v", err, room.ErrDuplicatePlayer)
	}
	if err := r.Join("third"); !errors.Is(err, room.ErrRoomFull) {
		t.Errorf("joining third player error = %v, want %v", err, room.ErrRoomFull)
	}
}

func TestSubmitMoveKeepsMovesPrivateUntilItResolvesOnce(t *testing.T) {
	r := readyRoom(t)

	if err := r.SubmitMove("host", game.Rock); err != nil {
		t.Fatalf("host SubmitMove() error = %v", err)
	}

	beforeResolution := r.State()
	if beforeResolution.Resolved {
		t.Error("State().Resolved = true after one move, want false")
	}
	if len(beforeResolution.Moves) != 0 || beforeResolution.Result != "" {
		t.Errorf("State() exposed private round data: %#v", beforeResolution)
	}
	if !beforeResolution.Players[0].Submitted || beforeResolution.Players[1].Submitted {
		t.Errorf("submitted readiness = (%t, %t), want (true, false)", beforeResolution.Players[0].Submitted, beforeResolution.Players[1].Submitted)
	}

	if err := r.SubmitMove("guest", game.Scissors); err != nil {
		t.Fatalf("guest SubmitMove() error = %v", err)
	}

	state := r.State()
	if !state.Resolved {
		t.Fatal("State().Resolved = false, want true")
	}
	if state.Result != game.PlayerOneWins {
		t.Errorf("State().Result = %q, want %q", state.Result, game.PlayerOneWins)
	}
	wantMoves := []room.PlayerMove{
		{PlayerID: "host", Move: game.Rock},
		{PlayerID: "guest", Move: game.Scissors},
	}
	if len(state.Moves) != len(wantMoves) {
		t.Fatalf("State().Moves = %#v, want %#v", state.Moves, wantMoves)
	}
	for i := range wantMoves {
		if state.Moves[i] != wantMoves[i] {
			t.Errorf("State().Moves[%d] = %#v, want %#v", i, state.Moves[i], wantMoves[i])
		}
	}
	assertWins(t, state, 1, 0)

	if err := r.SubmitMove("host", game.Paper); !errors.Is(err, room.ErrDuplicateMove) {
		t.Errorf("SubmitMove() after resolution error = %v, want %v", err, room.ErrDuplicateMove)
	}
	assertWins(t, r.State(), 1, 0)
}

func TestSubmitMoveRejectsInvalidTransitions(t *testing.T) {
	t.Run("before second player joins", func(t *testing.T) {
		r := newRoom(t)
		if err := r.SubmitMove("host", game.Rock); !errors.Is(err, room.ErrRoomNotReady) {
			t.Errorf("SubmitMove() error = %v, want %v", err, room.ErrRoomNotReady)
		}
	})

	t.Run("unknown player", func(t *testing.T) {
		r := readyRoom(t)
		if err := r.SubmitMove("stranger", game.Rock); !errors.Is(err, room.ErrUnknownPlayer) {
			t.Errorf("SubmitMove() error = %v, want %v", err, room.ErrUnknownPlayer)
		}
	})

	t.Run("invalid move does not consume submission", func(t *testing.T) {
		r := readyRoom(t)
		if err := r.SubmitMove("host", game.Move("lizard")); !errors.Is(err, room.ErrInvalidMove) {
			t.Errorf("SubmitMove() error = %v, want %v", err, room.ErrInvalidMove)
		}
		if err := r.SubmitMove("host", game.Paper); err != nil {
			t.Errorf("valid retry SubmitMove() error = %v", err)
		}
	})

	t.Run("duplicate submission", func(t *testing.T) {
		r := readyRoom(t)
		if err := r.SubmitMove("host", game.Rock); err != nil {
			t.Fatalf("first SubmitMove() error = %v", err)
		}
		if err := r.SubmitMove("host", game.Paper); !errors.Is(err, room.ErrDuplicateMove) {
			t.Errorf("second SubmitMove() error = %v, want %v", err, room.ErrDuplicateMove)
		}
	})
}

func TestResolutionAwardsOnlyTheWinner(t *testing.T) {
	tests := []struct {
		name      string
		hostMove  game.Move
		guestMove game.Move
		hostWins  int
		guestWins int
	}{
		{name: "guest wins", hostMove: game.Rock, guestMove: game.Paper, guestWins: 1},
		{name: "draw", hostMove: game.Scissors, guestMove: game.Scissors},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := readyRoom(t)
			if err := r.SubmitMove("host", test.hostMove); err != nil {
				t.Fatalf("host SubmitMove() error = %v", err)
			}
			if err := r.SubmitMove("guest", test.guestMove); err != nil {
				t.Fatalf("guest SubmitMove() error = %v", err)
			}
			assertWins(t, r.State(), test.hostWins, test.guestWins)
		})
	}
}

func TestNextRoundStartsOnlyAfterBothPlayersRequestIt(t *testing.T) {
	r := readyRoom(t)
	if err := r.RequestNextRound("host", 1); !errors.Is(err, room.ErrRoundNotResolved) {
		t.Errorf("RequestNextRound() before resolution error = %v, want %v", err, room.ErrRoundNotResolved)
	}

	if err := r.SubmitMove("host", game.Rock); err != nil {
		t.Fatalf("host SubmitMove() error = %v", err)
	}
	if err := r.SubmitMove("guest", game.Scissors); err != nil {
		t.Fatalf("guest SubmitMove() error = %v", err)
	}
	if err := r.RequestNextRound("guest", 1); err != nil {
		t.Fatalf("guest RequestNextRound() error = %v", err)
	}

	waiting := r.State()
	if !waiting.Resolved || !waiting.Players[1].WantsNextRound || waiting.Players[0].WantsNextRound {
		t.Fatalf("state after first request = %#v, want resolved round with only guest requesting", waiting)
	}
	if err := r.RequestNextRound("guest", 1); !errors.Is(err, room.ErrNextRoundRequested) {
		t.Errorf("duplicate RequestNextRound() error = %v, want %v", err, room.ErrNextRoundRequested)
	}
	if err := r.RequestNextRound("host", 1); err != nil {
		t.Fatalf("host RequestNextRound() error = %v", err)
	}

	state := r.State()
	if !state.Ready || state.Resolved || state.Round != 2 || len(state.Moves) != 0 || state.Result != "" {
		t.Errorf("next round state = %#v, want ready room with cleared round data", state)
	}
	if state.Players[0].Submitted || state.Players[1].Submitted {
		t.Errorf("next round submitted readiness = (%t, %t), want (false, false)", state.Players[0].Submitted, state.Players[1].Submitted)
	}
	assertWins(t, state, 1, 0)

	if err := r.SubmitMove("host", game.Rock); err != nil {
		t.Fatalf("second-round host SubmitMove() error = %v", err)
	}
	if err := r.SubmitMove("guest", game.Paper); err != nil {
		t.Fatalf("second-round guest SubmitMove() error = %v", err)
	}
	assertWins(t, r.State(), 1, 1)

	if err := r.RequestNextRound("host", 1); !errors.Is(err, room.ErrStaleRound) {
		t.Errorf("stale RequestNextRound() error = %v, want %v", err, room.ErrStaleRound)
	}
}

func TestLeaveClosesRoomForBothPlayers(t *testing.T) {
	r := readyRoom(t)
	if err := r.Leave("guest"); err != nil {
		t.Fatalf("Leave() error = %v", err)
	}
	if !r.State().Closed {
		t.Fatal("State().Closed = false, want true")
	}
	if err := r.SubmitMove("host", game.Rock); !errors.Is(err, room.ErrRoomClosed) {
		t.Errorf("SubmitMove() after leave error = %v, want %v", err, room.ErrRoomClosed)
	}
	if err := r.Leave("host"); !errors.Is(err, room.ErrRoomClosed) {
		t.Errorf("second Leave() error = %v, want %v", err, room.ErrRoomClosed)
	}
}

func TestStateIsDetachedFromRoom(t *testing.T) {
	r := readyRoom(t)
	if err := r.SubmitMove("host", game.Rock); err != nil {
		t.Fatalf("host SubmitMove() error = %v", err)
	}
	if err := r.SubmitMove("guest", game.Scissors); err != nil {
		t.Fatalf("guest SubmitMove() error = %v", err)
	}

	state := r.State()
	state.Players[0].Wins = 99
	state.Moves[0].Move = game.Paper

	unchanged := r.State()
	assertWins(t, unchanged, 1, 0)
	if unchanged.Moves[0].Move != game.Rock {
		t.Errorf("State().Moves[0].Move = %q after snapshot mutation, want %q", unchanged.Moves[0].Move, game.Rock)
	}
}

func newRoom(t *testing.T) *room.Room {
	t.Helper()
	r, err := room.New("ABCD", "host")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return r
}

func readyRoom(t *testing.T) *room.Room {
	t.Helper()
	r := newRoom(t)
	if err := r.Join("guest"); err != nil {
		t.Fatalf("Join() error = %v", err)
	}
	return r
}

func assertWins(t *testing.T, state room.State, hostWins, guestWins int) {
	t.Helper()
	if len(state.Players) != 2 {
		t.Fatalf("len(State().Players) = %d, want 2", len(state.Players))
	}
	if state.Players[0].Wins != hostWins || state.Players[1].Wins != guestWins {
		t.Errorf(
			"player wins = (%d, %d), want (%d, %d)",
			state.Players[0].Wins,
			state.Players[1].Wins,
			hostWins,
			guestWins,
		)
	}
}
