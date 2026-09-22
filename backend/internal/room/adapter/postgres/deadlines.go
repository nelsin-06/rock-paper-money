package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) OpenConnection(ctx context.Context, lease application.ConnectionLease) (application.PresenceTransition, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return application.PresenceTransition{}, err
	}
	defer tx.Rollback(ctx)
	if !lease.LeaseExpiresAt.After(lease.ConnectedAt) {
		return application.PresenceTransition{}, errors.New("connection lease must expire after connection time")
	}
	var round uint64
	if err = tx.QueryRow(ctx, "SELECT current_round FROM room_rooms WHERE code=$1 FOR UPDATE", lease.RoomCode).Scan(&round); errors.Is(err, pgx.ErrNoRows) {
		return application.PresenceTransition{}, application.ErrRoomNotFound
	} else if err != nil {
		return application.PresenceTransition{}, err
	}
	var role, playerID string
	if err = tx.QueryRow(ctx, `
		SELECT s.role,s.player_id
		FROM room_seats s
		JOIN web_sessions ws ON ws.account_id=s.auth_user_id
		WHERE s.room_code=$1 AND s.auth_user_id=$2 AND ws.session_digest=$3
		  AND ws.revoked_at IS NULL AND ws.idle_expires_at>$4 AND ws.absolute_expires_at>$4`,
		lease.RoomCode, lease.AccountID, lease.SessionDigest, lease.ConnectedAt).Scan(&role, &playerID); errors.Is(err, pgx.ErrNoRows) {
		return application.PresenceTransition{}, application.ErrUnauthorized
	} else if err != nil {
		return application.PresenceTransition{}, err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO room_connections(connection_id,session_digest,room_code,role,connected_at,lease_expires_at)
		VALUES($1,$2,$3,$4,$5,$6)`, lease.ID, lease.SessionDigest, lease.RoomCode, role, lease.ConnectedAt, lease.LeaseExpiresAt); isUnique(err) {
		return application.PresenceTransition{}, errors.New("connection identifier already exists")
	} else if err != nil {
		return application.PresenceTransition{}, err
	}
	var generation uint64
	var present bool
	presenceChanged := false
	err = tx.QueryRow(ctx, "SELECT generation,present FROM room_presence WHERE room_code=$1 AND player_id=$2 FOR UPDATE", lease.RoomCode, playerID).Scan(&generation, &present)
	if errors.Is(err, pgx.ErrNoRows) {
		generation = 1
		if _, err = tx.Exec(ctx, "INSERT INTO room_presence(room_code,player_id,generation,deadline,refreshed_at,present) VALUES($1,$2,$3,$4,$5,true)", lease.RoomCode, playerID, generation, lease.LeaseExpiresAt, lease.ConnectedAt); err != nil {
			return application.PresenceTransition{}, err
		}
		presenceChanged = true
	} else if err != nil {
		return application.PresenceTransition{}, err
	} else {
		if !present {
			presenceChanged = true
			if _, err = tx.Exec(ctx, "UPDATE room_deadlines SET status='cancelled',completed_at=$4 WHERE room_code=$1 AND round_number=$2 AND kind='disconnect' AND role=$3 AND generation=$5 AND status='pending'", lease.RoomCode, round, role, lease.ConnectedAt, generation); err != nil {
				return application.PresenceTransition{}, err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE room_presence SET present=true,deadline=$3,refreshed_at=$4,disconnected_at=NULL WHERE room_code=$1 AND player_id=$2", lease.RoomCode, playerID, lease.LeaseExpiresAt, lease.ConnectedAt); err != nil {
			return application.PresenceTransition{}, err
		}
	}
	if presenceChanged {
		if _, err = advancePresenceRevision(ctx, tx, lease.RoomCode, lease.ConnectedAt); err != nil {
			return application.PresenceTransition{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return application.PresenceTransition{}, err
	}
	return application.PresenceTransition{RoomCode: lease.RoomCode, Role: role, Generation: generation}, nil
}

func (r *Repository) RenewConnection(ctx context.Context, connectionID string, observedAt, leaseExpiresAt time.Time) error {
	if !leaseExpiresAt.After(observedAt) {
		return errors.New("connection lease must expire after observation time")
	}
	result, err := r.pool.Exec(ctx, `
		UPDATE room_connections c SET lease_expires_at=$3
		FROM web_sessions ws, room_seats s
		WHERE c.connection_id=$1 AND c.closed_at IS NULL AND c.session_digest=ws.session_digest
		  AND s.room_code=c.room_code AND s.role=c.role AND s.auth_user_id=ws.account_id
		  AND ws.revoked_at IS NULL AND ws.idle_expires_at>$2 AND ws.absolute_expires_at>$2`, connectionID, observedAt, leaseExpiresAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return application.ErrUnauthorized
	}
	return nil
}

func (r *Repository) CloseConnection(ctx context.Context, connectionID string, disappearedAt time.Time) (application.PresenceTransition, error) {
	return r.closeConnection(ctx, connectionID, disappearedAt, nil)
}

func (r *Repository) closeConnection(ctx context.Context, connectionID string, disappearedAt time.Time, leaseExpiredBy *time.Time) (application.PresenceTransition, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return application.PresenceTransition{}, err
	}
	defer tx.Rollback(ctx)
	var roomCode, role, playerID string
	if err = tx.QueryRow(ctx, `
		SELECT c.room_code,c.role,s.player_id
		FROM room_connections c JOIN room_seats s ON s.room_code=c.room_code AND s.role=c.role
		WHERE c.connection_id=$1`, connectionID).Scan(&roomCode, &role, &playerID); errors.Is(err, pgx.ErrNoRows) {
		return application.PresenceTransition{}, nil
	} else if err != nil {
		return application.PresenceTransition{}, err
	}
	var round uint64
	if err = tx.QueryRow(ctx, "SELECT current_round FROM room_rooms WHERE code=$1 FOR UPDATE", roomCode).Scan(&round); err != nil {
		return application.PresenceTransition{}, err
	}
	query := "UPDATE room_connections SET closed_at=$2 WHERE connection_id=$1 AND closed_at IS NULL"
	arguments := []any{connectionID, disappearedAt}
	if leaseExpiredBy != nil {
		query += " AND lease_expires_at<=$3"
		arguments = append(arguments, *leaseExpiredBy)
	}
	result, err := tx.Exec(ctx, query, arguments...)
	if err != nil {
		return application.PresenceTransition{}, err
	}
	if result.RowsAffected() == 0 {
		if err = tx.Commit(ctx); err != nil {
			return application.PresenceTransition{}, err
		}
		return application.PresenceTransition{}, nil
	}
	var active bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM room_connections WHERE room_code=$1 AND role=$2 AND closed_at IS NULL AND lease_expires_at>$3)", roomCode, role, disappearedAt).Scan(&active); err != nil {
		return application.PresenceTransition{}, err
	}
	transition := application.PresenceTransition{RoomCode: roomCode, Role: role}
	if !active {
		var generation uint64
		var wasPresent bool
		if err = tx.QueryRow(ctx, "SELECT generation,present FROM room_presence WHERE room_code=$1 AND player_id=$2 FOR UPDATE", roomCode, playerID).Scan(&generation, &wasPresent); err != nil {
			return application.PresenceTransition{}, err
		}
		transition.Generation = generation
		if wasPresent {
			generation++
			transition.Generation = generation
			transition.LastSocket = true
			transition.GraceDueAt = disappearedAt.Add(application.DisconnectGracePeriod)
			if _, err = tx.Exec(ctx, "UPDATE room_presence SET present=false,generation=$3,deadline=$4,disconnected_at=$5 WHERE room_code=$1 AND player_id=$2", roomCode, playerID, generation, transition.GraceDueAt, disappearedAt); err != nil {
				return application.PresenceTransition{}, err
			}
			if _, err = tx.Exec(ctx, `
				INSERT INTO room_deadlines(room_code,round_number,kind,role,generation,due_at)
				VALUES($1,$2,'disconnect',$3,$4,$5) ON CONFLICT DO NOTHING`, roomCode, round, role, generation, transition.GraceDueAt); err != nil {
				return application.PresenceTransition{}, err
			}
			if _, err = advancePresenceRevision(ctx, tx, roomCode, disappearedAt); err != nil {
				return application.PresenceTransition{}, err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return application.PresenceTransition{}, err
	}
	return transition, nil
}

func (r *Repository) ReapExpiredConnections(ctx context.Context, now time.Time) (int64, error) {
	rows, err := r.pool.Query(ctx, "SELECT connection_id::text,lease_expires_at FROM room_connections WHERE closed_at IS NULL AND lease_expires_at<=$1 ORDER BY lease_expires_at,connection_id", now)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id string
		at time.Time
	}
	var expiredConnections []expired
	for rows.Next() {
		var connection expired
		if err = rows.Scan(&connection.id, &connection.at); err != nil {
			rows.Close()
			return 0, err
		}
		expiredConnections = append(expiredConnections, connection)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	var reaped int64
	for _, connection := range expiredConnections {
		transition, closeErr := r.closeConnection(ctx, connection.id, connection.at, &now)
		if closeErr != nil {
			return reaped, closeErr
		}
		if transition.RoomCode != "" {
			reaped++
		}
	}
	return reaped, nil
}

func (r *Repository) ProcessNextDeadline(ctx context.Context, now time.Time) (application.DeadlineResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return application.DeadlineResult{}, err
	}
	defer tx.Rollback(ctx)
	var candidateID int64
	var roomCode string
	var candidateRound uint64
	var candidateDueAt time.Time
	if err = tx.QueryRow(ctx, `
		SELECT deadline_id,room_code,round_number,due_at FROM room_deadlines
		WHERE status='pending' AND due_at<=$1
		ORDER BY due_at,CASE kind WHEN 'disconnect' THEN 0 ELSE 1 END,deadline_id
		LIMIT 1`, now).Scan(&candidateID, &roomCode, &candidateRound, &candidateDueAt); errors.Is(err, pgx.ErrNoRows) {
		return application.DeadlineResult{}, nil
	} else if err != nil {
		return application.DeadlineResult{}, err
	}
	claim, err := tx.Exec(ctx, `
		UPDATE room_rounds rr SET status='settling'
		WHERE rr.room_code=$1 AND rr.number=$2 AND rr.status='active' AND rr.funded
		  AND EXISTS (SELECT 1 FROM room_rooms r WHERE r.code=rr.room_code AND r.current_round=rr.number AND r.status='active')`, roomCode, candidateRound)
	if err != nil {
		return application.DeadlineResult{}, err
	}
	if claim.RowsAffected() == 0 {
		if _, err = tx.Exec(ctx, `
			UPDATE room_deadlines d SET status='cancelled',completed_at=$2
			WHERE d.deadline_id=$1 AND NOT EXISTS (
				SELECT 1 FROM room_rounds rr JOIN room_rooms r ON r.code=rr.room_code
				WHERE rr.room_code=d.room_code AND rr.number=d.round_number
				  AND r.current_round=rr.number AND r.status='active' AND rr.status='active')`, candidateID, now); err != nil {
			return application.DeadlineResult{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return application.DeadlineResult{}, err
		}
		return application.DeadlineResult{Processed: true, Stale: true, RoomCode: roomCode, Round: candidateRound, DueAt: candidateDueAt}, nil
	}
	var round uint64
	if err = tx.QueryRow(ctx, "SELECT current_round FROM room_rooms WHERE code=$1 FOR UPDATE", roomCode).Scan(&round); errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, "UPDATE room_deadlines SET status='cancelled',completed_at=$2 WHERE deadline_id=$1", candidateID, now); err != nil {
			return application.DeadlineResult{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return application.DeadlineResult{}, err
		}
		return application.DeadlineResult{Processed: true, Stale: true, RoomCode: roomCode, DueAt: candidateDueAt}, nil
	} else if err != nil {
		return application.DeadlineResult{}, err
	}
	aggregate, roles, err := load(ctx, tx, roomCode)
	if err != nil {
		return application.DeadlineResult{}, err
	}
	deadline, found, err := nextApplicableDeadline(ctx, tx, roomCode, round, aggregate, now)
	if err != nil {
		return application.DeadlineResult{}, err
	}
	if !found {
		if _, err = tx.Exec(ctx, "UPDATE room_rounds SET status='active' WHERE room_code=$1 AND number=$2 AND status='settling'", roomCode, round); err != nil {
			return application.DeadlineResult{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return application.DeadlineResult{}, err
		}
		return application.DeadlineResult{Processed: true, Stale: true, RoomCode: roomCode, Round: round, DueAt: candidateDueAt}, nil
	}
	if deadline.kind == application.DeadlineDisconnect {
		present, presentErr := activeRoles(ctx, tx, roomCode, now)
		if presentErr != nil {
			return application.DeadlineResult{}, presentErr
		}
		otherRole := "host"
		if deadline.role == "host" {
			otherRole = "guest"
		}
		if present[deadline.role] {
			if _, err = tx.Exec(ctx, "UPDATE room_deadlines SET status='cancelled',completed_at=$2 WHERE deadline_id=$1", deadline.id, now); err != nil {
				return application.DeadlineResult{}, err
			}
			if _, err = tx.Exec(ctx, "UPDATE room_rounds SET status='active' WHERE room_code=$1 AND number=$2 AND status='settling'", roomCode, round); err != nil {
				return application.DeadlineResult{}, err
			}
			if err = tx.Commit(ctx); err != nil {
				return application.DeadlineResult{}, err
			}
			return application.DeadlineResult{Processed: true, Stale: true, RoomCode: roomCode, Round: round, Kind: deadline.kind, DueAt: deadline.dueAt}, nil
		}
		if present[otherRole] {
			err = aggregate.Forfeit(roles[deadline.role], round)
		} else {
			err = aggregate.ResolveDeadlineDraw(round)
		}
	} else {
		err = aggregate.ResolveInactivity(round)
	}
	if err != nil {
		if errors.Is(err, domain.ErrRoundResolved) || errors.Is(err, domain.ErrStaleRound) || errors.Is(err, domain.ErrRoomClosed) {
			if _, err = tx.Exec(ctx, "UPDATE room_deadlines SET status='cancelled',completed_at=$2 WHERE deadline_id=$1", deadline.id, now); err != nil {
				return application.DeadlineResult{}, err
			}
			if _, err = tx.Exec(ctx, "UPDATE room_rounds SET status='active' WHERE room_code=$1 AND number=$2 AND status='settling'", roomCode, round); err != nil {
				return application.DeadlineResult{}, err
			}
			if err = tx.Commit(ctx); err != nil {
				return application.DeadlineResult{}, err
			}
			return application.DeadlineResult{Processed: true, Stale: true, RoomCode: roomCode, Round: round, Kind: deadline.kind, DueAt: deadline.dueAt}, nil
		}
		return application.DeadlineResult{}, err
	}
	var revision uint64
	if err = tx.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1", roomCode).Scan(&revision); err != nil {
		return application.DeadlineResult{}, err
	}
	revision++
	if err = save(ctx, tx, aggregate, roles, round, revision); err != nil {
		return application.DeadlineResult{}, err
	}
	if err = settleRound(ctx, tx, roomCode, aggregate.State()); err != nil {
		return application.DeadlineResult{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_outbox(room_code,revision) VALUES($1,$2) ON CONFLICT DO NOTHING", roomCode, revision); err != nil {
		return application.DeadlineResult{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE room_deadlines SET status=CASE WHEN deadline_id=$3 THEN 'completed' ELSE 'cancelled' END,completed_at=$2 WHERE room_code=$1 AND round_number=$4 AND status='pending'", roomCode, now, deadline.id, round); err != nil {
		return application.DeadlineResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return application.DeadlineResult{}, err
	}
	return application.DeadlineResult{Processed: true, Settled: true, RoomCode: roomCode, Round: round, Kind: deadline.kind, DueAt: deadline.dueAt}, nil
}

type pendingDeadline struct {
	id         int64
	kind       application.DeadlineKind
	role       string
	generation uint64
	dueAt      time.Time
}

func nextApplicableDeadline(ctx context.Context, tx pgx.Tx, roomCode string, round uint64, aggregate *domain.Room, now time.Time) (pendingDeadline, bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT deadline_id,kind,COALESCE(role,''),generation,due_at
		FROM room_deadlines WHERE room_code=$1 AND status='pending' AND due_at<=$2
		ORDER BY due_at,CASE kind WHEN 'disconnect' THEN 0 ELSE 1 END,deadline_id FOR UPDATE`, roomCode, now)
	if err != nil {
		return pendingDeadline{}, false, err
	}
	defer rows.Close()
	var deadlines []pendingDeadline
	for rows.Next() {
		var deadline pendingDeadline
		if err = rows.Scan(&deadline.id, &deadline.kind, &deadline.role, &deadline.generation, &deadline.dueAt); err != nil {
			return pendingDeadline{}, false, err
		}
		deadlines = append(deadlines, deadline)
	}
	if err = rows.Err(); err != nil {
		return pendingDeadline{}, false, err
	}
	rows.Close()
	for _, deadline := range deadlines {
		applicable := aggregate.State().Round == round && !aggregate.State().Resolved && !aggregate.State().Closed
		var funded bool
		if applicable {
			if err = tx.QueryRow(ctx, "SELECT funded FROM room_rounds WHERE room_code=$1 AND number=$2", roomCode, round).Scan(&funded); err != nil {
				return pendingDeadline{}, false, err
			}
			applicable = funded
		}
		if applicable && deadline.kind == application.DeadlineDisconnect {
			var generation uint64
			var present bool
			err = tx.QueryRow(ctx, `
				SELECT p.generation,p.present FROM room_presence p
				JOIN room_seats s ON s.room_code=p.room_code AND s.player_id=p.player_id
				WHERE p.room_code=$1 AND s.role=$2`, roomCode, deadline.role).Scan(&generation, &present)
			applicable = err == nil && generation == deadline.generation && !present
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return pendingDeadline{}, false, err
			}
		}
		if applicable {
			return deadline, true, nil
		}
		if _, err = tx.Exec(ctx, "UPDATE room_deadlines SET status='cancelled',completed_at=$2 WHERE deadline_id=$1", deadline.id, now); err != nil {
			return pendingDeadline{}, false, err
		}
	}
	return pendingDeadline{}, false, nil
}

