package game

import "fmt"

type Move string

const (
	Rock     Move = "rock"
	Paper    Move = "paper"
	Scissors Move = "scissors"
)

type Result string

const (
	Draw          Result = "draw"
	PlayerOneWins Result = "player_one_wins"
	PlayerTwoWins Result = "player_two_wins"
)

func IsValidMove(move Move) bool {
	switch move {
	case Rock, Paper, Scissors:
		return true
	default:
		return false
	}
}

func DetermineResult(playerOne, playerTwo Move) (Result, error) {
	if !IsValidMove(playerOne) {
		return "", fmt.Errorf("invalid player one move %q", playerOne)
	}
	if !IsValidMove(playerTwo) {
		return "", fmt.Errorf("invalid player two move %q", playerTwo)
	}

	if playerOne == playerTwo {
		return Draw, nil
	}

	if (playerOne == Rock && playerTwo == Scissors) ||
		(playerOne == Paper && playerTwo == Rock) ||
		(playerOne == Scissors && playerTwo == Paper) {
		return PlayerOneWins, nil
	}

	return PlayerTwoWins, nil
}
