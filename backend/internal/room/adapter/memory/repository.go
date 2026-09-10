package memory

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sync"

	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

type Repository struct {
	mu     sync.RWMutex
	rooms  map[string]*storedRoom
	events *Events
}
type storedRoom struct {
	room        *domain.Room
	revision    uint64
	credentials map[string]application.CredentialDigest
}

func New() (*Repository, *Events) {
	events := NewEvents()
	return &Repository{rooms: map[string]*storedRoom{}, events: events}, events
}

func (r *Repository) Create(_ context.Context, aggregate *domain.Room, digest application.CredentialDigest) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := aggregate.State()
	if _, exists := r.rooms[state.Code]; exists {
		return application.Snapshot{}, fmt.Errorf("%w: %q", application.ErrDuplicateRoom, state.Code)
	}
	storedAggregate, err := domain.Restore(aggregate.PersistenceState())
	if err != nil {
		return application.Snapshot{}, err
	}
	r.rooms[state.Code] = &storedRoom{room: storedAggregate, revision: 1, credentials: map[string]application.CredentialDigest{state.Players[0].ID: digest}}
	return application.Snapshot{State: state, Revision: 1}, nil
}

func (r *Repository) Join(_ context.Context, code, playerID string, digest application.CredentialDigest) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, err
	}
	for _, existing := range stored.credentials {
		if equalDigest(existing, digest) {
			return application.Snapshot{}, domain.ErrDuplicatePlayer
		}
	}
	if err := stored.room.Join(playerID); err != nil {
		return application.Snapshot{}, err
	}
	stored.credentials[playerID] = digest
	return r.changed(code, stored), nil
}

func (r *Repository) Mutate(_ context.Context, code string, digest application.CredentialDigest, mutation application.Mutation) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, err
	}
	playerID, ok := authenticate(stored.credentials, digest)
	if !ok {
		return application.Snapshot{}, application.ErrUnauthorized
	}
	working, err := domain.Restore(stored.room.PersistenceState())
	if err != nil {
		return application.Snapshot{}, err
	}
	if err := mutation(working, playerID); err != nil {
		return application.Snapshot{}, err
	}
	stored.room = working
	return r.changed(code, stored), nil
}

func (r *Repository) Snapshot(_ context.Context, code string) (application.Snapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, err
	}
	return application.Snapshot{State: stored.room.State(), Revision: stored.revision}, nil
}

func (r *Repository) Authenticate(_ context.Context, code string, digest application.CredentialDigest) (application.Snapshot, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, "", err
	}
	id, ok := authenticate(stored.credentials, digest)
	if !ok {
		return application.Snapshot{}, "", application.ErrUnauthorized
	}
	state := stored.room.State()
	role := "guest"
	if state.Players[0].ID == id {
		role = "host"
	}
	return application.Snapshot{State: state, Revision: stored.revision}, role, nil
}

func (r *Repository) room(code string) (*storedRoom, error) {
	stored, ok := r.rooms[code]
	if !ok {
		return nil, fmt.Errorf("%w: %q", application.ErrRoomNotFound, code)
	}
	return stored, nil
}
func (r *Repository) changed(code string, stored *storedRoom) application.Snapshot {
	stored.revision++
	r.events.Publish(code)
	return application.Snapshot{State: stored.room.State(), Revision: stored.revision}
}
func authenticate(credentials map[string]application.CredentialDigest, digest application.CredentialDigest) (string, bool) {
	for id, stored := range credentials {
		if equalDigest(stored, digest) {
			return id, true
		}
	}
	return "", false
}
func equalDigest(a, b application.CredentialDigest) bool {
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

type Events struct {
	mu    sync.Mutex
	rooms map[string]map[chan struct{}]struct{}
}

func NewEvents() *Events { return &Events{rooms: map[string]map[chan struct{}]struct{}{}} }
func (e *Events) Subscribe(code string) (<-chan struct{}, func()) {
	e.mu.Lock()
	ch := make(chan struct{}, 1)
	if e.rooms[code] == nil {
		e.rooms[code] = map[chan struct{}]struct{}{}
	}
	e.rooms[code][ch] = struct{}{}
	e.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { e.mu.Lock(); delete(e.rooms[code], ch); e.mu.Unlock() }) }
}
func (e *Events) Publish(code string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.rooms[code] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
