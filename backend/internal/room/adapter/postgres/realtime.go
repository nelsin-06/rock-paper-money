package postgres

import (
	"context"
	"errors"

	"example.com/rock-paper-money/internal/room/application"
	"github.com/jackc/pgx/v5"
)

// LoadRoom implements the realtime authoritative snapshot loader. Redis hints
// select when to load; they never supply business state.
func (r *Repository) LoadRoom(ctx context.Context, roomID string) (application.Snapshot, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return application.Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	var revision uint64
	if err = tx.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1", roomID).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return application.Snapshot{}, application.ErrRoomNotFound
	} else if err != nil {
		return application.Snapshot{}, err
	}
	aggregate, _, err := load(ctx, tx, roomID)
	if err != nil {
		return application.Snapshot{}, err
	}
	presence, err := loadPresence(ctx, tx, roomID)
	if err != nil {
		return application.Snapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Snapshot{}, err
	}
	return application.Snapshot{State: aggregate.State(), Revision: revision, Presence: presence}, nil
}

func (r *Repository) AuthorizeRoom(ctx context.Context, roomID, accountID string) (string, error) {
	var role string
	if err := r.pool.QueryRow(ctx, "SELECT role FROM room_seats WHERE room_code=$1 AND auth_user_id=$2", roomID, accountID).Scan(&role); errors.Is(err, pgx.ErrNoRows) {
		return "", application.ErrUnauthorized
	} else if err != nil {
		return "", err
	}
	return role, nil
}

func loadPresence(ctx context.Context, tx pgx.Tx, roomID string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.role,COALESCE(p.present,false)
		FROM room_seats s LEFT JOIN room_presence p
		  ON p.room_code=s.room_code AND p.player_id=s.player_id
		WHERE s.room_code=$1 ORDER BY s.role`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	presence := make(map[string]bool)
	for rows.Next() {
		var role string
		var present bool
		if err = rows.Scan(&role, &present); err != nil {
			return nil, err
		}
		presence[role] = present
	}
	return presence, rows.Err()
}
