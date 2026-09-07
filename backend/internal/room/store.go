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
	rooms map[string]*storedRoom
}

func NewStore() *Store {
	return &Store{rooms: make(map[string]*storedRoom)}
}

type storedRoom struct {
	room        *Room
	revision    uint64
	subscribers map[chan struct{}]struct{}
}

// Snapshot is a detached room state paired with its monotonic revision.
type Snapshot struct {
	State    State
	Revision uint64
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

	s.rooms[code] = &storedRoom{
		room:        created,
		revision:    1,
		subscribers: make(map[chan struct{}]struct{}),
	}
	return created.State(), nil
}

func (s *Store) State(code string) (State, error) {
	snapshot, err := s.Snapshot(code)
	return snapshot.State, err
}

func (s *Store) Snapshot(code string) (Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	found, err := s.room(code)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{State: found.room.State(), Revision: found.revision}, nil
}

// Subscribe atomically registers for room changes and returns the current
// snapshot. Notifications are coalesced so slow subscribers never block room
// mutations or accumulate an unbounded queue.
func (s *Store) Subscribe(code string) (Snapshot, <-chan struct{}, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, err := s.room(code)
	if err != nil {
		return Snapshot{}, nil, nil, err
	}

	changes := make(chan struct{}, 1)
	found.subscribers[changes] = struct{}{}
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(found.subscribers, changes)
		})
	}

	return Snapshot{State: found.room.State(), Revision: found.revision}, changes, unsubscribe, nil
}

func (s *Store) Join(code, playerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, err := s.room(code)
	if err != nil {
		return err
	}
	if err := found.room.Join(playerID); err != nil {
		return err
	}
	s.notify(found)
	return nil
}

func (s *Store) SubmitMove(code, playerID string, move game.Move) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, err := s.room(code)
	if err != nil {
		return err
	}
	if err := found.room.SubmitMove(playerID, move); err != nil {
		return err
	}
	s.notify(found)
	return nil
}

func (s *Store) RequestNextRound(code, playerID string, round uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, err := s.room(code)
	if err != nil {
		return err
	}
	if err := found.room.RequestNextRound(playerID, round); err != nil {
		return err
	}
	s.notify(found)
	return nil
}

func (s *Store) Leave(code, playerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, err := s.room(code)
	if err != nil {
		return err
	}
	if err := found.room.Leave(playerID); err != nil {
		return err
	}
	s.notify(found)
	return nil
}

// room must only be called while s.mu is held.
func (s *Store) room(code string) (*storedRoom, error) {
	found, exists := s.rooms[code]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrRoomNotFound, code)
	}
	return found, nil
}

// notify must only be called while s.mu is held and after a successful mutation.
func (s *Store) notify(found *storedRoom) {
	found.revision++
	for subscriber := range found.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}