func activeRoles(ctx context.Context, tx pgx.Tx, roomCode string, now time.Time) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT role,EXISTS(
			SELECT 1 FROM room_connections active
			WHERE active.room_code=$1 AND active.role=seat.role AND active.closed_at IS NULL AND active.lease_expires_at>$2
		) FROM room_seats seat WHERE room_code=$1`, roomCode, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var role string
		var active bool
		if err = rows.Scan(&role, &active); err != nil {
			return nil, err
		}
		present[role] = active
	}
	return present, rows.Err()
}

func (r *Repository) CleanupDeadlines(ctx context.Context, before time.Time) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	deadlineResult, err := tx.Exec(ctx, "DELETE FROM room_deadlines WHERE status IN ('completed','cancelled') AND completed_at<$1", before)
	if err != nil {
		return 0, err
	}
	connectionResult, err := tx.Exec(ctx, "DELETE FROM room_connections WHERE closed_at IS NOT NULL AND closed_at<$1", before)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return deadlineResult.RowsAffected() + connectionResult.RowsAffected(), nil
}

func scheduleInactivityDeadline(ctx context.Context, tx pgx.Tx, code string, round uint64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO room_deadlines(room_code,round_number,kind,generation,due_at)
		VALUES($1,$2,'inactivity',$2,now()+$3::interval) ON CONFLICT DO NOTHING`, code, round, fmt.Sprintf("%d seconds", int(application.FundedInactivityDeadline.Seconds())))
	return err
}

func advancePresenceRevision(ctx context.Context, tx pgx.Tx, roomCode string, changedAt time.Time) (uint64, error) {
	var revision uint64
	if err := tx.QueryRow(ctx, "UPDATE room_rooms SET revision=revision+1,updated_at=$2 WHERE code=$1 RETURNING revision", roomCode, changedAt).Scan(&revision); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO room_outbox(room_code,revision) VALUES($1,$2)", roomCode, revision); err != nil {
		return 0, err
	}
	return revision, nil
}
