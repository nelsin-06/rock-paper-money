package worker_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	roompostgres "example.com/rock-paper-money/internal/room/adapter/postgres"
	"example.com/rock-paper-money/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOutboxWorkerRetriesFailureAndUncertainAcknowledgement(t *testing.T) {
	now := time.Date(2026, 9, 15, 18, 0, 0, 0, time.UTC)
	store := &outboxStoreStub{records: []worker.OutboxRecord{{ID: 7, RoomID: "ABC123", Revision: 9, ClaimToken: "claim-1", Attempts: 1}}}
	publisher := &publisherStub{err: errors.New("redis unavailable")}
	dispatcher := worker.NewOutboxWorker(store, publisher, worker.OutboxConfig{Now: func() time.Time { return now }, RetryBase: time.Second})

	report, err := dispatcher.RunOnce(context.Background())
	if !errors.Is(err, publisher.err) || report.Claimed != 1 || report.Published != 0 || report.Attempts != 1 || report.Retries != 0 || report.PublishFailures != 1 || len(store.released) != 1 {
		t.Fatalf("failure report=%#v err=%v released=%#v", report, err, store.released)
	}
	if !store.released[0].After(now) {
		t.Fatalf("retry time = %v", store.released[0])
	}

	store.records = []worker.OutboxRecord{{ID: 7, RoomID: "ABC123", Revision: 9, ClaimToken: "claim-2", Attempts: 2}}
	store.markErr = errors.New("acknowledgement uncertain")
	publisher.err = nil
	if _, err = dispatcher.RunOnce(context.Background()); !errors.Is(err, store.markErr) {
		t.Fatalf("mark error = %v", err)
	}
	if len(store.released) != 1 {
		t.Fatalf("uncertain publish was released early: %#v", store.released)
	}

	store.markErr = nil
	store.records = []worker.OutboxRecord{{ID: 7, RoomID: "ABC123", Revision: 9, ClaimToken: "claim-3", Attempts: 3}}
	restarted := worker.NewOutboxWorker(store, publisher, worker.OutboxConfig{Now: func() time.Time { return now.Add(time.Minute) }})
	report, err = restarted.RunOnce(context.Background())
	if err != nil || report.Published != 1 || report.Attempts != 1 || report.Retries != 1 || report.PublishFailures != 0 || publisher.calls != 3 || len(store.marked) != 2 {
		t.Fatalf("restart report=%#v err=%v publishes=%d marked=%#v", report, err, publisher.calls, store.marked)
	}
}

