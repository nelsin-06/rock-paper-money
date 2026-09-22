package application

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"example.com/rock-paper-money/internal/room/domain"
)

const CommandReceiptRetention = 7 * 24 * time.Hour

type Command struct {
	Key            string
	Operation      string
	RequestHash    [sha256.Size]byte
	ResponseStatus int
}

type CommandResult struct {
	Snapshot Snapshot        `json:"snapshot"`
	Leases   []PresenceLease `json:"leases,omitempty"`
	Balance  int64           `json:"balance,omitempty"`
	Replayed bool            `json:"-"`
}

type CommandRepository interface {
	CreateCommand(context.Context, *domain.Room, string, Command) (CommandResult, error)
	JoinCommand(context.Context, string, string, string, PresenceWindow, Command) (CommandResult, error)
	SubmitMoveCommand(context.Context, string, string, domain.Move, Command) (CommandResult, error)
	RequestNextRoundCommand(context.Context, string, string, uint64, Command) (CommandResult, error)
	MutateCommand(context.Context, string, string, Mutation, Command) (CommandResult, error)
	RechargeCommand(context.Context, string, int64, Command) (CommandResult, error)
	SnapshotForAccount(context.Context, string, string) (Snapshot, error)
}

func NewCommand(key, operation string, meaning any) (Command, error) {
	if key == "" || len(key) > 128 || strings.TrimSpace(key) != key || strings.ContainsAny(key, " \t\r\n") {
		return Command{}, ErrIdempotencyRequired
	}
	canonical, err := json.Marshal(meaning)
	if err != nil {
		return Command{}, err
	}
	return Command{Key: key, Operation: operation, RequestHash: sha256.Sum256(canonical), ResponseStatus: commandResponseStatus(operation)}, nil
}

func commandResponseStatus(operation string) int {
	switch operation {
	case "room.create", "room.join":
		return 201
	case "room.move", "room.next-round", "room.leave":
		return 204
	default:
		return 200
	}
}

func (s *Service) commandRepository() (CommandRepository, error) {
	repository, ok := s.repository.(CommandRepository)
	if !ok {
		return nil, errorsUnsupportedCommandRepository
	}
	return repository, nil
}

func (s *Service) CreateCommand(ctx context.Context, accountID, key string) (Credentials, error) {
	if accountID == "" {
		return Credentials{}, ErrUnauthorized
	}
	command, err := NewCommand(key, "room.create", struct{}{})
	if err != nil {
		return Credentials{}, err
	}
	repository, err := s.commandRepository()
	if err != nil {
		return Credentials{}, err
	}
	for range 8 {
		code, generateErr := s.generateCode()
		if generateErr != nil {
			return Credentials{}, generateErr
		}
		_, playerID, identityErr := s.identity()
		if identityErr != nil {
			return Credentials{}, identityErr
		}
		aggregate, domainErr := domain.New(code, playerID)
		if domainErr != nil {
			return Credentials{}, domainErr
		}
		result, createErr := repository.CreateCommand(ctx, aggregate, accountID, command)
		if errors.Is(createErr, ErrDuplicateRoom) {
			continue
		}
		if createErr != nil {
			return Credentials{}, createErr
		}
		return Credentials{RoomCode: result.Snapshot.State.Code}, nil
	}
	return Credentials{}, ErrDuplicateRoom
}

func (s *Service) JoinCommand(ctx context.Context, code, accountID, key string) (Credentials, error) {
	if accountID == "" {
		return Credentials{}, ErrUnauthorized
	}
	command, err := NewCommand(key, "room.join", struct {
		RoomCode string `json:"room_code"`
	}{code})
	if err != nil {
		return Credentials{}, err
	}
	repository, err := s.commandRepository()
	if err != nil {
		return Credentials{}, err
	}
	for range 8 {
		_, playerID, identityErr := s.identity()
		if identityErr != nil {
			return Credentials{}, identityErr
		}
		now := s.now()
		result, joinErr := repository.JoinCommand(ctx, code, playerID, accountID, s.presenceWindow(now), command)
		if errors.Is(joinErr, domain.ErrDuplicatePlayer) {
			continue
		}
		if joinErr != nil {
			return Credentials{}, joinErr
		}
		return Credentials{RoomCode: result.Snapshot.State.Code}, nil
	}
	return Credentials{}, domain.ErrDuplicatePlayer
}

func (s *Service) SubmitMoveCommand(ctx context.Context, code, accountID string, move domain.Move, key string) error {
	command, err := NewCommand(key, "room.move", struct {
		RoomCode string      `json:"room_code"`
		Move     domain.Move `json:"move"`
	}{code, move})
	if err != nil {
		return err
	}
	repository, err := s.commandRepository()
	if err != nil {
		return err
	}
	_, err = repository.SubmitMoveCommand(ctx, code, accountID, move, command)
	return err
}

func (s *Service) RequestNextRoundCommand(ctx context.Context, code, accountID string, round uint64, key string) error {
	command, err := NewCommand(key, "room.next-round", struct {
		RoomCode string `json:"room_code"`
		Round    uint64 `json:"round"`
	}{code, round})
	if err != nil {
		return err
	}
	repository, err := s.commandRepository()
	if err != nil {
		return err
	}
	_, err = repository.RequestNextRoundCommand(ctx, code, accountID, round, command)
	return err
}

func (s *Service) LeaveCommand(ctx context.Context, code, accountID, key string) error {
	command, err := NewCommand(key, "room.leave", struct {
		RoomCode string `json:"room_code"`
	}{code})
	if err != nil {
		return err
	}
	repository, err := s.commandRepository()
	if err != nil {
		return err
	}
	_, err = repository.MutateCommand(ctx, code, accountID, func(room *domain.Room, playerID string) error { return room.Leave(playerID) }, command)
	return err
}

func (s *Service) SnapshotForAccount(ctx context.Context, code, accountID string) (Snapshot, error) {
	if accountID == "" {
		return Snapshot{}, ErrUnauthorized
	}
	repository, err := s.commandRepository()
	if err != nil {
		return Snapshot{}, err
	}
	return repository.SnapshotForAccount(ctx, code, accountID)
}
