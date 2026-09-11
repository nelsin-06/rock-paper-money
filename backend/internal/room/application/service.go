package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"example.com/rock-paper-money/internal/room/domain"
)

var (
	ErrDuplicateRoom = errors.New("room code already exists")
	ErrRoomNotFound  = errors.New("room code was not found")
	ErrUnauthorized  = errors.New("invalid room credential")
	ErrAccountSeated = errors.New("account already occupies a seat")
)

type Snapshot struct {
	State    domain.State
	Revision uint64
}
type CredentialDigest [sha256.Size]byte
type Mutation func(*domain.Room, string) error

// Repository is an application-owned port for atomic room operations.
type Repository interface {
	Create(context.Context, *domain.Room, CredentialDigest, string) (Snapshot, error)
	Join(context.Context, string, string, CredentialDigest, string, PresenceWindow) (Snapshot, []PresenceLease, error)
	Mutate(context.Context, string, CredentialDigest, string, Mutation) (Snapshot, error)
	Snapshot(context.Context, string) (Snapshot, error)
	Authenticate(context.Context, string, CredentialDigest, string) (Snapshot, string, error)
	RefreshPresence(context.Context, string, CredentialDigest, string, PresenceWindow) ([]PresenceLease, error)
	ForfeitExpired(context.Context, PresenceLease, time.Time) (Snapshot, bool, error)
}

type Events interface {
	Subscribe(string) (<-chan struct{}, func())
}
type Generator func() (string, error)
type Credentials struct {
	RoomCode    string
	PlayerToken string
}

type PresenceLease struct {
	RoomCode   string
	PlayerID   string
	Round      uint64
	Generation uint64
	Deadline   time.Time
	EvaluateAt time.Time
	ProofAfter time.Time
	Active     bool
}

type PresenceWindow struct {
	ObservedAt time.Time
	ProofAfter time.Time
	Deadline   time.Time
	EvaluateAt time.Time
}

type Timer interface{ Stop() bool }
type ScheduleFunc func(time.Duration, func()) Timer

type Service struct {
	repository        Repository
	events            Events
	generateCode      Generator
	generateToken     Generator
	generatePlayerID  Generator
	now               func() time.Time
	schedule          ScheduleFunc
	gracePeriod       time.Duration
	heartbeatInterval time.Duration
	retryDelay        time.Duration
	maxRetries        int
	reportError       func(error)
	presenceMu        sync.Mutex
	presenceTimers    map[string]scheduledPresence
	presenceVersions  map[string]presenceVersion
}

type scheduledPresence struct {
	version presenceVersion
	timer   Timer
}

type presenceVersion struct {
	round      uint64
	generation uint64
}

const (
	DefaultPresenceGracePeriod = 10 * time.Second
	PresenceHeartbeatInterval  = 3 * time.Second
	defaultPresenceRetryDelay  = time.Second
	defaultPresenceMaxRetries  = 2
	presenceAttemptTimeout     = 5 * time.Second
)

type PresenceConfig struct {
	Now               func() time.Time
	Schedule          ScheduleFunc
	GracePeriod       time.Duration
	HeartbeatInterval time.Duration
	RetryDelay        time.Duration
	MaxRetries        int
	ReportError       func(error)
}

func NewService(repository Repository, events Events) *Service {
	return NewServiceWithGenerators(repository, events, randomRoomCode, randomToken, randomPlayerID)
}

func NewServiceWithGenerators(repository Repository, events Events, code, token, playerID Generator) *Service {
	return NewServiceWithPresenceConfig(repository, events, code, token, playerID, PresenceConfig{})
}

func NewServiceWithTiming(repository Repository, events Events, code, token, playerID Generator, now func() time.Time, schedule ScheduleFunc, gracePeriod time.Duration) *Service {
	return NewServiceWithPresenceConfig(repository, events, code, token, playerID, PresenceConfig{Now: now, Schedule: schedule, GracePeriod: gracePeriod})
}

