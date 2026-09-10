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

	"example.com/rock-paper-money/internal/room/domain"
)

var (
	ErrDuplicateRoom = errors.New("room code already exists")
	ErrRoomNotFound  = errors.New("room code was not found")
	ErrUnauthorized  = errors.New("invalid room credential")
)

type Snapshot struct {
	State    domain.State
	Revision uint64
}
type CredentialDigest [sha256.Size]byte
type Mutation func(*domain.Room, string) error

// Repository is an application-owned port for atomic room operations.
type Repository interface {
	Create(context.Context, *domain.Room, CredentialDigest) (Snapshot, error)
	Join(context.Context, string, string, CredentialDigest) (Snapshot, error)
	Mutate(context.Context, string, CredentialDigest, Mutation) (Snapshot, error)
	Snapshot(context.Context, string) (Snapshot, error)
	Authenticate(context.Context, string, CredentialDigest) (Snapshot, string, error)
}

type Events interface {
	Subscribe(string) (<-chan struct{}, func())
}
type Generator func() (string, error)
type Credentials struct {
	RoomCode    string
	PlayerToken string
}

type Service struct {
	repository       Repository
	events           Events
	generateCode     Generator
	generateToken    Generator
	generatePlayerID Generator
}

func NewService(repository Repository, events Events) *Service {
	return NewServiceWithGenerators(repository, events, randomRoomCode, randomToken, randomPlayerID)
}

func NewServiceWithGenerators(repository Repository, events Events, code, token, playerID Generator) *Service {
	return &Service{repository: repository, events: events, generateCode: code, generateToken: token, generatePlayerID: playerID}
}

func (s *Service) Create(ctx context.Context) (Credentials, error) {
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
		if _, err = s.repository.Create(ctx, r, DigestToken(token)); err != nil {
			if errors.Is(err, ErrDuplicateRoom) {
				continue
			}
			return Credentials{}, err
		}
		return Credentials{RoomCode: code, PlayerToken: token}, nil
	}
	return Credentials{}, ErrDuplicateRoom
}

func (s *Service) Join(ctx context.Context, code string) (Credentials, error) {
	for range 8 {
		token, id, err := s.identity()
		if err != nil {
			return Credentials{}, err
		}
		_, err = s.repository.Join(ctx, code, id, DigestToken(token))
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
func (s *Service) Authenticate(ctx context.Context, code, token string) (Snapshot, string, error) {
	return s.repository.Authenticate(ctx, code, DigestToken(token))
}
func (s *Service) SubmitMove(ctx context.Context, code, token string, move domain.Move) error {
	_, err := s.repository.Mutate(ctx, code, DigestToken(token), func(r *domain.Room, id string) error { return r.SubmitMove(id, move) })
	return err
}
func (s *Service) RequestNextRound(ctx context.Context, code, token string, round uint64) error {
	_, err := s.repository.Mutate(ctx, code, DigestToken(token), func(r *domain.Room, id string) error { return r.RequestNextRound(id, round) })
	return err
}
func (s *Service) Leave(ctx context.Context, code, token string) error {
	_, err := s.repository.Mutate(ctx, code, DigestToken(token), func(r *domain.Room, id string) error { return r.Leave(id) })
	return err
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
