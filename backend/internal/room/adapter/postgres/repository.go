package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct{ pool *pgxpool.Pool }

var errPresenceLeaseCurrent = errors.New("presence lease is still current")

func NewRepository(pool *pgxpool.Pool) *Repository   { return &Repository{pool: pool} }
func (r *Repository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }

func (r *Repository) Create(ctx context.Context, aggregate *domain.Room, digest application.CredentialDigest) (application.Snapshot, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return application.Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	state := aggregate.PersistenceState()
	if _, err = tx.Exec(ctx, "INSERT INTO room_rooms(code) VALUES($1)", state.Code); isUnique(err) {
		return application.Snapshot{}, application.ErrDuplicateRoom
	} else if err != nil {
		return application.Snapshot{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_rounds(room_code,number) VALUES($1,1)", state.Code); err != nil {
		return application.Snapshot{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_seats(room_code,role,player_id,credential_digest) VALUES($1,'host',$2,$3)", state.Code, state.Players[0].ID, digest[:]); err != nil {
		return application.Snapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Snapshot{}, err
	}
	return application.Snapshot{State: aggregate.State(), Revision: 1}, nil
}

func (r *Repository) Join(ctx context.Context, code, playerID string, digest application.CredentialDigest, window application.PresenceWindow) (application.Snapshot, []application.PresenceLease, error) {
	var leases []application.PresenceLease
	snapshot, err := r.write(ctx, code, nil, func(tx pgx.Tx, aggregate *domain.Room, _ string) error {
		if err := aggregate.Join(playerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO room_seats(room_code,role,player_id,credential_digest) VALUES($1,'guest',$2,$3)", code, playerID, digest[:])
		if isUnique(err) {
			return domain.ErrDuplicatePlayer
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "INSERT INTO room_presence(room_code,player_id,generation,deadline,refreshed_at) SELECT room_code,player_id,1,$2,$3 FROM room_seats WHERE room_code=$1 ON CONFLICT(room_code,player_id) DO UPDATE SET generation=room_presence.generation+1,deadline=EXCLUDED.deadline,refreshed_at=EXCLUDED.refreshed_at RETURNING player_id,generation", code, window.Deadline, window.ObservedAt)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var lease application.PresenceLease
			if err = rows.Scan(&lease.PlayerID, &lease.Generation); err != nil {
				return err
			}
			lease.RoomCode = code
			lease.Round = aggregate.State().Round
			lease.Deadline = window.Deadline
			lease.EvaluateAt = window.EvaluateAt
			lease.ProofAfter = window.ProofAfter
			lease.Active = true
			leases = append(leases, lease)
		}
		return rows.Err()
	})
	return snapshot, leases, err
}

func (r *Repository) Mutate(ctx context.Context, code string, digest application.CredentialDigest, mutation application.Mutation) (application.Snapshot, error) {
	return r.write(ctx, code, &digest, func(_ pgx.Tx, aggregate *domain.Room, id string) error { return mutation(aggregate, id) })
}

func (r *Repository) write(ctx context.Context, code string, digest *application.CredentialDigest, change func(pgx.Tx, *domain.Room, string) error) (application.Snapshot, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return application.Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	var revision uint64
	if err = tx.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1 FOR UPDATE", code).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return application.Snapshot{}, application.ErrRoomNotFound
	} else if err != nil {
		return application.Snapshot{}, err
	}
	aggregate, roles, err := load(ctx, tx, code)
	if err != nil {
		return application.Snapshot{}, err
	}
	previousRound := aggregate.PersistenceState().Round
	playerID := ""
	if digest != nil {
		var role string
		err = tx.QueryRow(ctx, "SELECT player_id,role FROM room_seats WHERE room_code=$1 AND credential_digest=$2", code, digest[:]).Scan(&playerID, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return application.Snapshot{}, application.ErrUnauthorized
		} else if err != nil {
			return application.Snapshot{}, err
		}
		_ = role
	}
	if err = change(tx, aggregate, playerID); err != nil {
		return application.Snapshot{}, err
	}
	revision++
	if err = save(ctx, tx, aggregate, roles, previousRound, revision); err != nil {
		return application.Snapshot{}, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_notify('room_changes',$1)", fmt.Sprintf("%s:%d", code, revision)); err != nil {
		return application.Snapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Snapshot{}, err
	}
	return application.Snapshot{State: aggregate.State(), Revision: revision}, nil
}

func (r *Repository) Snapshot(ctx context.Context, code string) (application.Snapshot, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return application.Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	var revision uint64
	if err = tx.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1", code).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return application.Snapshot{}, application.ErrRoomNotFound
	} else if err != nil {
		return application.Snapshot{}, err
	}
	aggregate, _, err := load(ctx, tx, code)
	if err != nil {
		return application.Snapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Snapshot{}, err
	}
	return application.Snapshot{State: aggregate.State(), Revision: revision}, nil
}

