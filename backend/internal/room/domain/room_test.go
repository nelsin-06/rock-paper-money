package domain_test

import (
	"errors"
	"testing"

	"example.com/rock-paper-money/internal/room/domain"
)

func TestRoundLifecycleAndMovePrivacy(t *testing.T) {
	r := readyRoom(t)
	if err := r.SubmitMove("host", domain.Rock); err != nil {
		t.Fatal(err)
	}
	waiting := r.State()
	if waiting.Resolved || len(waiting.Moves) != 0 || waiting.Result != "" || !waiting.Players[0].Submitted {
		t.Fatalf("private waiting state = %#v", waiting)
	}
	if err := r.SubmitMove("guest", domain.Scissors); err != nil {
		t.Fatal(err)
	}
	resolved := r.State()
	if !resolved.Resolved || resolved.Result != domain.PlayerOneWins || resolved.Players[0].Wins != 1 || len(resolved.Moves) != 2 {
		t.Fatalf("resolved state = %#v", resolved)
	}
	if err := r.SubmitMove("host", domain.Paper); !errors.Is(err, domain.ErrDuplicateMove) {
		t.Fatalf("duplicate move error = %v", err)
	}
	if err := r.RequestNextRound("guest", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.RequestNextRound("host", 1); err != nil {
		t.Fatal(err)
	}
	next := r.State()
	if next.Round != 2 || next.Resolved || next.Players[0].Wins != 1 || len(next.Moves) != 0 {
		t.Fatalf("next round = %#v", next)
	}
}

func TestDetermineResult(t *testing.T) {
	tests := []struct {
		name string
		one  domain.Move
		two  domain.Move
		want domain.Result
	}{
		{"rock draws rock", domain.Rock, domain.Rock, domain.Draw},
		{"rock loses to paper", domain.Rock, domain.Paper, domain.PlayerTwoWins},
		{"rock beats scissors", domain.Rock, domain.Scissors, domain.PlayerOneWins},
		{"paper beats rock", domain.Paper, domain.Rock, domain.PlayerOneWins},
		{"paper draws paper", domain.Paper, domain.Paper, domain.Draw},
		{"paper loses to scissors", domain.Paper, domain.Scissors, domain.PlayerTwoWins},
		{"scissors loses to rock", domain.Scissors, domain.Rock, domain.PlayerTwoWins},
		{"scissors beats paper", domain.Scissors, domain.Paper, domain.PlayerOneWins},
		{"scissors draws scissors", domain.Scissors, domain.Scissors, domain.Draw},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := domain.DetermineResult(tt.one, tt.two)
			if err != nil {
				t.Fatal(err)
			}
			if result != tt.want {
				t.Fatalf("result = %q, want %q", result, tt.want)
			}
		})
	}
}

func TestRoomRejectsInvalidTransitions(t *testing.T) {
	tests := []struct {
		name string
		run  func(*domain.Room) error
		want error
	}{
		{"move before join", func(r *domain.Room) error { return r.SubmitMove("host", domain.Rock) }, domain.ErrRoomNotReady},
		{"unknown player", func(r *domain.Room) error { _ = r.Join("guest"); return r.SubmitMove("other", domain.Rock) }, domain.ErrUnknownPlayer},
		{"invalid move", func(r *domain.Room) error { _ = r.Join("guest"); return r.SubmitMove("host", "lizard") }, domain.ErrInvalidMove},
		{"next round unresolved", func(r *domain.Room) error { _ = r.Join("guest"); return r.RequestNextRound("host", 1) }, domain.ErrRoundNotResolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := domain.New("ROOM", "host")
			if err := tt.run(r); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRestoreRoundTripAndRejectsCorruptState(t *testing.T) {
	r := readyRoom(t)
	_ = r.SubmitMove("host", domain.Paper)
	_ = r.SubmitMove("guest", domain.Rock)
	restored, err := domain.Restore(r.PersistenceState())
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.State(); !got.Resolved || got.Result != domain.PlayerOneWins || got.Players[0].Wins != 1 {
		t.Fatalf("restored = %#v", got)
	}
	bad := r.PersistenceState()
	bad.Result = domain.Draw
	if _, err := domain.Restore(bad); !errors.Is(err, domain.ErrInvalidState) {
		t.Fatalf("corrupt restore error = %v", err)
	}
}

func TestLeaveClosesRoom(t *testing.T) {
	r := readyRoom(t)
	if err := r.Leave("guest"); err != nil {
		t.Fatal(err)
	}
	if !r.State().Closed {
		t.Fatal("room not closed")
	}
	if err := r.SubmitMove("host", domain.Rock); !errors.Is(err, domain.ErrRoomClosed) {
		t.Fatalf("error = %v", err)
	}
}
func readyRoom(t *testing.T) *domain.Room {
	t.Helper()
	r, err := domain.New("ROOM", "host")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Join("guest"); err != nil {
		t.Fatal(err)
	}
	return r
}
