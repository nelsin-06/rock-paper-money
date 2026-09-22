package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"example.com/rock-paper-money/internal/latency"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
	"example.com/rock-paper-money/internal/worker"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) CreateCommand(ctx context.Context, aggregate *domain.Room, accountID string, command application.Command) (application.CommandResult, error) {
	finishPhase := latency.StartPhase(ctx, latency.PhasePoolBeginTransaction)
	tx, err := r.pool.Begin(ctx)
	finishPhase()
	if err != nil {
		return application.CommandResult{}, err
	}
	defer tx.Rollback(ctx)
	finishPhase = latency.StartPhase(ctx, latency.PhaseIdempotencyLockReceipt)
	replay, found, receiptErr := commandReceipt(ctx, tx, accountID, command)
	finishPhase()
	if receiptErr != nil || found {
		return replay, receiptErr
	}
	state := aggregate.PersistenceState()
	finishPhase = latency.StartPhase(ctx, latency.PhasePersistenceWalletSettlement)
	if err = ensureUserWallet(ctx, tx, accountID); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	account := userAccountID(accountID)
	if err = lockWallets(ctx, tx, []string{account}); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	var balance int64
	if err = tx.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id=$1", account).Scan(&balance); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	if balance < application.RoundStake {
		finishPhase()
		return application.CommandResult{}, application.ErrInsufficientFunds
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_rooms(code) VALUES($1)", state.Code); isUnique(err) {
		finishPhase()
		return application.CommandResult{}, application.ErrDuplicateRoom
	} else if err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_rounds(room_code,number) VALUES($1,1)", state.Code); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_seats(room_code,role,player_id,auth_user_id) VALUES($1,'host',$2,$3)", state.Code, state.Players[0].ID, accountID); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	finishPhase()
	result := application.CommandResult{Snapshot: application.Snapshot{State: aggregate.State(), Revision: 1}}
	finishPhase = latency.StartPhase(ctx, latency.PhaseOutboxReceipt)
	if err = persistCommandEffects(ctx, tx, state.Code, 1, accountID, command, result); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	finishPhase()
	finishPhase = latency.StartPhase(ctx, latency.PhaseCommit)
	if err = tx.Commit(ctx); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	finishPhase()
	return result, nil
}

func (r *Repository) JoinCommand(ctx context.Context, code, playerID, accountID string, window application.PresenceWindow, command application.Command) (application.CommandResult, error) {
	return r.writeAccountCommand(ctx, code, accountID, command, false, func(tx pgx.Tx, aggregate *domain.Room, roles map[string]string, _ string) ([]application.PresenceLease, error) {
		finishMutation := latency.StartPhase(ctx, latency.PhaseDomainMutation)
		if err := aggregate.Join(playerID); err != nil {
			finishMutation()
			return nil, err
		}
		finishMutation()
		finishPersistence := latency.StartPhase(ctx, latency.PhasePersistenceWalletSettlement)
		defer finishPersistence()
		var occupied bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM room_seats WHERE room_code=$1 AND auth_user_id=$2)", code, accountID).Scan(&occupied); err != nil {
			return nil, err
		}
		if occupied {
			return nil, application.ErrAccountSeated
		}
		if err := fundRound(ctx, tx, code, aggregate.State().Round, accountID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO room_seats(room_code,role,player_id,auth_user_id) VALUES($1,'guest',$2,$3)", code, playerID, accountID); isUnique(err) {
			return nil, domain.ErrRoomFull
		} else if err != nil {
			return nil, err
		}
		roles["guest"] = playerID
		rows, err := tx.Query(ctx, "INSERT INTO room_presence(room_code,player_id,generation,deadline,refreshed_at) SELECT room_code,player_id,1,$2,$3 FROM room_seats WHERE room_code=$1 ON CONFLICT(room_code,player_id) DO UPDATE SET generation=room_presence.generation+1,deadline=EXCLUDED.deadline,refreshed_at=EXCLUDED.refreshed_at RETURNING player_id,generation", code, window.Deadline, window.ObservedAt)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var leases []application.PresenceLease
		for rows.Next() {
			var lease application.PresenceLease
			if err = rows.Scan(&lease.PlayerID, &lease.Generation); err != nil {
				return nil, err
			}
			lease.RoomCode, lease.Round = code, aggregate.State().Round
			lease.Deadline, lease.EvaluateAt, lease.ProofAfter, lease.Active = window.Deadline, window.EvaluateAt, window.ProofAfter, true
			leases = append(leases, lease)
		}
		return leases, rows.Err()
	})
}