func NewServiceWithPresenceConfig(repository Repository, events Events, code, token, playerID Generator, config PresenceConfig) *Service {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Schedule == nil {
		config.Schedule = func(delay time.Duration, run func()) Timer { return time.AfterFunc(delay, run) }
	}
	if config.GracePeriod == 0 {
		config.GracePeriod = DefaultPresenceGracePeriod
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = PresenceHeartbeatInterval
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = defaultPresenceRetryDelay
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = defaultPresenceMaxRetries
	}
	if config.ReportError == nil {
		config.ReportError = func(err error) { slog.Error("room presence expiry failed", "error", err) }
	}
	return &Service{repository: repository, events: events, generateCode: code, generateToken: token, generatePlayerID: playerID, now: config.Now, schedule: config.Schedule, gracePeriod: config.GracePeriod, heartbeatInterval: config.HeartbeatInterval, retryDelay: config.RetryDelay, maxRetries: config.MaxRetries, reportError: config.ReportError, presenceTimers: map[string]scheduledPresence{}, presenceVersions: map[string]presenceVersion{}}
}

func (s *Service) Create(ctx context.Context, authUserID string) (Credentials, error) {
	if authUserID == "" {
		return Credentials{}, ErrUnauthorized
	}
	for range 8 {
		code, err := s.generateCode()
		if err != nil {
			return Credentials{}, err
		}
		token, id, err := s.identity()
		if err != nil {
			return Credentials{}, err
		}
		r, err := domain.New(code, id)
		if err != nil {
			return Credentials{}, err
		}
		if _, err = s.repository.Create(ctx, r, DigestToken(token), authUserID); err != nil {
			if errors.Is(err, ErrDuplicateRoom) {
				continue
			}
			return Credentials{}, err
		}
		return Credentials{RoomCode: code, PlayerToken: token}, nil
	}
	return Credentials{}, ErrDuplicateRoom
}

func (s *Service) Join(ctx context.Context, code, authUserID string) (Credentials, error) {
	if authUserID == "" {
		return Credentials{}, ErrUnauthorized
	}
	for range 8 {
		token, id, err := s.identity()
		if err != nil {
			return Credentials{}, err
		}
		now := s.now()
		_, leases, err := s.repository.Join(ctx, code, id, DigestToken(token), authUserID, s.presenceWindow(now))
		if errors.Is(err, domain.ErrDuplicatePlayer) {
			continue
		}
		if err != nil {
			return Credentials{}, err
		}
		for _, lease := range leases {
			s.installPresence(lease)
		}
		return Credentials{RoomCode: code, PlayerToken: token}, nil
	}
	return Credentials{}, domain.ErrDuplicatePlayer
}

func (s *Service) Snapshot(ctx context.Context, code string) (Snapshot, error) {
	return s.repository.Snapshot(ctx, code)
}
func (s *Service) Authenticate(ctx context.Context, code, token, authUserID string) (Snapshot, string, error) {
	return s.repository.Authenticate(ctx, code, DigestToken(token), authUserID)
}
func (s *Service) SubmitMove(ctx context.Context, code, token, authUserID string, move domain.Move) error {
	_, err := s.repository.Mutate(ctx, code, DigestToken(token), authUserID, func(r *domain.Room, id string) error { return r.SubmitMove(id, move) })
	return err
}
func (s *Service) RequestNextRound(ctx context.Context, code, token, authUserID string, round uint64) error {
	_, err := s.repository.Mutate(ctx, code, DigestToken(token), authUserID, func(r *domain.Room, id string) error { return r.RequestNextRound(id, round) })
	return err
}
func (s *Service) Leave(ctx context.Context, code, token, authUserID string) error {
	_, err := s.repository.Mutate(ctx, code, DigestToken(token), authUserID, func(r *domain.Room, id string) error { return r.Leave(id) })
	return err
}

