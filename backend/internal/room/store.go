package room

import (
	"errors"
	"fmt"
	"sync"

	"example.com/rock-paper-money/internal/game"
)

var (
	ErrDuplicateRoom = errors.New("room code already exists")
	ErrRoomNotFound  = errors.New("room code was not found")
)

// Store owns rooms and synchronizes every access to them.
type Store struct {
	mu    sync.RWMutex
	rooms map[string]*Room
}

func NewStore() *Store {
	return &Store{rooms: make(map[string]*Room)}
}

func (s *Store) Create(code, hostPlayerID string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.rooms[code]; exists {
		return State{}, fmt.Errorf("%w: %q", ErrDuplicateRoom, code)
	}

	created, err := New(code, hostPlayerID)
	if err != nil {
		return State{}, err
	}

	s.rooms[code] = created
	return created.State(), nil
}

func (s *Store) State(code string) (State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	found, err := s.room(code)
	if err != nil {
		return State{}, err
	}
	return found.State(), nil
}

func (s *Store) Join(code, playerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, err := s.room(code)
	if err != nil {
		return err
	}
	return found.Join(playerID)
}

func (s *Store) SubmitMove(code, playerID string, move game.Move) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, err := s.room(code)
	if err != nil {
		return err
	}
	return found.SubmitMove(playerID, move)
}

func (s *Store) StartNextRound(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, err := s.room(code)
	if err != nil {
		return err
	}
	return found.StartNextRound()
}

// room must only be called while s.mu is held.
func (s *Store) room(code string) (*Room, error) {
	found, exists := s.rooms[code]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrRoomNotFound, code)
	}
	return found, nil
}