func (r *Repository) SubmitMoveCommand(ctx context.Context, code, accountID string, move domain.Move, command application.Command) (application.CommandResult, error) {
	if !domain.IsValidMove(move) {
		return application.CommandResult{}, fmt.Errorf("%w: %q", domain.ErrInvalidMove, move)
	}
	return r.recordRoomAction(ctx, code, accountID, command, "active", 0, func(tx pgx.Tx, round uint64, role string) error {
		result, err := tx.Exec(latency.WithQueryName(ctx, "room_action_insert"), `
			INSERT INTO room_moves(room_code,round_number,role,move)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, code, round, role, move)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return domain.ErrDuplicateMove
		}
		return nil
	})
}

func (r *Repository) RequestNextRoundCommand(ctx context.Context, code, accountID string, round uint64, command application.Command) (application.CommandResult, error) {
	return r.recordRoomAction(ctx, code, accountID, command, "resolved", round, func(tx pgx.Tx, currentRound uint64, role string) error {
		result, err := tx.Exec(latency.WithQueryName(ctx, "room_action_insert"), `
			INSERT INTO room_next_round_requests(room_code,round_number,role)
			VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, code, currentRound, role)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return domain.ErrNextRoundRequested
		}
		return nil
	})
}

func (r *Repository) recordRoomAction(ctx context.Context, code, accountID string, command application.Command, requiredRoundStatus string, expectedRound uint64, insert func(pgx.Tx, uint64, string) error) (application.CommandResult, error) {
	finishPhase := latency.StartPhase(ctx, latency.PhasePoolBeginTransaction)
	tx, err := r.pool.Begin(ctx)
	finishPhase()
	if err != nil {
		return application.CommandResult{}, err
	}
	defer tx.Rollback(ctx)
	finishPhase = latency.StartPhase(ctx, latency.PhaseIdempotencyLockReceipt)
	replay, found, receiptErr := commandReceipt(ctx, tx, accountID, command)
	finishPhase()
	if receiptErr != nil || found {
		return replay, receiptErr
	}
	var currentRound uint64
	var roomStatus, roundStatus string
	var roundReady bool
	finishPhase = latency.StartPhase(ctx, latency.PhaseRoomLock)
	err = tx.QueryRow(latency.WithQueryName(ctx, "round_shared_lock"), `
		SELECT r.current_round,r.status,rr.status,
		       rr.funded AND (SELECT count(*) = 2 FROM room_seats s WHERE s.room_code=r.code)
		FROM room_rooms r JOIN room_rounds rr
		  ON rr.room_code=r.code AND rr.number=r.current_round
		WHERE r.code=$1
		FOR SHARE OF rr`, code).Scan(&currentRound, &roomStatus, &roundStatus, &roundReady)
	finishPhase()
	if errors.Is(err, pgx.ErrNoRows) {
		return application.CommandResult{}, application.ErrRoomNotFound
	}
	if err != nil {
		return application.CommandResult{}, err
	}
	if roomStatus == "closed" {
		return application.CommandResult{}, domain.ErrRoomClosed
	}
	if expectedRound != 0 && expectedRound != currentRound {
		return application.CommandResult{}, domain.ErrStaleRound
	}
	if roundStatus != requiredRoundStatus {
		if requiredRoundStatus == "active" {
			return application.CommandResult{}, domain.ErrRoundResolved
		}
		return application.CommandResult{}, domain.ErrRoundNotResolved
	}
	var role string
	if err = tx.QueryRow(ctx, "SELECT role FROM room_seats WHERE room_code=$1 AND auth_user_id=$2", code, accountID).Scan(&role); errors.Is(err, pgx.ErrNoRows) {
		return application.CommandResult{}, application.ErrUnauthorized
	} else if err != nil {
		return application.CommandResult{}, err
	}
	if !roundReady {
		return application.CommandResult{}, domain.ErrRoomNotReady
	}
	finishPhase = latency.StartPhase(ctx, latency.PhasePersistenceWalletSettlement)
	if err = insert(tx, currentRound, role); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	var revision uint64
	err = tx.QueryRow(ctx, "UPDATE room_rooms SET revision=revision+1,updated_at=now() WHERE code=$1 RETURNING revision", code).Scan(&revision)
	finishPhase()
	if err != nil {
		return application.CommandResult{}, err
	}
	result := application.CommandResult{Snapshot: application.Snapshot{State: domain.State{Code: code, Round: currentRound, Ready: true, Resolved: requiredRoundStatus == "resolved"}, Revision: revision}}
	finishPhase = latency.StartPhase(ctx, latency.PhaseOutboxReceipt)
	if err = persistCommandEffects(ctx, tx, code, revision, accountID, command, result); err == nil {
		err = notifyRoomStateChanged(ctx, tx, code, currentRound)
	}
	finishPhase()
	if err != nil {
		return application.CommandResult{}, err
	}
	finishPhase = latency.StartPhase(ctx, latency.PhaseCommit)
	err = tx.Commit(ctx)
	finishPhase()
	if err != nil {
		return application.CommandResult{}, err
	}
	// The durable worker remains the progress guarantee. This best-effort pass
	// only reduces normal response-to-update latency and runs after action commit.
	reconcileContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	_, _ = r.ReconcileRoom(reconcileContext, worker.RoomHint{RoomCode: code, Round: currentRound})
	cancel()
	return result, nil
}

