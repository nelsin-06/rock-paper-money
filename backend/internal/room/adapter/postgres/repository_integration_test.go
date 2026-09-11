package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/room/adapter/postgres"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresPersistsAndSerializesRooms(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	service := fixedService(repository, postgres.NewEvents(pool, slog.Default()))
	host, err := service.Create(ctx, "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(ctx, host.RoomCode, "00000000-0000-4000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- service.SubmitMove(ctx, host.RoomCode, host.PlayerToken, "00000000-0000-4000-8000-000000000001", domain.Rock)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs <- service.SubmitMove(ctx, host.RoomCode, guest.PlayerToken, "00000000-0000-4000-8000-000000000002", domain.Scissors)
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	restarted := application.NewService(postgres.NewRepository(pool), postgres.NewEvents(pool, slog.Default()))
	snapshot, err := restarted.Snapshot(ctx, host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 4 || !snapshot.State.Resolved || snapshot.State.Players[0].Wins != 1 {
		t.Fatalf("reconstructed = %#v", snapshot)
	}
	if err = restarted.SubmitMove(ctx, host.RoomCode, host.PlayerToken, "00000000-0000-4000-8000-000000000001", domain.Paper); !errors.Is(err, domain.ErrRoundResolved) {
		t.Fatalf("resolved round error = %v", err)
	}
	unchanged, err := restarted.Snapshot(ctx, host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != 4 || unchanged.State.Players[0].Wins != 1 {
		t.Fatalf("failed mutation changed state: %#v", unchanged)
	}
	var credentialText string
	if err = pool.QueryRow(ctx, "SELECT encode(credential_digest,'hex') FROM room_seats WHERE room_code=$1 AND role='host'", host.RoomCode).Scan(&credentialText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(credentialText, host.PlayerToken) || credentialText == host.PlayerToken {
		t.Fatal("raw token persisted")
	}
	var owner string
	if err = pool.QueryRow(ctx, "SELECT auth_user_id::text FROM room_seats WHERE room_code=$1 AND role='host'", host.RoomCode).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("seat owner = %q", owner)
	}
	if err = restarted.SubmitMove(ctx, host.RoomCode, host.PlayerToken, "00000000-0000-4000-8000-000000000099", domain.Paper); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("cross-account mutation error = %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO room_moves(room_code,round_number,role,move) VALUES($1,1,'host','lizard')", host.RoomCode); err == nil {
		t.Fatal("move CHECK constraint accepted invalid data")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresAllowsLegacyNullableSeatsButRejectsDuplicateAccountSeats(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO room_rooms(code) VALUES('OLD234')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO room_rounds(room_code,number) VALUES('OLD234',1)"); err != nil {
		t.Fatal(err)
	}
	legacyDigest := application.DigestToken("legacy-token")
	if _, err := pool.Exec(ctx, "INSERT INTO room_seats(room_code,role,player_id,credential_digest) VALUES('OLD234','host','legacy-player',$1)", legacyDigest[:]); err != nil {
		t.Fatal(err)
	}

	repository := postgres.NewRepository(pool)
	aggregate, _ := domain.New("OWN234", "host-id")
	owner := "00000000-0000-4000-8000-000000000001"
	if _, err := repository.Create(ctx, aggregate, application.DigestToken("host-token"), owner); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	deadline := now.Add(13 * time.Second)
	_, _, err := repository.Join(ctx, "OWN234", "guest-id", application.DigestToken("guest-token"), owner, application.PresenceWindow{ObservedAt: now, ProofAfter: deadline, Deadline: deadline, EvaluateAt: deadline.Add(3 * time.Second)})
	if !errors.Is(err, application.ErrAccountSeated) {
		t.Fatalf("same-account join error = %v", err)
	}
}

func TestPostgresConcurrentJoinAllowsOneGuest(t *testing.T) {
	pool := integrationPool(t)
	repository := postgres.NewRepository(pool)
	aggregate, _ := domain.New("JOIN23", "host-id")
	if _, err := repository.Create(context.Background(), aggregate, application.DigestToken("host-token"), "00000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := range 2 {
		go func() {
			<-start
			now := time.Now()
			deadline := now.Add(13 * time.Second)
			_, _, err := repository.Join(context.Background(), "JOIN23", fmt.Sprintf("guest-%d", i), application.DigestToken(fmt.Sprintf("token-%d", i)), fmt.Sprintf("00000000-0000-4000-8000-%012d", i+2), application.PresenceWindow{ObservedAt: now, ProofAfter: deadline, Deadline: deadline, EvaluateAt: deadline.Add(3 * time.Second)})
			errs <- err
		}()
	}
	close(start)
	success, full := 0, 0
	for range 2 {
		err := <-errs
		if err == nil {
			success++
		} else if errors.Is(err, domain.ErrRoomFull) {
			full++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || full != 1 {
		t.Fatalf("success=%d full=%d", success, full)
	}
}

func TestPostgresPersistsForfeitAndRejectsStaleLease(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	service := fixedService(repository, postgres.NewEvents(pool, slog.Default()))
	host, err := service.Create(ctx, "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(ctx, host.RoomCode, "00000000-0000-4000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	guestDeadline := observed.Add(13 * time.Second)
	guestWindow := application.PresenceWindow{ObservedAt: observed, ProofAfter: guestDeadline, Deadline: guestDeadline, EvaluateAt: guestDeadline.Add(3 * time.Second)}
	skewedHostObserved := observed.Add(4 * time.Second)
	skewedHostDeadline := skewedHostObserved.Add(13 * time.Second)
	skewedHostWindow := application.PresenceWindow{ObservedAt: skewedHostObserved, ProofAfter: skewedHostDeadline, Deadline: skewedHostDeadline, EvaluateAt: skewedHostDeadline.Add(3 * time.Second)}
	if _, err = repository.RefreshPresence(ctx, host.RoomCode, application.DigestToken(host.PlayerToken), "00000000-0000-4000-8000-000000000001", skewedHostWindow); err != nil {
		t.Fatal(err)
	}
	staleLeases, err := repository.RefreshPresence(ctx, host.RoomCode, application.DigestToken(guest.PlayerToken), "00000000-0000-4000-8000-000000000002", guestWindow)
	if err != nil {
		t.Fatal(err)
	}
	stale := postgresLeaseForPlayer(t, staleLeases, "guest-id")
	currentLeases, err := repository.RefreshPresence(ctx, host.RoomCode, application.DigestToken(guest.PlayerToken), "00000000-0000-4000-8000-000000000002", guestWindow)
	if err != nil {
		t.Fatal(err)
	}
	current := postgresLeaseForPlayer(t, currentLeases, "guest-id")
	if _, changed, err := repository.ForfeitExpired(ctx, stale, guestWindow.EvaluateAt); err != nil || changed {
		t.Fatalf("stale lease changed=%v error=%v", changed, err)
	}
	if _, changed, err := repository.ForfeitExpired(ctx, current, guestWindow.EvaluateAt); err != nil || changed {
		t.Fatalf("skewed absent lease changed=%v error=%v", changed, err)
	}
	hostObserved := guestWindow.ProofAfter.Add(time.Second)
	hostDeadline := hostObserved.Add(13 * time.Second)
	hostWindow := application.PresenceWindow{ObservedAt: hostObserved, ProofAfter: hostDeadline, Deadline: hostDeadline, EvaluateAt: hostDeadline.Add(3 * time.Second)}
	refreshedLeases, err := repository.RefreshPresence(ctx, host.RoomCode, application.DigestToken(host.PlayerToken), "00000000-0000-4000-8000-000000000001", hostWindow)
	if err != nil {
		t.Fatal(err)
	}
	reconstructed := postgresLeaseForPlayer(t, refreshedLeases, "guest-id")
	if reconstructed.Generation <= current.Generation || reconstructed.Deadline != current.Deadline || reconstructed.ProofAfter != current.ProofAfter {
		t.Fatalf("reconstructed lease = %#v, previous = %#v", reconstructed, current)
	}
	snapshot, changed, err := repository.ForfeitExpired(ctx, reconstructed, guestWindow.EvaluateAt)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !snapshot.State.Forfeit || snapshot.State.Result != domain.PlayerOneWins || snapshot.State.Players[0].Wins != 1 {
		t.Fatalf("forfeit snapshot = %#v, changed=%v", snapshot, changed)
	}
	if _, changed, err = repository.ForfeitExpired(ctx, reconstructed, guestWindow.EvaluateAt); err != nil || changed {
		t.Fatalf("duplicate lease changed=%v error=%v", changed, err)
	}
	var presenceCount int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_presence WHERE room_code=$1", host.RoomCode).Scan(&presenceCount); err != nil {
		t.Fatal(err)
	}
	if presenceCount != 2 {
		t.Fatalf("presence rows after forfeit = %d, want 2", presenceCount)
	}
	restarted := application.NewService(postgres.NewRepository(pool), postgres.NewEvents(pool, slog.Default()))
	restored, err := restarted.Snapshot(ctx, host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.State.Forfeit || restored.State.Result != domain.PlayerOneWins || restored.State.Players[0].Wins != 1 {
		t.Fatalf("restored forfeit = %#v", restored)
	}
}

func TestPostgresPreservesRoundHistoryAndClosedRoomAcrossRestart(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	service := fixedService(postgres.NewRepository(pool), postgres.NewEvents(pool, slog.Default()))
	host, err := service.Create(ctx, "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(ctx, host.RoomCode, "00000000-0000-4000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, host.PlayerToken, "00000000-0000-4000-8000-000000000001", domain.Paper); err != nil {
		t.Fatal(err)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, guest.PlayerToken, "00000000-0000-4000-8000-000000000002", domain.Rock); err != nil {
		t.Fatal(err)
	}
	if err = service.RequestNextRound(ctx, host.RoomCode, guest.PlayerToken, "00000000-0000-4000-8000-000000000002", 1); err != nil {
		t.Fatal(err)
	}
	if err = service.RequestNextRound(ctx, host.RoomCode, host.PlayerToken, "00000000-0000-4000-8000-000000000001", 1); err != nil {
		t.Fatal(err)
	}

	var result string
	var moveCount, requestCount int
	if err = pool.QueryRow(ctx, "SELECT result FROM room_rounds WHERE room_code=$1 AND number=1", host.RoomCode).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_moves WHERE room_code=$1 AND round_number=1", host.RoomCode).Scan(&moveCount); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_next_round_requests WHERE room_code=$1 AND round_number=1", host.RoomCode).Scan(&requestCount); err != nil {
		t.Fatal(err)
	}
	if result != string(domain.PlayerOneWins) || moveCount != 2 || requestCount != 2 {
		t.Fatalf("round-one history = result %q, %d moves, %d requests", result, moveCount, requestCount)
	}

	if err = service.SubmitMove(ctx, host.RoomCode, host.PlayerToken, "00000000-0000-4000-8000-000000000001", domain.Rock); err != nil {
		t.Fatal(err)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, guest.PlayerToken, "00000000-0000-4000-8000-000000000002", domain.Rock); err != nil {
		t.Fatal(err)
	}
	if err = service.Leave(ctx, host.RoomCode, host.PlayerToken, "00000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	restarted := application.NewService(postgres.NewRepository(pool), postgres.NewEvents(pool, slog.Default()))
	snapshot, role, err := restarted.Authenticate(ctx, host.RoomCode, host.PlayerToken, "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if role != "host" || snapshot.Revision != 9 || snapshot.State.Round != 2 || !snapshot.State.Resolved || !snapshot.State.Closed || snapshot.State.Players[0].Wins != 1 {
		t.Fatalf("restarted closed room = role %q, snapshot %#v", role, snapshot)
	}
}

func TestPostgresNotificationsCrossInstances(t *testing.T) {
	pool := integrationPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventsOne := postgres.NewEvents(pool, slog.Default())
	eventsTwo := postgres.NewEvents(pool, slog.Default())
	go eventsOne.Run(ctx)
	go eventsTwo.Run(ctx)
	repository := postgres.NewRepository(pool)
	serviceOne := fixedService(repository, eventsOne)
	host, err := serviceOne.Create(ctx, "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	serviceTwo := application.NewService(postgres.NewRepository(pool), eventsTwo)
	_, changes, unsubscribe, err := serviceTwo.Subscribe(ctx, host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	time.Sleep(100 * time.Millisecond)
	if _, err = serviceOne.Join(ctx, host.RoomCode, "00000000-0000-4000-8000-000000000002"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("cross-instance notification not received")
	}
	snapshot, err := serviceTwo.Snapshot(ctx, host.RoomCode)
	if err != nil || snapshot.Revision != 2 || !snapshot.State.Ready {
		t.Fatalf("authoritative snapshot = %#v, error=%v", snapshot, err)
	}
}

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("PostgreSQL integration test skipped in short mode")
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = pool.Ping(context.Background()); err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	if err = postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), "TRUNCATE room_rooms CASCADE"); err != nil {
		t.Fatal(err)
	}
	return pool
}
func fixedService(repository application.Repository, events application.Events) *application.Service {
	values := []string{"host-token", "host-id", "guest-token", "guest-id"}
	var mu sync.Mutex
	next := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		value := values[0]
		values = values[1:]
		return value, nil
	}
	return application.NewServiceWithGenerators(repository, events, func() (string, error) { return "PGT234", nil }, next, next)
}

func postgresLeaseForPlayer(t *testing.T, leases []application.PresenceLease, playerID string) application.PresenceLease {
	t.Helper()
	for _, lease := range leases {
		if lease.PlayerID == playerID {
			return lease
		}
	}
	t.Fatalf("lease for %q not found in %#v", playerID, leases)
	return application.PresenceLease{}
}