// RefreshPresence renews an authenticated participant and reschedules the
// authoritative leases for both seats so an expired opponent is reconsidered.
func (s *Service) RefreshPresence(ctx context.Context, code, token, authUserID string) error {
	now := s.now()
	leases, err := s.repository.RefreshPresence(ctx, code, DigestToken(token), authUserID, s.presenceWindow(now))
	if err != nil {
		return err
	}
	for _, lease := range leases {
		s.installPresence(lease)
	}
	return nil
}

func (s *Service) presenceDeadline(now time.Time) time.Time {
	return now.Add(s.heartbeatInterval + s.gracePeriod)
}

func (s *Service) presenceWindow(now time.Time) PresenceWindow {
	deadline := s.presenceDeadline(now)
	return PresenceWindow{ObservedAt: now, ProofAfter: deadline, Deadline: deadline, EvaluateAt: deadline.Add(s.heartbeatInterval)}
}

func (s *Service) installPresence(lease PresenceLease) {
	key := lease.RoomCode + ":" + lease.PlayerID
	s.presenceMu.Lock()
	version := presenceVersion{round: lease.Round, generation: lease.Generation}
	if current, exists := s.presenceVersions[key]; exists && !version.after(current) {
		s.presenceMu.Unlock()
		return
	}
	s.presenceVersions[key] = version
	previous, exists := s.presenceTimers[key]
	if exists {
		previous.timer.Stop()
		delete(s.presenceTimers, key)
	}
	if lease.Active {
		s.schedulePresenceLocked(key, lease, 0, maxDuration(lease.EvaluateAt.Sub(s.now())))
	}
	s.presenceMu.Unlock()
}

func (s *Service) schedulePresenceLocked(key string, lease PresenceLease, attempt int, delay time.Duration) {
	timer := s.schedule(delay, func() { s.expirePresence(key, lease, attempt) })
	s.presenceTimers[key] = scheduledPresence{version: presenceVersion{round: lease.Round, generation: lease.Generation}, timer: timer}
}

func (s *Service) expirePresence(key string, lease PresenceLease, attempt int) {
	ctx, cancel := context.WithTimeout(context.Background(), presenceAttemptTimeout)
	_, _, err := s.repository.ForfeitExpired(ctx, lease, s.now())
	cancel()
	if err != nil {
		s.reportError(fmt.Errorf("resolve presence expiry for room %s: %w", lease.RoomCode, err))
	}
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	current, exists := s.presenceTimers[key]
	if !exists || current.version != (presenceVersion{round: lease.Round, generation: lease.Generation}) {
		return
	}
	if err != nil && attempt < s.maxRetries {
		s.schedulePresenceLocked(key, lease, attempt+1, s.retryDelay)
		return
	}
	delete(s.presenceTimers, key)
}

func (v presenceVersion) after(other presenceVersion) bool {
	return v.round > other.round || (v.round == other.round && v.generation > other.generation)
}

func maxDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

// Subscribe registers before loading state, so a concurrent change is never missed.
func (s *Service) Subscribe(ctx context.Context, code string) (Snapshot, <-chan struct{}, func(), error) {
	changes, unsubscribe := s.events.Subscribe(code)
	snapshot, err := s.repository.Snapshot(ctx, code)
	if err != nil {
		unsubscribe()
		return Snapshot{}, nil, nil, err
	}
	return snapshot, changes, unsubscribe, nil
}

func DigestToken(token string) CredentialDigest { return sha256.Sum256([]byte(token)) }
func (s *Service) identity() (string, string, error) {
	token, err := s.generateToken()
	if err != nil {
		return "", "", err
	}
	id, err := s.generatePlayerID()
	return token, id, err
}

const roomAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomRoomCode() (string, error) {
	b := make([]byte, 6)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("generate room code: %w", err)
	}
	for i, v := range b {
		b[i] = roomAlphabet[int(v)%len(roomAlphabet)]
	}
	return string(b), nil
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("generate player token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func randomPlayerID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("generate player identity: %w", err)
	}
	return hex.EncodeToString(b), nil
}