func (r *Repository) MutateCommand(ctx context.Context, code, accountID string, mutation application.Mutation, command application.Command) (application.CommandResult, error) {
	return r.writeAccountCommand(ctx, code, accountID, command, true, func(_ pgx.Tx, aggregate *domain.Room, _ map[string]string, playerID string) ([]application.PresenceLease, error) {
		finishMutation := latency.StartPhase(ctx, latency.PhaseDomainMutation)
		err := mutation(aggregate, playerID)
		finishMutation()
		return nil, err
	})
}

func (r *Repository) writeAccountCommand(ctx context.Context, code, accountID string, command application.Command, authorize bool, change func(pgx.Tx, *domain.Room, map[string]string, string) ([]application.PresenceLease, error)) (application.CommandResult, error) {
	finishPhase := latency.StartPhase(ctx, latency.PhasePoolBeginTransaction)
	tx, err := r.pool.Begin(ctx)
	finishPhase()
	if err != nil {
		return application.CommandResult{}, err
	}
	defer tx.Rollback(ctx)
	finishPhase = latency.StartPhase(ctx, latency.PhaseIdempotencyLockReceipt)
	replay, found, receiptErr := commandReceipt(ctx, tx, accountID, command)
	finishPhase()
	if receiptErr != nil || found {
		return replay, receiptErr
	}
	var revision uint64
	finishPhase = latency.StartPhase(ctx, latency.PhaseRoomLock)
	if err = tx.QueryRow(latency.WithQueryName(ctx, "room_lock_wait"), "SELECT revision FROM room_rooms WHERE code=$1 FOR UPDATE", code).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		finishPhase()
		return application.CommandResult{}, application.ErrRoomNotFound
	} else if err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	finishPhase()
	finishPhase = latency.StartPhase(ctx, latency.PhaseAggregateLoad)
	aggregate, roles, err := load(ctx, tx, code)
	if err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	var playerID string
	if authorize {
		if err = tx.QueryRow(ctx, "SELECT player_id FROM room_seats WHERE room_code=$1 AND auth_user_id=$2", code, accountID).Scan(&playerID); errors.Is(err, pgx.ErrNoRows) {
			finishPhase()
			return application.CommandResult{}, application.ErrUnauthorized
		} else if err != nil {
			finishPhase()
			return application.CommandResult{}, err
		}
	}
	finishPhase()
	previousRound := aggregate.PersistenceState().Round
	leases, err := change(tx, aggregate, roles, playerID)
	if err != nil {
		return application.CommandResult{}, err
	}
	revision++
	finishPhase = latency.StartPhase(ctx, latency.PhasePersistenceWalletSettlement)
	if err = save(ctx, tx, aggregate, roles, previousRound, revision); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	finishPhase()
	result := application.CommandResult{Snapshot: application.Snapshot{State: aggregate.State(), Revision: revision}, Leases: leases}
	finishPhase = latency.StartPhase(ctx, latency.PhaseOutboxReceipt)
	if err = persistCommandEffects(ctx, tx, code, revision, accountID, command, result); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	finishPhase()
	finishPhase = latency.StartPhase(ctx, latency.PhaseCommit)
	if err = tx.Commit(ctx); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	finishPhase()
	return result, nil
}

