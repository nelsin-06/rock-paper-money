package postgres

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode"

	"example.com/rock-paper-money/internal/worker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RoomStateListener struct {
	config *pgx.ConnConfig
}

func NewRoomStateListener(pool *pgxpool.Pool) *RoomStateListener {
	return &RoomStateListener{config: pool.Config().ConnConfig.Copy()}
}

func (l *RoomStateListener) Listen(ctx context.Context, established func(), notify func(worker.RoomHint)) error {
	conn, err := pgx.ConnectConfig(ctx, l.config.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(ctx, "LISTEN "+roomStateChangedChannel); err != nil {
		return err
	}
	established()
	for {
		notification, waitErr := conn.WaitForNotification(ctx)
		if waitErr != nil {
			return waitErr
		}
		hint, parseErr := parseRoomHint(notification.Payload)
		if parseErr == nil {
			notify(hint)
		}
	}
}

func parseRoomHint(payload string) (worker.RoomHint, error) {
	if len(payload) == 0 || len(payload) > 128 {
		return worker.RoomHint{}, errors.New("invalid room notification hint length")
	}
	separator := strings.LastIndexByte(payload, ':')
	if separator <= 0 || separator == len(payload)-1 {
		return worker.RoomHint{}, errors.New("invalid room notification hint")
	}
	code := payload[:separator]
	if len(code) > 64 {
		return worker.RoomHint{}, errors.New("invalid room notification code")
	}
	for _, character := range code {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '-' && character != '_' {
			return worker.RoomHint{}, errors.New("invalid room notification code")
		}
	}
	round, err := strconv.ParseUint(payload[separator+1:], 10, 64)
	if err != nil || round == 0 {
		return worker.RoomHint{}, errors.New("invalid room notification round")
	}
	return worker.RoomHint{RoomCode: code, Round: round}, nil
}
