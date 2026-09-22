package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresSessionStore struct {
	pool *pgxpool.Pool
}

func NewPostgresSessionStore(pool *pgxpool.Pool) *PostgresSessionStore {
	return &PostgresSessionStore{pool: pool}
}

func (s *PostgresSessionStore) CreateSession(ctx context.Context, session Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO web_sessions(
			session_digest, account_id, csrf_digest, created_at, last_seen_at,
			idle_expires_at, absolute_expires_at, revoked_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		session.Digest[:], session.AccountID, session.CSRFDigest[:], session.CreatedAt,
		session.LastSeenAt, session.IdleExpiresAt, session.AbsoluteExpiresAt, session.RevokedAt)
	return err
}

func (s *PostgresSessionStore) UseSession(ctx context.Context, digest Digest, now, idleExpiresAt time.Time) (Session, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE web_sessions
		SET last_seen_at=$2, idle_expires_at=LEAST($3, absolute_expires_at)
		WHERE session_digest=$1
		  AND revoked_at IS NULL
		  AND idle_expires_at>$2
		  AND absolute_expires_at>$2
		RETURNING session_digest, csrf_digest, account_id::text, created_at, last_seen_at,
		          idle_expires_at, absolute_expires_at, revoked_at`, digest[:], now, idleExpiresAt)
	return scanSession(row)
}

func (s *PostgresSessionStore) RevokeSession(ctx context.Context, digest Digest, revokedAt time.Time) error {
	result, err := s.pool.Exec(ctx, `
		UPDATE web_sessions
		SET revoked_at=COALESCE(revoked_at,$2)
		WHERE session_digest=$1`, digest[:], revokedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrInvalidSession
	}
	return nil
}

func (s *PostgresSessionStore) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.pool.Exec(ctx, `
		DELETE FROM web_sessions
		WHERE revoked_at IS NOT NULL OR idle_expires_at<=$1 OR absolute_expires_at<=$1`, now)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}

func scanSession(row pgx.Row) (Session, error) {
	var session Session
	var sessionDigest, csrfDigest []byte
	err := row.Scan(&sessionDigest, &csrfDigest, &session.AccountID, &session.CreatedAt,
		&session.LastSeenAt, &session.IdleExpiresAt, &session.AbsoluteExpiresAt, &session.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrInvalidSession
	}
	if err != nil {
		return Session{}, err
	}
	if len(sessionDigest) != len(session.Digest) || len(csrfDigest) != len(session.CSRFDigest) {
		return Session{}, ErrInvalidSession
	}
	copy(session.Digest[:], sessionDigest)
	copy(session.CSRFDigest[:], csrfDigest)
	return session, nil
}
