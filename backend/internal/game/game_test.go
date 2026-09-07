package game_test

import (
	"testing"

	"example.com/rock-paper-money/internal/game"
)

func TestDetermineResult(t *testing.T) {
	tests := []struct {
		name      string
		playerOne game.Move
		playerTwo game.Move
		want      game.Result
	}{
		{name: "rock draws with rock", playerOne: game.Rock, playerTwo: game.Rock, want: game.Draw},
		{name: "rock loses to paper", playerOne: game.Rock, playerTwo: game.Paper, want: game.PlayerTwoWins},
		{name: "rock beats scissors", playerOne: game.Rock, playerTwo: game.Scissors, want: game.PlayerOneWins},
		{name: "paper beats rock", playerOne: game.Paper, playerTwo: game.Rock, want: game.PlayerOneWins},
		{name: "paper draws with paper", playerOne: game.Paper, playerTwo: game.Paper, want: game.Draw},
		{name: "paper loses to scissors", playerOne: game.Paper, playerTwo: game.Scissors, want: game.PlayerTwoWins},
		{name: "scissors loses to rock", playerOne: game.Scissors, playerTwo: game.Rock, want: game.PlayerTwoWins},
		{name: "scissors beats paper", playerOne: game.Scissors, playerTwo: game.Paper, want: game.PlayerOneWins},
		{name: "scissors draws with scissors", playerOne: game.Scissors, playerTwo: game.Scissors, want: game.Draw},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := game.DetermineResult(test.playerOne, test.playerTwo)
			if err != nil {
				t.Fatalf("DetermineResult() error = %v", err)
			}
			if got != test.want {
				t.Errorf("DetermineResult() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDetermineResultRejectsInvalidMoves(t *testing.T) {
	tests := []struct {
		name      string
		playerOne game.Move
		playerTwo game.Move
	}{
		{name: "invalid player one move", playerOne: game.Move("lizard"), playerTwo: game.Rock},
		{name: "invalid player two move", playerOne: game.Rock, playerTwo: game.Move("lizard")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := game.DetermineResult(test.playerOne, test.playerTwo)
			if err == nil {
				t.Fatal("DetermineResult() error = nil, want an invalid move error")
			}
			if got != "" {
				t.Errorf("DetermineResult() = %q, want no result", got)
			}
		})
	}
}

func TestIsValidMove(t *testing.T) {
	tests := []struct {
		name string
		move game.Move
		want bool
	}{
		{name: "rock", move: game.Rock, want: true},
		{name: "paper", move: game.Paper, want: true},
		{name: "scissors", move: game.Scissors, want: true},
		{name: "unknown move", move: game.Move("lizard"), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := game.IsValidMove(test.move); got != test.want {
				t.Errorf("IsValidMove(%q) = %t, want %t", test.move, got, test.want)
			}
		})
	}
}
