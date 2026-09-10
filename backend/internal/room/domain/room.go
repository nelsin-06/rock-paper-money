package domain

import (
	"errors"
	"fmt"
)

var (
	ErrEmptyCode          = errors.New("room code is required")
	ErrMissingPlayerID    = errors.New("player ID is required")
	ErrDuplicatePlayer    = errors.New("player already joined")
	ErrRoomFull           = errors.New("room already has two players")
	ErrUnknownPlayer      = errors.New("player is not in the room")
	ErrRoomNotReady       = errors.New("room needs a second player")
	ErrDuplicateMove      = errors.New("player already submitted a move")
	ErrInvalidMove        = errors.New("move is invalid")
	ErrRoundNotResolved   = errors.New("round is not resolved")
	ErrNextRoundRequested = errors.New("player already requested another round")
	ErrStaleRound         = errors.New("round request is stale")
	ErrRoomClosed         = errors.New("room is closed")
	ErrInvalidState       = errors.New("invalid persisted room state")
)

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
	return move == Rock || move == Paper || move == Scissors
}

func DetermineResult(one, two Move) (Result, error) {
	if !IsValidMove(one) || !IsValidMove(two) {
		return "", ErrInvalidMove
	}
	if one == two {
		return Draw, nil
	}
	if (one == Rock && two == Scissors) || (one == Paper && two == Rock) || (one == Scissors && two == Paper) {
		return PlayerOneWins, nil
	}
	return PlayerTwoWins, nil
}

type Player struct {
	ID             string
	Wins           int
	Submitted      bool
	WantsNextRound bool
}

type PlayerMove struct {
	PlayerID string
	Move     Move
}

type State struct {
	Code     string
	Players  []Player
	Ready    bool
	Resolved bool
	Round    uint64
	Closed   bool
	Moves    []PlayerMove
	Result   Result
}

// PersistenceState contains the complete aggregate state without credentials.
type PersistenceState struct {
	Code              string
	Players           []PersistedPlayer
	Round             uint64
	Closed            bool
	Resolved          bool
	Result            Result
	Moves             map[string]Move
	NextRoundRequests map[string]bool
}

type PersistedPlayer struct {
	ID   string
	Wins int
}

type Room struct {
	code              string
	players           []PersistedPlayer
	moves             map[string]Move
	resolved          bool
	result            Result
	round             uint64
	nextRoundRequests map[string]bool
	closed            bool
}

func New(code, hostPlayerID string) (*Room, error) {
	if code == "" {
		return nil, ErrEmptyCode
	}
	if hostPlayerID == "" {
		return nil, ErrMissingPlayerID
	}
	return &Room{code: code, players: []PersistedPlayer{{ID: hostPlayerID}}, moves: map[string]Move{}, round: 1, nextRoundRequests: map[string]bool{}}, nil
}

// Restore validates persisted data before reconstructing an aggregate.
func Restore(state PersistenceState) (*Room, error) {
	if state.Code == "" || state.Round == 0 || len(state.Players) < 1 || len(state.Players) > 2 {
		return nil, ErrInvalidState
	}
	seen := map[string]bool{}
	for _, p := range state.Players {
		if p.ID == "" || p.Wins < 0 || seen[p.ID] {
			return nil, ErrInvalidState
		}
		seen[p.ID] = true
	}
	for id, move := range state.Moves {
		if !seen[id] || !IsValidMove(move) {
			return nil, ErrInvalidState
		}
	}
	for id := range state.NextRoundRequests {
		if !seen[id] {
			return nil, ErrInvalidState
		}
	}
	if state.Resolved {
		if len(state.Players) != 2 || len(state.Moves) != 2 || (state.Result != Draw && state.Result != PlayerOneWins && state.Result != PlayerTwoWins) {
			return nil, ErrInvalidState
		}
		result, _ := DetermineResult(state.Moves[state.Players[0].ID], state.Moves[state.Players[1].ID])
		if result != state.Result {
			return nil, ErrInvalidState
		}
	} else if state.Result != "" || len(state.NextRoundRequests) != 0 {
		return nil, ErrInvalidState
	}
	return &Room{code: state.Code, players: append([]PersistedPlayer(nil), state.Players...), moves: cloneMoves(state.Moves), resolved: state.Resolved, result: state.Result, round: state.Round, nextRoundRequests: cloneRequests(state.NextRoundRequests), closed: state.Closed}, nil
}

