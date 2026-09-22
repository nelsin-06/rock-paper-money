package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/rock-paper-money/internal/latency"
	"example.com/rock-paper-money/internal/worker"
	"github.com/jackc/pgx/v5"
)

const roomStateChangedChannel = "room_state_changed"

type reconciliationCandidate struct {
	hint      worker.RoomHint
	createdAt time.Time
}

func (r *Repository) ReconcileBatch(ctx context.Context, limit int) (worker.ReconciliationReport, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := r.pool.Query(ctx, `
		SELECT room_code,round_number,created_at,count(*) OVER(),min(created_at) OVER() FROM (
			SELECT rr.room_code,rr.number AS round_number,rr.created_at
			FROM room_rounds rr JOIN room_rooms r ON r.code=rr.room_code AND r.current_round=rr.number
			WHERE r.status='active' AND rr.status='active' AND rr.funded
			  AND (SELECT count(*) FROM room_moves m WHERE m.room_code=rr.room_code AND m.round_number=rr.number)=2
			UNION ALL
			SELECT rr.room_code,rr.number,rr.created_at
			FROM room_rounds rr JOIN room_rooms r ON r.code=rr.room_code AND r.current_round=rr.number
			WHERE r.status='active' AND rr.status='resolved'
			  AND (SELECT count(*) FROM room_next_round_requests n WHERE n.room_code=rr.room_code AND n.round_number=rr.number)=2
		) eligible
		ORDER BY created_at,room_code,round_number LIMIT $1`, limit)
	if err != nil {
		return worker.ReconciliationReport{}, err
	}
	var candidates []reconciliationCandidate
	var backlog int64
	var oldest time.Time
	for rows.Next() {
		var candidate reconciliationCandidate
		if err = rows.Scan(&candidate.hint.RoomCode, &candidate.hint.Round, &candidate.createdAt, &backlog, &oldest); err != nil {
			rows.Close()
			return worker.ReconciliationReport{}, err
		}
		candidates = append(candidates, candidate)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return worker.ReconciliationReport{}, err
	}
	rows.Close()
	report := worker.ReconciliationReport{EligibleBacklog: backlog}
	if len(candidates) > 0 {
		report.OldestEligibleAge = time.Since(oldest)
	}
	var errs []error
	for _, candidate := range candidates {
		candidateReport, reconcileErr := r.ReconcileRoom(ctx, candidate.hint)
		report.Add(candidateReport)
		if reconcileErr != nil {
			report.Failures++
			errs = append(errs, fmt.Errorf("reconcile room transition: %w", reconcileErr))
		}
	}
	return report, errors.Join(errs...)
}

func (r *Repository) ReconcileRoom(ctx context.Context, hint worker.RoomHint) (worker.ReconciliationReport, error) {
	report := worker.ReconciliationReport{ClaimAttempts: 1}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	transition, claimed, err := claimRoomTransition(ctx, tx, hint)
	if err != nil {
		return report, err
	}
	if !claimed {
		report.ClaimNoops = 1
		return report, tx.Commit(ctx)
	}
	report.ClaimWins = 1
	aggregate, roles, err := load(ctx, tx, hint.RoomCode)
	if err != nil {
		return report, err
	}
	previousRound := aggregate.PersistenceState().Round
	switch transition {
	case "resolve":
		if err = aggregate.ResolveCommittedMoves(hint.Round); err != nil {
			return report, err
		}
	case "advance":
		if err = aggregate.AdvanceCommittedNextRound(hint.Round); err != nil {
			return report, err
		}
	default:
		return report, errors.New("unknown reconciliation transition")
	}
	var revision uint64
	if err = tx.QueryRow(latency.WithQueryName(ctx, "room_lock_wait"), "SELECT revision FROM room_rooms WHERE code=$1 FOR UPDATE", hint.RoomCode).Scan(&revision); err != nil {
		return report, err
	}
	revision++
	if err = save(ctx, tx, aggregate, roles, previousRound, revision); err != nil {
		return report, err
	}
	if transition == "resolve" {
		if err = settleRound(ctx, tx, hint.RoomCode, aggregate.State()); err != nil {
			return report, err
		}
		if _, err = tx.Exec(ctx, "UPDATE room_deadlines SET status='cancelled',completed_at=now() WHERE room_code=$1 AND round_number=$2 AND status='pending'", hint.RoomCode, hint.Round); err != nil {
			return report, err
		}
		report.SettledRounds = 1
	} else {
		if err = fundRound(ctx, tx, hint.RoomCode, aggregate.State().Round, ""); err != nil {
			return report, err
		}
		if _, err = tx.Exec(ctx, "UPDATE room_rounds SET status='resolved' WHERE room_code=$1 AND number=$2 AND status='settling'", hint.RoomCode, hint.Round); err != nil {
			return report, err
		}
		report.AdvancedRounds = 1
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_outbox(room_code,revision) VALUES($1,$2)", hint.RoomCode, revision); err != nil {
		return report, err
	}
	if err = notifyRoomStateChanged(ctx, tx, hint.RoomCode, aggregate.State().Round); err != nil {
		return report, err
	}
	if err = tx.Commit(ctx); err != nil {
		return report, err
	}
	return report, nil
}

func claimRoomTransition(ctx context.Context, tx pgx.Tx, hint worker.RoomHint) (string, bool, error) {
	var claimed int
	err := tx.QueryRow(latency.WithQueryName(ctx, "round_transition_claim"), `
		UPDATE room_rounds rr SET status='settling'
		WHERE rr.room_code=$1 AND rr.number=$2 AND rr.status='active' AND rr.funded
		  AND EXISTS (SELECT 1 FROM room_rooms r WHERE r.code=rr.room_code AND r.current_round=rr.number AND r.status='active')
		  AND (SELECT count(*) FROM room_moves m WHERE m.room_code=rr.room_code AND m.round_number=rr.number)=2
		RETURNING 1`, hint.RoomCode, hint.Round).Scan(&claimed)
	if err == nil {
		return "resolve", true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	err = tx.QueryRow(latency.WithQueryName(ctx, "round_transition_claim"), `
		UPDATE room_rounds rr SET status='settling'
		WHERE rr.room_code=$1 AND rr.number=$2 AND rr.status='resolved'
		  AND EXISTS (SELECT 1 FROM room_rooms r WHERE r.code=rr.room_code AND r.current_round=rr.number AND r.status='active')
		  AND (SELECT count(*) FROM room_next_round_requests n WHERE n.room_code=rr.room_code AND n.round_number=rr.number)=2
		RETURNING 1`, hint.RoomCode, hint.Round).Scan(&claimed)
	if err == nil {
		return "advance", true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return "", false, err
}

func notifyRoomStateChanged(ctx context.Context, tx pgx.Tx, code string, round uint64) error {
	payload := fmt.Sprintf("%s:%d", code, round)
	if len(payload) > 128 {
		return errors.New("room notification hint exceeds safe bound")
	}
	_, err := tx.Exec(ctx, "SELECT pg_notify($1,$2)", roomStateChangedChannel, payload)
	return err
}
