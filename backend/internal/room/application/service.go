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
	"time"

	"example.com/rock-paper-money/internal/room/domain"
)

var (
	ErrDuplicateRoom                   = errors.New("room code already exists")
	ErrRoomNotFound                    = errors.New("room code was not found")
	ErrUnauthorized                    = errors.New("invalid room credential")
	ErrAccountSeated                   = errors.New("account already occupies a seat")
	ErrInsufficientFunds               = errors.New("insufficient coin balance")
	ErrInvalidCoinAmount               = errors.New("coin amount must be a positive integer")
	ErrIdempotencyRequired             = errors.New("idempotency key is required")
	ErrIdempotencyConflict             = errors.New("idempotency key was already used with different data")
	errorsUnsupportedCommandRepository = errors.New("repository does not support idempotent commands")
)

const (
	RoundStake   int64 = 50
	WinnerPayout int64 = 75
	HousePayout  int64 = 25
)

type Snapshot struct {
	State    domain.State
	Revision uint64
	Presence map[string]bool
}
type CredentialDigest [sha256.Size]byte
type Mutation func(*domain.Room, string) error

// Repository is an application-owned port for atomic room operations.
type Repository interface {
	Create(context.Context, *domain.Room, CredentialDigest, string) (Snapshot, error)
	JoinFunded(context.Context, string, string, CredentialDigest, string, PresenceWindow) (Snapshot, []PresenceLease, error)
	SubmitMoveAndSettle(context.Context, string, CredentialDigest, string, domain.Move) (Snapshot, error)
	RequestNextRoundAndFund(context.Context, string, CredentialDigest, string, uint64) (Snapshot, error)
	Mutate(context.Context, string, CredentialDigest, string, Mutation) (Snapshot, error)
	Snapshot(context.Context, string) (Snapshot, error)
	Authenticate(context.Context, string, CredentialDigest, string) (Snapshot, string, error)
	RefreshPresence(context.Context, string, CredentialDigest, string, PresenceWindow) ([]PresenceLease, error)
	ForfeitExpired(context.Context, PresenceLease, time.Time) (Snapshot, bool, error)
	Balance(context.Context, string) (int64, error)
	Recharge(context.Context, string, int64, string) (int64, error)
	Analytics(context.Context) (Analytics, error)
}

type PlayedRound struct {
	RoomCode      string
	Round         uint64
	Result        domain.Result
	WinnerRole    string
	Forfeit       bool
	HouseEarnings int64
	ResolvedAt    time.Time
}

type Analytics struct {
	TotalHouseEarnings int64
	Rounds             []PlayedRound
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
	generateCode      Generator
	generateToken     Generator
	generatePlayerID  Generator
	now               func() time.Time
	gracePeriod       time.Duration
	heartbeatInterval time.Duration
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
	if config.GracePeriod == 0 {
		config.GracePeriod = DefaultPresenceGracePeriod
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = PresenceHeartbeatInterval
	}
	return &Service{repository: repository, generateCode: code, generateToken: token, generatePlayerID: playerID, now: config.Now, gracePeriod: config.GracePeriod, heartbeatInterval: config.HeartbeatInterval}
}

func (s *Service) Create(ctx context.Context, authUserID string) (Credentials, error) {
	if authUserID == "" {
		return Credentials{}, ErrUnauthorized
	}
	balance, err := s.repository.Balance(ctx, authUserID)
	if err != nil {
		return Credentials{}, err
	}
	if balance < RoundStake {
		return Credentials{}, ErrInsufficientFunds
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
		_, _, err = s.repository.JoinFunded(ctx, code, id, DigestToken(token), authUserID, s.presenceWindow(now))
		if errors.Is(err, domain.ErrDuplicatePlayer) {
			continue
		}
		if err != nil {
			return Credentials{}, err
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
	_, err := s.repository.SubmitMoveAndSettle(ctx, code, DigestToken(token), authUserID, move)
	return err
}
func (s *Service) RequestNextRound(ctx context.Context, code, token, authUserID string, round uint64) error {
	_, err := s.repository.RequestNextRoundAndFund(ctx, code, DigestToken(token), authUserID, round)
	return err
}
func (s *Service) Leave(ctx context.Context, code, token, authUserID string) error {
	_, err := s.repository.Mutate(ctx, code, DigestToken(token), authUserID, func(r *domain.Room, id string) error { return r.Leave(id) })
	return err
}

func (s *Service) Balance(ctx context.Context, authUserID string) (int64, error) {
	if authUserID == "" {
		return 0, ErrUnauthorized
	}
	return s.repository.Balance(ctx, authUserID)
}

func (s *Service) Recharge(ctx context.Context, authUserID string, amount int64, idempotencyKey string) (int64, error) {
	if authUserID == "" {
		return 0, ErrUnauthorized
	}
	if amount <= 0 {
		return 0, ErrInvalidCoinAmount
	}
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return 0, ErrIdempotencyRequired
	}
	if repository, ok := s.repository.(CommandRepository); ok {
		command, err := NewCommand(idempotencyKey, "wallet.recharge", struct {
			Amount int64 `json:"amount"`
		}{amount})
		if err != nil {
			return 0, err
		}
		result, err := repository.RechargeCommand(ctx, authUserID, amount, command)
		return result.Balance, err
	}
	return s.repository.Recharge(ctx, authUserID, amount, idempotencyKey)
}

func (s *Service) Analytics(ctx context.Context, authUserID string) (Analytics, error) {
	if authUserID == "" {
		return Analytics{}, ErrUnauthorized
	}
	return s.repository.Analytics(ctx)
}

// RefreshPresence renews an authenticated participant and reschedules the
// authoritative leases for both seats so an expired opponent is reconsidered.
func (s *Service) RefreshPresence(ctx context.Context, code, token, authUserID string) error {
	now := s.now()
	_, err := s.repository.RefreshPresence(ctx, code, DigestToken(token), authUserID, s.presenceWindow(now))
	if err != nil {
		return err
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
