package memory

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

type Repository struct {
	mu        sync.RWMutex
	rooms     map[string]*storedRoom
	events    *Events
	wallets   map[string]int64
	recharges map[string]memoryRecharge
	history   []application.PlayedRound
}
type storedRoom struct {
	room        *domain.Room
	revision    uint64
	credentials map[string]application.CredentialDigest
	owners      map[string]string
	presence    map[string]memoryPresence
	funded      map[uint64]bool
}

type memoryRecharge struct {
	owner  string
	amount int64
}

type memoryPresence struct {
	generation  uint64
	refreshedAt time.Time
	deadline    time.Time
}

func New() (*Repository, *Events) {
	events := NewEvents()
	return &Repository{rooms: map[string]*storedRoom{}, events: events, wallets: map[string]int64{"house": 0}, recharges: map[string]memoryRecharge{}}, events
}

func (r *Repository) Create(_ context.Context, aggregate *domain.Room, digest application.CredentialDigest, authUserID string) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := aggregate.State()
	r.ensureWallet(authUserID)
	if _, exists := r.rooms[state.Code]; exists {
		return application.Snapshot{}, fmt.Errorf("%w: %q", application.ErrDuplicateRoom, state.Code)
	}
	storedAggregate, err := domain.Restore(aggregate.PersistenceState())
	if err != nil {
		return application.Snapshot{}, err
	}
	r.rooms[state.Code] = &storedRoom{room: storedAggregate, revision: 1, credentials: map[string]application.CredentialDigest{state.Players[0].ID: digest}, owners: map[string]string{state.Players[0].ID: authUserID}, presence: map[string]memoryPresence{}, funded: map[uint64]bool{}}
	return application.Snapshot{State: state, Revision: 1}, nil
}

func (r *Repository) JoinFunded(_ context.Context, code, playerID string, digest application.CredentialDigest, authUserID string, window application.PresenceWindow) (application.Snapshot, []application.PresenceLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, nil, err
	}
	for _, existing := range stored.credentials {
		if equalDigest(existing, digest) {
			return application.Snapshot{}, nil, domain.ErrDuplicatePlayer
		}
	}
	for _, owner := range stored.owners {
		if owner == authUserID {
			return application.Snapshot{}, nil, application.ErrAccountSeated
		}
	}
	working, err := domain.Restore(stored.room.PersistenceState())
	if err != nil {
		return application.Snapshot{}, nil, err
	}
	if err := working.Join(playerID); err != nil {
		return application.Snapshot{}, nil, err
	}
	r.ensureWallet(authUserID)
	if err := r.fundRound(stored, code, working.State().Round, authUserID); err != nil {
		return application.Snapshot{}, nil, err
	}
	stored.room = working
	stored.credentials[playerID] = digest
	stored.owners[playerID] = authUserID
	state := stored.room.State()
	leases := make([]application.PresenceLease, 0, len(state.Players))
	for _, player := range state.Players {
		generation := stored.presence[player.ID].generation + 1
		stored.presence[player.ID] = memoryPresence{generation: generation, refreshedAt: window.ObservedAt, deadline: window.Deadline}
		leases = append(leases, application.PresenceLease{RoomCode: code, PlayerID: player.ID, Round: state.Round, Generation: generation, Deadline: window.Deadline, EvaluateAt: window.EvaluateAt, ProofAfter: window.ProofAfter, Active: true})
	}
	return r.changed(code, stored), leases, nil
}

func (r *Repository) SubmitMoveAndSettle(_ context.Context, code string, digest application.CredentialDigest, authUserID string, move domain.Move) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, err
	}
	playerID, ok := authenticate(stored.credentials, stored.owners, digest, authUserID)
	if !ok {
		return application.Snapshot{}, application.ErrUnauthorized
	}
	working, err := domain.Restore(stored.room.PersistenceState())
	if err != nil {
		return application.Snapshot{}, err
	}
	wasResolved := working.State().Resolved
	if err = working.SubmitMove(playerID, move); err != nil {
		return application.Snapshot{}, err
	}
	if !stored.funded[working.State().Round] {
		if err = r.fundRound(stored, code, working.State().Round, ""); err != nil {
			return application.Snapshot{}, err
		}
	}
	if !wasResolved && working.State().Resolved {
		r.settleRound(stored, code, working.State())
	}
	stored.room = working
	return r.changed(code, stored), nil
}