func (r *Repository) Authenticate(ctx context.Context, code string, digest application.CredentialDigest) (application.Snapshot, string, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return application.Snapshot{}, "", err
	}
	defer tx.Rollback(ctx)
	var revision uint64
	if err = tx.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1", code).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return application.Snapshot{}, "", application.ErrRoomNotFound
	} else if err != nil {
		return application.Snapshot{}, "", err
	}
	var role string
	if err = tx.QueryRow(ctx, "SELECT role FROM room_seats WHERE room_code=$1 AND credential_digest=$2", code, digest[:]).Scan(&role); errors.Is(err, pgx.ErrNoRows) {
		return application.Snapshot{}, "", application.ErrUnauthorized
	} else if err != nil {
		return application.Snapshot{}, "", err
	}
	aggregate, _, err := load(ctx, tx, code)
	if err != nil {
		return application.Snapshot{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Snapshot{}, "", err
	}
	return application.Snapshot{State: aggregate.State(), Revision: revision}, role, nil
}

func (r *Repository) RefreshPresence(ctx context.Context, code string, digest application.CredentialDigest, window application.PresenceWindow) ([]application.PresenceLease, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var status string
	var round uint64
	if err = tx.QueryRow(ctx, "SELECT status,current_round FROM room_rooms WHERE code=$1 FOR UPDATE", code).Scan(&status, &round); errors.Is(err, pgx.ErrNoRows) {
		return nil, application.ErrRoomNotFound
	} else if err != nil {
		return nil, err
	}
	var playerID string
	if err = tx.QueryRow(ctx, "SELECT player_id FROM room_seats WHERE room_code=$1 AND credential_digest=$2", code, digest[:]).Scan(&playerID); errors.Is(err, pgx.ErrNoRows) {
		return nil, application.ErrUnauthorized
	} else if err != nil {
		return nil, err
	}
	var playerCount int
	var result *string
	if err = tx.QueryRow(ctx, "SELECT count(*),max(result) FROM room_seats s LEFT JOIN room_rounds rr ON rr.room_code=s.room_code AND rr.number=$2 WHERE s.room_code=$1", code, round).Scan(&playerCount, &result); err != nil {
		return nil, err
	}
	active := status == "active" && result == nil && playerCount == 2
	if _, err = tx.Exec(ctx, "INSERT INTO room_presence(room_code,player_id,generation,deadline,refreshed_at) VALUES($1,$2,1,$3,$4) ON CONFLICT(room_code,player_id) DO UPDATE SET deadline=EXCLUDED.deadline,refreshed_at=EXCLUDED.refreshed_at", code, playerID, window.Deadline, window.ObservedAt); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, "UPDATE room_presence SET generation=generation+1 WHERE room_code=$1 RETURNING player_id,generation,deadline", code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	evaluationDelay := window.EvaluateAt.Sub(window.Deadline)
	leases := make([]application.PresenceLease, 0, playerCount)
	for rows.Next() {
		var lease application.PresenceLease
		if err = rows.Scan(&lease.PlayerID, &lease.Generation, &lease.Deadline); err != nil {
			return nil, err
		}
		lease.RoomCode = code
		lease.Round = round
		lease.EvaluateAt = lease.Deadline.Add(evaluationDelay)
		lease.ProofAfter = lease.Deadline
		lease.Active = active
		leases = append(leases, lease)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return leases, nil
}

func (r *Repository) ForfeitExpired(ctx context.Context, lease application.PresenceLease, now time.Time) (application.Snapshot, bool, error) {
	snapshot, err := r.write(ctx, lease.RoomCode, nil, func(tx pgx.Tx, aggregate *domain.Room, _ string) error {
		var generation uint64
		var deadline time.Time
		if queryErr := tx.QueryRow(ctx, "SELECT generation,deadline FROM room_presence WHERE room_code=$1 AND player_id=$2 FOR UPDATE", lease.RoomCode, lease.PlayerID).Scan(&generation, &deadline); errors.Is(queryErr, pgx.ErrNoRows) {
			return errPresenceLeaseCurrent
		} else if queryErr != nil {
			return queryErr
		}
		if generation != lease.Generation || now.Before(deadline) {
			return errPresenceLeaseCurrent
		}
		var remainingConnected bool
		if queryErr := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM room_presence WHERE room_code=$1 AND player_id<>$2 AND deadline>$3 AND refreshed_at>$4)", lease.RoomCode, lease.PlayerID, now, lease.ProofAfter).Scan(&remainingConnected); queryErr != nil {
			return queryErr
		}
		if !remainingConnected {
			return errPresenceLeaseCurrent
		}
		if forfeitErr := aggregate.Forfeit(lease.PlayerID, lease.Round); forfeitErr != nil {
			if errors.Is(forfeitErr, domain.ErrStaleRound) || errors.Is(forfeitErr, domain.ErrRoundResolved) || errors.Is(forfeitErr, domain.ErrRoomClosed) {
				return errPresenceLeaseCurrent
			}
			return forfeitErr
		}
		return nil
	})
	if errors.Is(err, errPresenceLeaseCurrent) {
		return application.Snapshot{}, false, nil
	}
	return snapshot, err == nil, err
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func load(ctx context.Context, q queryer, code string) (*domain.Room, map[string]string, error) {
	var round uint64
	var status string
	var result, forfeitedRole *string
	if err := q.QueryRow(ctx, "SELECT r.current_round,r.status,rr.result,rr.forfeited_role FROM room_rooms r JOIN room_rounds rr ON rr.room_code=r.code AND rr.number=r.current_round WHERE r.code=$1", code).Scan(&round, &status, &result, &forfeitedRole); err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, "SELECT role,player_id,wins FROM room_seats WHERE room_code=$1 ORDER BY CASE role WHEN 'host' THEN 0 ELSE 1 END", code)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	state := domain.PersistenceState{Code: code, Round: round, Closed: status == "closed", Moves: map[string]domain.Move{}, NextRoundRequests: map[string]bool{}}
	roles := map[string]string{}
	for rows.Next() {
		var role, id string
		var wins int
		if err = rows.Scan(&role, &id, &wins); err != nil {
			return nil, nil, err
		}
		state.Players = append(state.Players, domain.PersistedPlayer{ID: id, Wins: wins})
		roles[role] = id
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	moveRows, err := q.Query(ctx, "SELECT s.player_id,m.move FROM room_moves m JOIN room_seats s ON s.room_code=m.room_code AND s.role=m.role WHERE m.room_code=$1 AND m.round_number=$2", code, round)
	if err != nil {
		return nil, nil, err
	}
	if err = collectMoves(moveRows, state.Moves); err != nil {
		return nil, nil, err
	}
	requestRows, err := q.Query(ctx, "SELECT s.player_id FROM room_next_round_requests n JOIN room_seats s ON s.room_code=n.room_code AND s.role=n.role WHERE n.room_code=$1 AND n.round_number=$2", code, round)
	if err != nil {
		return nil, nil, err
	}
	if err = collectNextRoundRequests(requestRows, state.NextRoundRequests); err != nil {
		return nil, nil, err
	}
	if result != nil {
		state.Resolved = true
		state.Result = domain.Result(*result)
	}
	if forfeitedRole != nil {
		state.ForfeitedPlayerID = roles[*forfeitedRole]
	}
	aggregate, err := domain.Restore(state)
	return aggregate, roles, err
}

