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
	host, err := service.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(ctx, host.RoomCode)
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
		errs <- service.SubmitMove(ctx, host.RoomCode, host.PlayerToken, domain.Rock)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs <- service.SubmitMove(ctx, host.RoomCode, guest.PlayerToken, domain.Scissors)
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
	if err = restarted.SubmitMove(ctx, host.RoomCode, host.PlayerToken, domain.Paper); !errors.Is(err, domain.ErrDuplicateMove) {
		t.Fatalf("duplicate error = %v", err)
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

func TestPostgresConcurrentJoinAllowsOneGuest(t *testing.T) {
	pool := integrationPool(t)
	repository := postgres.NewRepository(pool)
	aggregate, _ := domain.New("JOIN23", "host-id")
	if _, err := repository.Create(context.Background(), aggregate, application.DigestToken("host-token")); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := range 2 {
		go func() {
			<-start
			_, err := repository.Join(context.Background(), "JOIN23", fmt.Sprintf("guest-%d", i), application.DigestToken(fmt.Sprintf("token-%d", i)))
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

func TestPostgresPreservesRoundHistoryAndClosedRoomAcrossRestart(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	service := fixedService(postgres.NewRepository(pool), postgres.NewEvents(pool, slog.Default()))
	host, err := service.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(ctx, host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, host.PlayerToken, domain.Paper); err != nil {
		t.Fatal(err)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, guest.PlayerToken, domain.Rock); err != nil {
		t.Fatal(err)
	}
	if err = service.RequestNextRound(ctx, host.RoomCode, guest.PlayerToken, 1); err != nil {
		t.Fatal(err)
	}
	if err = service.RequestNextRound(ctx, host.RoomCode, host.PlayerToken, 1); err != nil {
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

	if err = service.Leave(ctx, host.RoomCode, host.PlayerToken); err != nil {
		t.Fatal(err)
	}
	restarted := application.NewService(postgres.NewRepository(pool), postgres.NewEvents(pool, slog.Default()))
	snapshot, role, err := restarted.Authenticate(ctx, host.RoomCode, host.PlayerToken)
	if err != nil {
		t.Fatal(err)
	}
	if role != "host" || snapshot.Revision != 7 || snapshot.State.Round != 2 || !snapshot.State.Closed || snapshot.State.Players[0].Wins != 1 {
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
	host, err := serviceOne.Create(ctx)
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
	if _, err = serviceOne.Join(ctx, host.RoomCode); err != nil {
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
