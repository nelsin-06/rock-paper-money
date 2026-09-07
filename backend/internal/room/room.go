package room

import (
	"errors"
	"fmt"

	"example.com/rock-paper-money/internal/game"
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
)

type Player struct {
	ID             string
	Wins           int
	Submitted      bool
	WantsNextRound bool
}

type player struct {
	id   string
	wins int
}

type PlayerMove struct {
	PlayerID string
	Move     game.Move
}

// State is a detached snapshot. Changing it does not change the room.
type State struct {
	Code     string
	Players  []Player
	Ready    bool
	Resolved bool
	Round    uint64
	Closed   bool
	Moves    []PlayerMove
	Result   game.Result
}

type Room struct {
	code              string
	players           []player
	moves             map[string]game.Move
	resolved          bool
	result            game.Result
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

	return &Room{
		code:              code,
		players:           []player{{id: hostPlayerID}},
		moves:             make(map[string]game.Move, 2),
		round:             1,
		nextRoundRequests: make(map[string]bool, 2),
	}, nil
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

	r.players = append(r.players, player{id: playerID})
	return nil
}

func (r *Room) SubmitMove(playerID string, move game.Move) error {
	if r.closed {
		return ErrRoomClosed
	}
	if !r.hasPlayer(playerID) {
		return ErrUnknownPlayer
	}
	if len(r.players) < 2 {
		return ErrRoomNotReady
	}
	if !game.IsValidMove(move) {
		return fmt.Errorf("%w: %q", ErrInvalidMove, move)
	}
	if _, submitted := r.moves[playerID]; submitted {
		return ErrDuplicateMove
	}

	r.moves[playerID] = move
	if len(r.moves) == 2 {
		return r.resolve()
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
	if len(r.nextRoundRequests) < len(r.players) {
		return nil
	}

	r.moves = make(map[string]game.Move, 2)
	r.resolved = false
	r.result = ""
	r.round++
	r.nextRoundRequests = make(map[string]bool, 2)
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
	for i, player := range r.players {
		_, submitted := r.moves[player.id]
		players[i] = Player{
			ID:             player.id,
			Wins:           player.wins,
			Submitted:      submitted,
			WantsNextRound: r.nextRoundRequests[player.id],
		}
	}

	state := State{
		Code:     r.code,
		Players:  players,
		Ready:    len(r.players) == 2,
		Resolved: r.resolved,
		Round:    r.round,
		Closed:   r.closed,
	}

	if !r.resolved {
		return state
	}

	state.Result = r.result
	state.Moves = make([]PlayerMove, 0, len(r.players))
	for _, player := range r.players {
		state.Moves = append(state.Moves, PlayerMove{
			PlayerID: player.id,
			Move:     r.moves[player.id],
		})
	}

	return state
}

func (r *Room) hasPlayer(playerID string) bool {
	for _, player := range r.players {
		if player.id == playerID {
			return true
		}
	}
	return false
}

func (r *Room) resolve() error {
	result, err := game.DetermineResult(
		r.moves[r.players[0].id],
		r.moves[r.players[1].id],
	)
	if err != nil {
		return err
	}

	r.result = result
	r.resolved = true

	switch result {
	case game.PlayerOneWins:
		r.players[0].wins++
	case game.PlayerTwoWins:
		r.players[1].wins++
	}

	return nil
}