func (r *Repository) RequestNextRoundAndFund(_ context.Context, code string, digest application.CredentialDigest, authUserID string, round uint64) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, err
	}
	playerID, ok := authenticate(stored.credentials, stored.owners, digest, authUserID)
	if !ok {
		return application.Snapshot{}, application.ErrUnauthorized
	}
	working, err := domain.Restore(stored.room.PersistenceState())
	if err != nil {
		return application.Snapshot{}, err
	}
	previousRound := working.State().Round
	if err = working.RequestNextRound(playerID, round); err != nil {
		return application.Snapshot{}, err
	}
	if working.State().Round > previousRound {
		if err = r.fundRound(stored, code, working.State().Round, ""); err != nil {
			return application.Snapshot{}, err
		}
	}
	stored.room = working
	return r.changed(code, stored), nil
}

func (r *Repository) Mutate(_ context.Context, code string, digest application.CredentialDigest, authUserID string, mutation application.Mutation) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, err
	}
	playerID, ok := authenticate(stored.credentials, stored.owners, digest, authUserID)
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

func (r *Repository) Authenticate(_ context.Context, code string, digest application.CredentialDigest, authUserID string) (application.Snapshot, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	stored, err := r.room(code)
	if err != nil {
		return application.Snapshot{}, "", err
	}
	id, ok := authenticate(stored.credentials, stored.owners, digest, authUserID)
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

func (r *Repository) RefreshPresence(_ context.Context, code string, digest application.CredentialDigest, authUserID string, window application.PresenceWindow) ([]application.PresenceLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.room(code)
	if err != nil {
		return nil, err
	}
	playerID, ok := authenticate(stored.credentials, stored.owners, digest, authUserID)
	if !ok {
		return nil, application.ErrUnauthorized
	}
	state := stored.room.State()
	presence := stored.presence[playerID]
	presence.refreshedAt = window.ObservedAt
	presence.deadline = window.Deadline
	stored.presence[playerID] = presence
	active := !state.Closed && !state.Resolved && state.Ready
	evaluationDelay := window.EvaluateAt.Sub(window.Deadline)
	leases := make([]application.PresenceLease, 0, len(state.Players))
	for _, player := range state.Players {
		participant, exists := stored.presence[player.ID]
		if !exists {
			continue
		}
		participant.generation++
		stored.presence[player.ID] = participant
		leases = append(leases, application.PresenceLease{RoomCode: code, PlayerID: player.ID, Round: state.Round, Generation: participant.generation, Deadline: participant.deadline, EvaluateAt: participant.deadline.Add(evaluationDelay), ProofAfter: participant.deadline, Active: active})
	}
	return leases, nil
}

func (r *Repository) ForfeitExpired(_ context.Context, lease application.PresenceLease, now time.Time) (application.Snapshot, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, err := r.room(lease.RoomCode)
	if err != nil {
		return application.Snapshot{}, false, err
	}
	presence, ok := stored.presence[lease.PlayerID]
	if !ok || presence.generation != lease.Generation || now.Before(presence.deadline) {
		return application.Snapshot{State: stored.room.State(), Revision: stored.revision}, false, nil
	}
	state := stored.room.State()
	remainingConnected := false
	for _, player := range state.Players {
		if player.ID == lease.PlayerID {
			continue
		}
		remaining, present := stored.presence[player.ID]
		remainingConnected = present && now.Before(remaining.deadline) && remaining.refreshedAt.After(lease.ProofAfter)
	}
	if !remainingConnected {
		return application.Snapshot{State: state, Revision: stored.revision}, false, nil
	}
	working, err := domain.Restore(stored.room.PersistenceState())
	if err != nil {
		return application.Snapshot{}, false, err
	}
	if err = working.Forfeit(lease.PlayerID, lease.Round); err != nil {
		if errors.Is(err, domain.ErrStaleRound) || errors.Is(err, domain.ErrRoundResolved) || errors.Is(err, domain.ErrRoomClosed) {
			return application.Snapshot{State: stored.room.State(), Revision: stored.revision}, false, nil
		}
		return application.Snapshot{}, false, err
	}
	if !stored.funded[working.State().Round] {
		if err = r.fundRound(stored, lease.RoomCode, working.State().Round, ""); err != nil {
			return application.Snapshot{}, false, err
		}
	}
	r.settleRound(stored, lease.RoomCode, working.State())
	stored.room = working
	return r.changed(lease.RoomCode, stored), true, nil
}

func (r *Repository) Balance(_ context.Context, authUserID string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureWallet(authUserID)
	return r.wallets[authUserID], nil
}

func (r *Repository) Recharge(_ context.Context, authUserID string, amount int64, key string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	businessKey := authUserID + ":" + key
	if previous, ok := r.recharges[businessKey]; ok {
		if previous.owner != authUserID || previous.amount != amount {
			return 0, application.ErrIdempotencyConflict
		}
		return r.wallets[authUserID], nil
	}
	r.ensureWallet(authUserID)
	if amount > math.MaxInt64-r.wallets[authUserID] {
		return 0, application.ErrInvalidCoinAmount
	}
	r.wallets[authUserID] += amount
	r.recharges[businessKey] = memoryRecharge{owner: authUserID, amount: amount}
	return r.wallets[authUserID], nil
}

func (r *Repository) Analytics(_ context.Context) (application.Analytics, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rounds := append([]application.PlayedRound(nil), r.history...)
	return application.Analytics{TotalHouseEarnings: r.wallets["house"], Rounds: rounds}, nil
}

func (r *Repository) ensureWallet(authUserID string) {
	if _, ok := r.wallets[authUserID]; !ok {
		r.wallets[authUserID] = 0
	}
}

func (r *Repository) fundRound(stored *storedRoom, code string, round uint64, joiningOwner string) error {
	if stored.funded[round] {
		return nil
	}
	owners := make([]string, 0, 2)
	for _, player := range stored.room.State().Players {
		owners = append(owners, stored.owners[player.ID])
	}
	if joiningOwner != "" {
		owners = append(owners, joiningOwner)
	}
	if len(owners) != 2 {
		return domain.ErrRoomNotReady
	}
	for _, owner := range owners {
		r.ensureWallet(owner)
		if r.wallets[owner] < application.RoundStake {
			return application.ErrInsufficientFunds
		}
	}
	for _, owner := range owners {
		r.wallets[owner] -= application.RoundStake
	}
	r.wallets[escrowKey(code, round)] = application.RoundStake * 2
	stored.funded[round] = true
	return nil
}

func (r *Repository) settleRound(stored *storedRoom, code string, state domain.State) {
	escrow := escrowKey(code, state.Round)
	if r.wallets[escrow] == 0 {
		return
	}
	playerOwners := []string{stored.owners[state.Players[0].ID], stored.owners[state.Players[1].ID]}
	houseEarnings := int64(0)
	winnerRole := ""
	if state.Result == domain.Draw {
		r.wallets[playerOwners[0]] += application.RoundStake
		r.wallets[playerOwners[1]] += application.RoundStake
	} else {
		winner := 0
		winnerRole = "host"
		if state.Result == domain.PlayerTwoWins {
			winner = 1
			winnerRole = "guest"
		}
		r.wallets[playerOwners[winner]] += application.WinnerPayout
		r.wallets["house"] += application.HousePayout
		houseEarnings = application.HousePayout
	}
	r.wallets[escrow] = 0
	r.history = append([]application.PlayedRound{{RoomCode: code, Round: state.Round, Result: state.Result, WinnerRole: winnerRole, Forfeit: state.Forfeit, HouseEarnings: houseEarnings, ResolvedAt: time.Now().UTC()}}, r.history...)
}

func escrowKey(code string, round uint64) string { return fmt.Sprintf("escrow:%s:%d", code, round) }

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
func authenticate(credentials map[string]application.CredentialDigest, owners map[string]string, digest application.CredentialDigest, authUserID string) (string, bool) {
	for id, stored := range credentials {
		if equalDigest(stored, digest) && owners[id] == authUserID && authUserID != "" {
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
