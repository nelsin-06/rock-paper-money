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

func (r *Repository) Create(ctx context.Context, aggregate *domain.Room, digest application.CredentialDigest, authUserID string) (application.Snapshot, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return application.Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	state := aggregate.PersistenceState()
	if err = ensureUserWallet(ctx, tx, authUserID); err != nil {
		return application.Snapshot{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_rooms(code) VALUES($1)", state.Code); isUnique(err) {
		return application.Snapshot{}, application.ErrDuplicateRoom
	} else if err != nil {
		return application.Snapshot{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_rounds(room_code,number) VALUES($1,1)", state.Code); err != nil {
		return application.Snapshot{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_seats(room_code,role,player_id,credential_digest,auth_user_id) VALUES($1,'host',$2,$3,$4)", state.Code, state.Players[0].ID, digest[:], authUserID); err != nil {
		return application.Snapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Snapshot{}, err
	}
	return application.Snapshot{State: aggregate.State(), Revision: 1}, nil
}

func (r *Repository) JoinFunded(ctx context.Context, code, playerID string, digest application.CredentialDigest, authUserID string, window application.PresenceWindow) (application.Snapshot, []application.PresenceLease, error) {
	var leases []application.PresenceLease
	snapshot, err := r.write(ctx, code, nil, "", func(tx pgx.Tx, aggregate *domain.Room, _ string) error {
		if err := aggregate.Join(playerID); err != nil {
			return err
		}
		var occupied bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM room_seats WHERE room_code=$1 AND auth_user_id=$2)", code, authUserID).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return application.ErrAccountSeated
		}
		if err := fundRound(ctx, tx, code, aggregate.State().Round, authUserID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO room_seats(room_code,role,player_id,credential_digest,auth_user_id) VALUES($1,'guest',$2,$3,$4)", code, playerID, digest[:], authUserID)
		if isUnique(err) {
			return application.ErrAccountSeated
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

func (r *Repository) SubmitMoveAndSettle(ctx context.Context, code string, digest application.CredentialDigest, authUserID string, move domain.Move) (application.Snapshot, error) {
	return r.write(ctx, code, &digest, authUserID, func(tx pgx.Tx, aggregate *domain.Room, playerID string) error {
		state := aggregate.State()
		if err := fundRound(ctx, tx, code, state.Round, ""); err != nil {
			return err
		}
		if err := aggregate.SubmitMove(playerID, move); err != nil {
			return err
		}
		if !state.Resolved && aggregate.State().Resolved {
			return settleRound(ctx, tx, code, aggregate.State())
		}
		return nil
	})
}

func (r *Repository) RequestNextRoundAndFund(ctx context.Context, code string, digest application.CredentialDigest, authUserID string, round uint64) (application.Snapshot, error) {
	return r.write(ctx, code, &digest, authUserID, func(tx pgx.Tx, aggregate *domain.Room, playerID string) error {
		previousRound := aggregate.State().Round
		if err := aggregate.RequestNextRound(playerID, round); err != nil {
			return err
		}
		if aggregate.State().Round > previousRound {
			return fundRound(ctx, tx, code, aggregate.State().Round, "")
		}
		return nil
	})
}

func (r *Repository) Mutate(ctx context.Context, code string, digest application.CredentialDigest, authUserID string, mutation application.Mutation) (application.Snapshot, error) {
	return r.write(ctx, code, &digest, authUserID, func(_ pgx.Tx, aggregate *domain.Room, id string) error { return mutation(aggregate, id) })
}

func (r *Repository) write(ctx context.Context, code string, digest *application.CredentialDigest, authUserID string, change func(pgx.Tx, *domain.Room, string) error) (application.Snapshot, error) {
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
		err = tx.QueryRow(ctx, "SELECT player_id,role FROM room_seats WHERE room_code=$1 AND credential_digest=$2 AND auth_user_id=$3", code, digest[:], authUserID).Scan(&playerID, &role)
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

func (r *Repository) Authenticate(ctx context.Context, code string, digest application.CredentialDigest, authUserID string) (application.Snapshot, string, error) {
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
	if err = tx.QueryRow(ctx, "SELECT role FROM room_seats WHERE room_code=$1 AND credential_digest=$2 AND auth_user_id=$3", code, digest[:], authUserID).Scan(&role); errors.Is(err, pgx.ErrNoRows) {
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

func (r *Repository) RefreshPresence(ctx context.Context, code string, digest application.CredentialDigest, authUserID string, window application.PresenceWindow) ([]application.PresenceLease, error) {
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
	if err = tx.QueryRow(ctx, "SELECT player_id FROM room_seats WHERE room_code=$1 AND credential_digest=$2 AND auth_user_id=$3", code, digest[:], authUserID).Scan(&playerID); errors.Is(err, pgx.ErrNoRows) {
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
	snapshot, err := r.write(ctx, lease.RoomCode, nil, "", func(tx pgx.Tx, aggregate *domain.Room, _ string) error {
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
		if fundErr := fundRound(ctx, tx, lease.RoomCode, aggregate.State().Round, ""); fundErr != nil {
			return fundErr
		}
		if forfeitErr := aggregate.Forfeit(lease.PlayerID, lease.Round); forfeitErr != nil {
			if errors.Is(forfeitErr, domain.ErrStaleRound) || errors.Is(forfeitErr, domain.ErrRoundResolved) || errors.Is(forfeitErr, domain.ErrRoomClosed) {
				return errPresenceLeaseCurrent
			}
			return forfeitErr
		}
		return settleRound(ctx, tx, lease.RoomCode, aggregate.State())
	})
	if errors.Is(err, errPresenceLeaseCurrent) {
		return application.Snapshot{}, false, nil
	}
	return snapshot, err == nil, err
}

func (r *Repository) Balance(ctx context.Context, authUserID string) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err = ensureUserWallet(ctx, tx, authUserID); err != nil {
		return 0, err
	}
	var balance int64
	if err = tx.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id=$1", userAccountID(authUserID)).Scan(&balance); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return balance, nil
}

func (r *Repository) Recharge(ctx context.Context, authUserID string, amount int64, idempotencyKey string) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err = ensureUserWallet(ctx, tx, authUserID); err != nil {
		return 0, err
	}
	userAccount := userAccountID(authUserID)
	if err = lockWallets(ctx, tx, []string{"mint", userAccount}); err != nil {
		return 0, err
	}
	businessKey := "recharge:" + authUserID + ":" + idempotencyKey
	var existingOwner string
	var existingAmount int64
	err = tx.QueryRow(ctx, "SELECT auth_user_id::text,amount FROM wallet_transactions WHERE business_key=$1", businessKey).Scan(&existingOwner, &existingAmount)
	if err == nil {
		if existingOwner != authUserID || existingAmount != amount {
			return 0, application.ErrIdempotencyConflict
		}
		var balance int64
		if err = tx.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id=$1", userAccount).Scan(&balance); err != nil {
			return 0, err
		}
		if err = tx.Commit(ctx); err != nil {
			return 0, err
		}
		return balance, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var transactionID int64
	if err = tx.QueryRow(ctx, "INSERT INTO wallet_transactions(business_key,transaction_type,auth_user_id,amount) VALUES($1,'recharge',$2,$3) RETURNING transaction_id", businessKey, authUserID, amount).Scan(&transactionID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO wallet_postings(transaction_id,account_id,amount) VALUES($1,'mint',$2),($1,$3,$4)", transactionID, -amount, userAccount, amount); err != nil {
		return 0, err
	}
	var balance int64
	if err = tx.QueryRow(ctx, "UPDATE wallet_accounts SET balance=balance+$2 WHERE account_id=$1 RETURNING balance", userAccount, amount).Scan(&balance); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, "UPDATE wallet_accounts SET balance=balance-$1 WHERE account_id='mint'", amount); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return balance, nil
}

func (r *Repository) Analytics(ctx context.Context) (application.Analytics, error) {
	var analytics application.Analytics
	if err := r.pool.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id='house'").Scan(&analytics.TotalHouseEarnings); err != nil {
		return application.Analytics{}, err
	}
	rows, err := r.pool.Query(ctx, "SELECT room_code,round_number,result,COALESCE(winner_role,''),forfeited,house_earnings,resolved_at FROM game_round_history ORDER BY resolved_at DESC,room_code,round_number DESC LIMIT 500")
	if err != nil {
		return application.Analytics{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var played application.PlayedRound
		if err = rows.Scan(&played.RoomCode, &played.Round, &played.Result, &played.WinnerRole, &played.Forfeit, &played.HouseEarnings, &played.ResolvedAt); err != nil {
			return application.Analytics{}, err
		}
		analytics.Rounds = append(analytics.Rounds, played)
	}
	return analytics, rows.Err()
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

func ensureUserWallet(ctx context.Context, tx pgx.Tx, authUserID string) error {
	_, err := tx.Exec(ctx, "INSERT INTO wallet_accounts(account_id,account_type,auth_user_id) VALUES($1,'user',$2) ON CONFLICT (auth_user_id) DO NOTHING", userAccountID(authUserID), authUserID)
	return err
}

func userAccountID(authUserID string) string { return "user:" + authUserID }

func escrowAccountID(code string, round uint64) string {
	return fmt.Sprintf("escrow:%s:%d", code, round)
}

func lockWallets(ctx context.Context, tx pgx.Tx, accountIDs []string) error {
	rows, err := tx.Query(ctx, "SELECT account_id FROM wallet_accounts WHERE account_id=ANY($1) ORDER BY account_id FOR UPDATE", accountIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != len(accountIDs) {
		return errors.New("wallet account set is incomplete")
	}
	return nil
}

func fundRound(ctx context.Context, tx pgx.Tx, code string, round uint64, joiningAuthUserID string) error {
	if _, err := tx.Exec(ctx, "INSERT INTO room_rounds(room_code,number) VALUES($1,$2) ON CONFLICT (room_code,number) DO NOTHING", code, round); err != nil {
		return err
	}
	var funded bool
	if err := tx.QueryRow(ctx, "SELECT funded FROM room_rounds WHERE room_code=$1 AND number=$2", code, round).Scan(&funded); err != nil {
		return err
	}
	if funded {
		return nil
	}
	rows, err := tx.Query(ctx, "SELECT auth_user_id::text FROM room_seats WHERE room_code=$1 ORDER BY role", code)
	if err != nil {
		return err
	}
	owners := make([]string, 0, 2)
	for rows.Next() {
		var owner *string
		if err = rows.Scan(&owner); err != nil {
			rows.Close()
			return err
		}
		if owner == nil || *owner == "" {
			rows.Close()
			return application.ErrUnauthorized
		}
		owners = append(owners, *owner)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if joiningAuthUserID != "" {
		owners = append(owners, joiningAuthUserID)
	}
	if len(owners) != 2 {
		return domain.ErrRoomNotReady
	}
	accountIDs := []string{userAccountID(owners[0]), userAccountID(owners[1])}
	for _, owner := range owners {
		if err = ensureUserWallet(ctx, tx, owner); err != nil {
			return err
		}
	}
	escrow := escrowAccountID(code, round)
	if _, err = tx.Exec(ctx, "INSERT INTO wallet_accounts(account_id,account_type,room_code,round_number) VALUES($1,'escrow',$2,$3) ON CONFLICT (room_code,round_number) DO NOTHING", escrow, code, round); err != nil {
		return err
	}
	accountIDs = append(accountIDs, escrow)
	if err = lockWallets(ctx, tx, accountIDs); err != nil {
		return err
	}
	for _, accountID := range accountIDs[:2] {
		var balance int64
		if err = tx.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id=$1", accountID).Scan(&balance); err != nil {
			return err
		}
		if balance < application.RoundStake {
			return application.ErrInsufficientFunds
		}
	}
	businessKey := fmt.Sprintf("stake:%s:%d", code, round)
	var transactionID int64
	err = tx.QueryRow(ctx, "INSERT INTO wallet_transactions(business_key,transaction_type) VALUES($1,'stake') ON CONFLICT (business_key) DO NOTHING RETURNING transaction_id", businessKey).Scan(&transactionID)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, "UPDATE room_rounds SET funded=true WHERE room_code=$1 AND number=$2", code, round)
		return err
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO wallet_postings(transaction_id,account_id,amount) VALUES($1,$2,$4),($1,$3,$4),($1,$5,$6)", transactionID, accountIDs[0], accountIDs[1], -application.RoundStake, escrow, application.RoundStake*2); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE wallet_accounts SET balance=balance-$2 WHERE account_id=ANY($1)", accountIDs[:2], application.RoundStake); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE wallet_accounts SET balance=balance+$2 WHERE account_id=$1", escrow, application.RoundStake*2); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE room_rounds SET funded=true WHERE room_code=$1 AND number=$2", code, round)
	return err
}

func settleRound(ctx context.Context, tx pgx.Tx, code string, state domain.State) error {
	var alreadyRecorded bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM game_round_history WHERE room_code=$1 AND round_number=$2)", code, state.Round).Scan(&alreadyRecorded); err != nil || alreadyRecorded {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT role,'user:' || auth_user_id::text FROM room_seats WHERE room_code=$1 ORDER BY CASE role WHEN 'host' THEN 0 ELSE 1 END", code)
	if err != nil {
		return err
	}
	accounts := make([]string, 0, 2)
	for rows.Next() {
		var role, accountID string
		if err = rows.Scan(&role, &accountID); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, accountID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(accounts) != 2 {
		return domain.ErrRoomNotReady
	}
	escrow := escrowAccountID(code, state.Round)
	lockIDs := []string{"house", accounts[0], accounts[1], escrow}
	if err = lockWallets(ctx, tx, lockIDs); err != nil {
		return err
	}
	var escrowBalance int64
	if err = tx.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id=$1", escrow).Scan(&escrowBalance); err != nil {
		return err
	}
	if escrowBalance != application.RoundStake*2 {
		return errors.New("funded round escrow has invalid balance")
	}
	var transactionID int64
	businessKey := fmt.Sprintf("settlement:%s:%d", code, state.Round)
	err = tx.QueryRow(ctx, "INSERT INTO wallet_transactions(business_key,transaction_type) VALUES($1,'settlement') ON CONFLICT (business_key) DO NOTHING RETURNING transaction_id", businessKey).Scan(&transactionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	winnerRole := ""
	houseEarnings := int64(0)
	if state.Result == domain.Draw {
		if _, err = tx.Exec(ctx, "INSERT INTO wallet_postings(transaction_id,account_id,amount) VALUES($1,$2,$3),($1,$4,$5),($1,$6,$5)", transactionID, escrow, -application.RoundStake*2, accounts[0], application.RoundStake, accounts[1]); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE wallet_accounts SET balance=balance+$2 WHERE account_id=ANY($1)", accounts, application.RoundStake); err != nil {
			return err
		}
	} else {
		winnerAccount := accounts[0]
		winnerRole = "host"
		if state.Result == domain.PlayerTwoWins {
			winnerAccount = accounts[1]
			winnerRole = "guest"
		}
		houseEarnings = application.HousePayout
		if _, err = tx.Exec(ctx, "INSERT INTO wallet_postings(transaction_id,account_id,amount) VALUES($1,$2,$3),($1,$4,$5),($1,'house',$6)", transactionID, escrow, -application.RoundStake*2, winnerAccount, application.WinnerPayout, application.HousePayout); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE wallet_accounts SET balance=balance+$2 WHERE account_id=$1", winnerAccount, application.WinnerPayout); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE wallet_accounts SET balance=balance+$1 WHERE account_id='house'", application.HousePayout); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, "UPDATE wallet_accounts SET balance=0 WHERE account_id=$1", escrow); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO game_round_history(room_code,round_number,host_account_id,guest_account_id,result,winner_role,forfeited,house_earnings) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8)", code, state.Round, accounts[0], accounts[1], state.Result, winnerRole, state.Forfeit, houseEarnings)
	return err
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