func (r *Room) Join(playerID string) error {
	if r.closed {
		return ErrRoomClosed
	}
	if playerID == "" {
		return ErrMissingPlayerID
	}
	if r.hasPlayer(playerID) {
		return ErrDuplicatePlayer
	}
	if len(r.players) == 2 {
		return ErrRoomFull
	}
	r.players = append(r.players, PersistedPlayer{ID: playerID})
	return nil
}

func (r *Room) SubmitMove(playerID string, move Move) error {
	if r.closed {
		return ErrRoomClosed
	}
	if !r.hasPlayer(playerID) {
		return ErrUnknownPlayer
	}
	if len(r.players) < 2 {
		return ErrRoomNotReady
	}
	if !IsValidMove(move) {
		return fmt.Errorf("%w: %q", ErrInvalidMove, move)
	}
	if _, ok := r.moves[playerID]; ok {
		return ErrDuplicateMove
	}
	r.moves[playerID] = move
	if len(r.moves) == 2 {
		r.resolve()
	}
	return nil
}

func (r *Room) RequestNextRound(playerID string, round uint64) error {
	if r.closed {
		return ErrRoomClosed
	}
	if !r.hasPlayer(playerID) {
		return ErrUnknownPlayer
	}
	if !r.resolved {
		return ErrRoundNotResolved
	}
	if round != r.round {
		return ErrStaleRound
	}
	if r.nextRoundRequests[playerID] {
		return ErrNextRoundRequested
	}
	r.nextRoundRequests[playerID] = true
	if len(r.nextRoundRequests) == len(r.players) {
		r.moves = map[string]Move{}
		r.resolved = false
		r.result = ""
		r.round++
		r.nextRoundRequests = map[string]bool{}
	}
	return nil
}

func (r *Room) Leave(playerID string) error {
	if r.closed {
		return ErrRoomClosed
	}
	if !r.hasPlayer(playerID) {
		return ErrUnknownPlayer
	}
	r.closed = true
	return nil
}

func (r *Room) State() State {
	players := make([]Player, len(r.players))
	for i, p := range r.players {
		_, submitted := r.moves[p.ID]
		players[i] = Player{ID: p.ID, Wins: p.Wins, Submitted: submitted, WantsNextRound: r.nextRoundRequests[p.ID]}
	}
	state := State{Code: r.code, Players: players, Ready: len(players) == 2, Resolved: r.resolved, Round: r.round, Closed: r.closed}
	if r.resolved {
		state.Result = r.result
		for _, p := range r.players {
			state.Moves = append(state.Moves, PlayerMove{PlayerID: p.ID, Move: r.moves[p.ID]})
		}
	}
	return state
}

func (r *Room) PersistenceState() PersistenceState {
	return PersistenceState{Code: r.code, Players: append([]PersistedPlayer(nil), r.players...), Round: r.round, Closed: r.closed, Resolved: r.resolved, Result: r.result, Moves: cloneMoves(r.moves), NextRoundRequests: cloneRequests(r.nextRoundRequests)}
}

func (r *Room) hasPlayer(id string) bool {
	for _, p := range r.players {
		if p.ID == id {
			return true
		}
	}
	return false
}
func (r *Room) resolve() {
	r.result, _ = DetermineResult(r.moves[r.players[0].ID], r.moves[r.players[1].ID])
	r.resolved = true
	if r.result == PlayerOneWins {
		r.players[0].Wins++
	} else if r.result == PlayerTwoWins {
		r.players[1].Wins++
	}
}
func cloneMoves(in map[string]Move) map[string]Move {
	out := make(map[string]Move, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func cloneRequests(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