func TestPostgresOutboxClaimsExpireAndCleanupOnlyPublished(t *testing.T) {
	pool := outboxIntegrationPool(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 19, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, "INSERT INTO room_rooms(code,revision) VALUES('OUT123',3)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO wallet_accounts(account_id,account_type,auth_user_id,balance)
		VALUES('user:11111111-1111-4111-8111-111111111111','user','11111111-1111-4111-8111-111111111111',875);
		INSERT INTO wallet_transactions(business_key,transaction_type) VALUES('settlement:OUT123:1','settlement');
		INSERT INTO wallet_postings(transaction_id,account_id,amount)
		SELECT transaction_id,'user:11111111-1111-4111-8111-111111111111',75
		FROM wallet_transactions WHERE business_key='settlement:OUT123:1'
		UNION ALL
		SELECT transaction_id,'house',-75
		FROM wallet_transactions WHERE business_key='settlement:OUT123:1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO room_outbox(room_code,revision,created_at,next_attempt_at) VALUES
		('OUT123',1,$1,$2),('OUT123',2,$1,$2),('OUT123',3,$1,$2)`, now.Add(-48*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE room_rooms SET revision=4 WHERE code='OUT123'"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_outbox(room_code,revision,next_attempt_at) VALUES('OUT123',4,$1)", now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var rolledBack int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_outbox WHERE room_code='OUT123' AND revision=4").Scan(&rolledBack); err != nil || rolledBack != 0 {
		t.Fatalf("rolled-back outbox count=%d err=%v", rolledBack, err)
	}
	store := roompostgres.NewRepository(pool)
	claimed, err := store.ClaimOutbox(ctx, now, 2, 10*time.Second)
	if err != nil || len(claimed) != 2 || claimed[0].Revision != 1 || claimed[1].Revision != 2 {
		t.Fatalf("claimed=%#v err=%v", claimed, err)
	}
	if again, claimErr := store.ClaimOutbox(ctx, now, 3, 10*time.Second); claimErr != nil || len(again) != 1 || again[0].Revision != 3 {
		t.Fatalf("concurrent claim=%#v err=%v", again, claimErr)
	}
	if err = store.MarkOutboxPublished(ctx, claimed[0], now); err != nil {
		t.Fatal(err)
	}
	var balanceBefore int64
	var transactionsBefore, postingsBefore int
	if err = pool.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id='user:11111111-1111-4111-8111-111111111111'").Scan(&balanceBefore); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM wallet_transactions WHERE business_key='settlement:OUT123:1'").Scan(&transactionsBefore); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM wallet_postings p JOIN wallet_transactions t USING(transaction_id) WHERE t.business_key='settlement:OUT123:1'").Scan(&postingsBefore); err != nil {
		t.Fatal(err)
	}
	if removed, cleanupErr := store.CleanupPublishedOutbox(ctx, now.Add(time.Hour)); cleanupErr != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, cleanupErr)
	}
	if early, claimErr := store.ClaimOutbox(ctx, now.Add(9*time.Second), 3, 10*time.Second); claimErr != nil || len(early) != 0 {
		t.Fatalf("claim before lease expiry=%#v err=%v", early, claimErr)
	}
	recovered, err := store.ClaimOutbox(ctx, now.Add(11*time.Second), 3, 10*time.Second)
	if err != nil || len(recovered) != 2 {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	var pending, rooms, transactionsAfter, postingsAfter int
	var balanceAfter int64
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_outbox WHERE published_at IS NULL").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_rooms WHERE code='OUT123' AND revision=3").Scan(&rooms); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id='user:11111111-1111-4111-8111-111111111111'").Scan(&balanceAfter); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM wallet_transactions WHERE business_key='settlement:OUT123:1'").Scan(&transactionsAfter); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM wallet_postings p JOIN wallet_transactions t USING(transaction_id) WHERE t.business_key='settlement:OUT123:1'").Scan(&postingsAfter); err != nil {
		t.Fatal(err)
	}
	if pending != 2 || rooms != 1 || balanceAfter != balanceBefore || transactionsAfter != transactionsBefore || postingsAfter != postingsBefore {
		t.Fatalf("pending=%d rooms=%d balance=%d/%d transactions=%d/%d postings=%d/%d", pending, rooms, balanceAfter, balanceBefore, transactionsAfter, transactionsBefore, postingsAfter, postingsBefore)
	}
}

type outboxStoreStub struct {
	records  []worker.OutboxRecord
	markErr  error
	marked   []worker.OutboxRecord
	released []time.Time
}

func (s *outboxStoreStub) ClaimOutbox(context.Context, time.Time, int, time.Duration) ([]worker.OutboxRecord, error) {
	return s.records, nil
}
func (s *outboxStoreStub) OutboxPendingStats(context.Context, time.Time) (worker.OutboxPendingStats, error) {
	return worker.OutboxPendingStats{BacklogDepth: int64(len(s.records)), OldestPendingAge: 5 * time.Second}, nil
}
func (s *outboxStoreStub) MarkOutboxPublished(_ context.Context, record worker.OutboxRecord, _ time.Time) error {
	s.marked = append(s.marked, record)
	return s.markErr
}
func (s *outboxStoreStub) ReleaseOutbox(_ context.Context, _ worker.OutboxRecord, retryAt time.Time) error {
	s.released = append(s.released, retryAt)
	return nil
}
func (s *outboxStoreStub) CleanupPublishedOutbox(context.Context, time.Time) (int64, error) {
	return 0, nil
}

type publisherStub struct {
	err   error
	calls int
}

func (p *publisherStub) PublishRevision(context.Context, string, uint64) error {
	p.calls++
	return p.err
}

func outboxIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("PostgreSQL integration test")
	}
	if os.Getenv("WU4_POSTGRES_OUTBOX") != "1" {
		t.Skip("WU4_POSTGRES_OUTBOX is not enabled")
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = roompostgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "TRUNCATE web_sessions,command_receipts,room_rooms,game_round_history,wallet_postings,wallet_transactions,wallet_accounts CASCADE; INSERT INTO wallet_accounts(account_id,account_type) VALUES('house','house'),('mint','mint')"); err != nil {
		t.Fatal(err)
	}
	return pool
}