func (r *Repository) RechargeCommand(ctx context.Context, accountID string, amount int64, command application.Command) (application.CommandResult, error) {
	finishPhase := latency.StartPhase(ctx, latency.PhasePoolBeginTransaction)
	tx, err := r.pool.Begin(ctx)
	finishPhase()
	if err != nil {
		return application.CommandResult{}, err
	}
	defer tx.Rollback(ctx)
	finishPhase = latency.StartPhase(ctx, latency.PhaseIdempotencyLockReceipt)
	replay, found, receiptErr := commandReceipt(ctx, tx, accountID, command)
	finishPhase()
	if receiptErr != nil || found {
		return replay, receiptErr
	}
	finishPhase = latency.StartPhase(ctx, latency.PhasePersistenceWalletSettlement)
	defer finishPhase()
	if err = ensureUserWallet(ctx, tx, accountID); err != nil {
		return application.CommandResult{}, err
	}
	userAccount := userAccountID(accountID)
	if err = lockWallets(ctx, tx, []string{"mint", userAccount}); err != nil {
		return application.CommandResult{}, err
	}
	var transactionID int64
	businessKey := "recharge:" + accountID + ":" + command.Key
	if err = tx.QueryRow(ctx, "INSERT INTO wallet_transactions(business_key,transaction_type,auth_user_id,amount) VALUES($1,'recharge',$2,$3) RETURNING transaction_id", businessKey, accountID, amount).Scan(&transactionID); err != nil {
		return application.CommandResult{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO wallet_postings(transaction_id,account_id,amount) VALUES($1,'mint',$2),($1,$3,$4)", transactionID, -amount, userAccount, amount); err != nil {
		return application.CommandResult{}, err
	}
	var balance int64
	if err = tx.QueryRow(ctx, "UPDATE wallet_accounts SET balance=balance+$2 WHERE account_id=$1 RETURNING balance", userAccount, amount).Scan(&balance); err != nil {
		return application.CommandResult{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE wallet_accounts SET balance=balance-$1 WHERE account_id='mint'", amount); err != nil {
		return application.CommandResult{}, err
	}
	result := application.CommandResult{Balance: balance}
	if err = finishCommandReceipt(ctx, tx, accountID, command, result); err != nil {
		return application.CommandResult{}, err
	}
	finishPhase()
	finishPhase = latency.StartPhase(ctx, latency.PhaseCommit)
	if err = tx.Commit(ctx); err != nil {
		finishPhase()
		return application.CommandResult{}, err
	}
	finishPhase()
	return result, nil
}

func (r *Repository) SnapshotForAccount(ctx context.Context, code, accountID string) (application.Snapshot, error) {
	if accountID == "" {
		return application.Snapshot{}, application.ErrUnauthorized
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return application.Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	var revision uint64
	if err = tx.QueryRow(ctx, "SELECT r.revision FROM room_rooms r JOIN room_seats s ON s.room_code=r.code WHERE r.code=$1 AND s.auth_user_id=$2", code, accountID).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return application.Snapshot{}, application.ErrUnauthorized
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

func (r *Repository) CleanupCommandReceipts(ctx context.Context, now time.Time) (int64, error) {
	result, err := r.pool.Exec(ctx, "DELETE FROM command_receipts WHERE expires_at <= $1", now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

func commandReceipt(ctx context.Context, tx pgx.Tx, accountID string, command application.Command) (application.CommandResult, bool, error) {
	if _, err := tx.Exec(latency.WithQueryName(ctx, "idempotency_lock_wait"), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", command.Key); err != nil {
		return application.CommandResult{}, false, err
	}
	var owner, operation string
	var requestHash, body []byte
	err := tx.QueryRow(ctx, "SELECT account_id::text,operation,request_hash,response_body FROM command_receipts WHERE idempotency_key=$1", command.Key).Scan(&owner, &operation, &requestHash, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.CommandResult{}, false, nil
	}
	if err != nil {
		return application.CommandResult{}, false, err
	}
	if owner != accountID || operation != command.Operation || !bytes.Equal(requestHash, command.RequestHash[:]) {
		return application.CommandResult{}, false, application.ErrIdempotencyConflict
	}
	var result application.CommandResult
	if err = json.Unmarshal(body, &result); err != nil {
		return application.CommandResult{}, false, err
	}
	result.Replayed = true
	return result, true, nil
}

func persistCommandEffects(ctx context.Context, tx pgx.Tx, code string, revision uint64, accountID string, command application.Command, result application.CommandResult) error {
	if _, err := tx.Exec(ctx, "INSERT INTO room_outbox(room_code,revision) VALUES($1,$2)", code, revision); err != nil {
		return err
	}
	return finishCommandReceipt(ctx, tx, accountID, command, result)
}

func finishCommandReceipt(ctx context.Context, tx pgx.Tx, accountID string, command application.Command, result application.CommandResult) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO command_receipts(account_id,idempotency_key,operation,request_hash,response_status,response_headers,response_body,expires_at) VALUES($1,$2,$3,$4,$5,'{}'::jsonb,$6,now()+$7::interval)", accountID, command.Key, command.Operation, command.RequestHash[:], command.ResponseStatus, body, "7 days")
	return err
}