func collectMoves(rows pgx.Rows, moves map[string]domain.Move) error {
	defer rows.Close()
	for rows.Next() {
		var id string
		var move domain.Move
		if err := rows.Scan(&id, &move); err != nil {
			return err
		}
		moves[id] = move
	}
	return rows.Err()
}

func collectNextRoundRequests(rows pgx.Rows, requests map[string]bool) error {
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		requests[id] = true
	}
	return rows.Err()
}

func save(ctx context.Context, tx pgx.Tx, aggregate *domain.Room, roles map[string]string, previousRound, revision uint64) error {
	state := aggregate.PersistenceState()
	status := "active"
	if state.Closed {
		status = "closed"
	}
	if _, err := tx.Exec(ctx, "UPDATE room_rooms SET status=$2,current_round=$3,revision=$4,updated_at=now(),closed_at=CASE WHEN $2='closed' THEN COALESCE(closed_at,now()) ELSE NULL END WHERE code=$1", state.Code, status, state.Round, revision); err != nil {
		return err
	}
	for _, role := range []string{"host", "guest"} {
		id, ok := roles[role]
		if !ok && role == "guest" && len(state.Players) == 2 {
			id = state.Players[1].ID
			ok = true
		}
		if !ok {
			continue
		}
		wins := 0
		for _, p := range state.Players {
			if p.ID == id {
				wins = p.Wins
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE room_seats SET wins=$3 WHERE room_code=$1 AND role=$2", state.Code, role, wins); err != nil {
			return err
		}
	}
	var result any
	var resolvedAt any
	var forfeitedRole any
	if state.Resolved {
		result = string(state.Result)
		resolvedAt = "now"
		for role, id := range roles {
			if id == state.ForfeitedPlayerID {
				forfeitedRole = role
			}
		}
	}
	if _, err := tx.Exec(ctx, "INSERT INTO room_rounds(room_code,number,result,resolved_at,forfeited_role) VALUES($1,$2,$3,CASE WHEN $4::text IS NULL THEN NULL ELSE now() END,$5) ON CONFLICT(room_code,number) DO UPDATE SET result=EXCLUDED.result,resolved_at=EXCLUDED.resolved_at,forfeited_role=EXCLUDED.forfeited_role", state.Code, state.Round, result, resolvedAt, forfeitedRole); err != nil {
		return err
	}
	if state.Round > previousRound {
		for role := range roles {
			if _, err := tx.Exec(ctx, "INSERT INTO room_next_round_requests(room_code,round_number,role) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", state.Code, previousRound, role); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM room_moves WHERE room_code=$1 AND round_number=$2", state.Code, state.Round); err != nil {
		return err
	}
	for role, id := range roles {
		if move, ok := state.Moves[id]; ok {
			if _, err := tx.Exec(ctx, "INSERT INTO room_moves(room_code,round_number,role,move) VALUES($1,$2,$3,$4)", state.Code, state.Round, role, move); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM room_next_round_requests WHERE room_code=$1 AND round_number=$2", state.Code, state.Round); err != nil {
		return err
	}
	for role, id := range roles {
		if state.NextRoundRequests[id] {
			if _, err := tx.Exec(ctx, "INSERT INTO room_next_round_requests(room_code,round_number,role) VALUES($1,$2,$3)", state.Code, state.Round, role); err != nil {
				return err
			}
		}
	}
	return nil
}
func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
