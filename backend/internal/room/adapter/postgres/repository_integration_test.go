package postgres_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/auth"
	roomhttp "example.com/rock-paper-money/internal/room/adapter/http"
	"example.com/rock-paper-money/internal/room/adapter/postgres"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresSessionRestartRevocationAndSafeRoomReset(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	current := now
	store := auth.NewPostgresSessionStore(pool)
	service := auth.NewSessionService(store, rand.Reader, func() time.Time { return current })
	credentials, err := service.Create(ctx, "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}

	restarted := auth.NewSessionService(auth.NewPostgresSessionStore(pool), rand.Reader, func() time.Time { return current.Add(time.Hour) })
	if _, err = restarted.Authenticate(ctx, credentials.Token); err != nil {
		t.Fatalf("authenticate after restart: %v", err)
	}
	if err = restarted.Revoke(ctx, credentials.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Authenticate(ctx, credentials.Token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("revoked session error = %v", err)
	}

	if _, err = pool.Exec(ctx, `
		INSERT INTO room_rooms(code) VALUES('RST234');
		INSERT INTO room_rounds(room_code,number,funded) VALUES('RST234',1,true);
		INSERT INTO wallet_accounts(account_id,account_type,room_code,round_number,balance)
		VALUES('escrow:RST234:1','escrow','RST234',1,100);
		INSERT INTO wallet_transactions(business_key,transaction_type)
		VALUES('settlement:RST234:1','settlement');
		INSERT INTO game_round_history(room_code,round_number,host_account_id,guest_account_id,result,forfeited,house_earnings)
		VALUES('RST234',1,'user:host','user:guest','draw',false,0)`); err != nil {
		t.Fatal(err)
	}
	repository := postgres.NewRepository(pool)
	if err = repository.ResetDisposableRooms(ctx); !errors.Is(err, postgres.ErrFundedEscrowRemaining) {
		t.Fatalf("funded reset error = %v", err)
	}
	var rooms int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_rooms WHERE code='RST234'").Scan(&rooms); err != nil || rooms != 1 {
		t.Fatalf("room count after blocked reset = %d, error = %v", rooms, err)
	}
	if _, err = pool.Exec(ctx, "UPDATE wallet_accounts SET balance=0 WHERE account_id='escrow:RST234:1'"); err != nil {
		t.Fatal(err)
	}
	if err = repository.ResetDisposableRooms(ctx); err != nil {
		t.Fatal(err)
	}
	var escrowAccounts, settlements, history int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM wallet_accounts WHERE account_id='escrow:RST234:1'").Scan(&escrowAccounts); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM wallet_transactions WHERE business_key='settlement:RST234:1'").Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM game_round_history WHERE room_code='RST234'").Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_rooms").Scan(&rooms); err != nil || rooms != 0 || escrowAccounts != 1 || settlements != 1 || history != 1 {
		t.Fatalf("after reset rooms=%d escrow=%d settlements=%d history=%d error=%v", rooms, escrowAccounts, settlements, history, err)
	}
}

func TestPostgresAccountSeatRaceAndCurrentAuthority(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	service := fixedService(repository, postgres.NewEvents(pool, slog.Default()))
	hostAccount := "00000000-0000-4000-8000-000000000001"
	guestAccounts := []string{
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
	}
	if _, err := service.Recharge(ctx, guestAccounts[1], 1_000, "seat-race-third-account-funding"); err != nil {
		t.Fatal(err)
	}

	host, err := service.CreateCommand(ctx, hostAccount, "seat-race-create")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, len(guestAccounts))
	for index, account := range guestAccounts {
		go func() {
			<-start
			_, joinErr := service.JoinCommand(ctx, host.RoomCode, account, fmt.Sprintf("seat-race-join-%d", index))
			errs <- joinErr
		}()
	}
	close(start)

	winner := ""
	for range guestAccounts {
		joinErr := <-errs
		if joinErr == nil {
			continue
		}
		if !errors.Is(joinErr, domain.ErrRoomFull) {
			t.Fatalf("seat race error = %v", joinErr)
		}
	}
	if err = pool.QueryRow(ctx, "SELECT auth_user_id::text FROM room_seats WHERE room_code=$1 AND role='guest'", host.RoomCode).Scan(&winner); err != nil {
		t.Fatal(err)
	}
	restarted := application.NewService(postgres.NewRepository(pool), postgres.NewEvents(pool, slog.Default()))
	if _, err = restarted.SnapshotForAccount(ctx, host.RoomCode, winner); err != nil {
		t.Fatalf("winner after restart: %v", err)
	}
	loser := ""
	for _, account := range guestAccounts {
		if account == winner {
			continue
		}
		loser = account
		if _, err = restarted.SnapshotForAccount(ctx, host.RoomCode, account); !errors.Is(err, application.ErrUnauthorized) {
			t.Fatalf("losing account access error = %v", err)
		}
	}
	if _, err = restarted.SnapshotForAccount(ctx, host.RoomCode, ""); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("unauthenticated access error = %v", err)
	}
	if _, _, err = restarted.Authenticate(ctx, host.RoomCode, "host-token", loser); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("legacy room-token claim error = %v", err)
	}
	otherAccount := "00000000-0000-4000-8000-000000000004"
	if _, err = restarted.Recharge(ctx, otherAccount, 1_000, "cross-room-owner-funding"); err != nil {
		t.Fatal(err)
	}
	otherRoom, _ := domain.New("OTHR23", "other-host")
	otherCommand, _ := application.NewCommand("cross-room-create", "room.create", struct{}{})
	if _, err = repository.CreateCommand(ctx, otherRoom, otherAccount, otherCommand); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.SnapshotForAccount(ctx, "OTHR23", hostAccount); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("cross-room access error = %v", err)
	}
}

func TestPostgresCommandReceiptReplaysAcrossLossRestartAndConcurrency(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	service := fixedService(repository, postgres.NewEvents(pool, slog.Default()))
	hostAccount := "00000000-0000-4000-8000-000000000001"
	guestAccount := "00000000-0000-4000-8000-000000000002"
	host, err := service.CreateCommand(ctx, hostAccount, "receipt-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.JoinCommand(ctx, host.RoomCode, guestAccount, "receipt-join"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- service.SubmitMoveCommand(ctx, host.RoomCode, hostAccount, domain.Rock, "lost-response-move")
		}()
	}
	close(start)
	for range 2 {
		if commandErr := <-errs; commandErr != nil {
			t.Fatal(commandErr)
		}
	}

	restarted := application.NewService(postgres.NewRepository(pool), postgres.NewEvents(pool, slog.Default()))
	if err = restarted.SubmitMoveCommand(ctx, host.RoomCode, hostAccount, domain.Rock, "lost-response-move"); err != nil {
		t.Fatalf("restart replay: %v", err)
	}
	if err = restarted.SubmitMoveCommand(ctx, host.RoomCode, hostAccount, domain.Paper, "lost-response-move"); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("changed meaning error = %v", err)
	}
	if err = restarted.SubmitMoveCommand(ctx, host.RoomCode, guestAccount, domain.Scissors, "lost-response-move"); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("cross-account key error = %v", err)
	}
	if err = restarted.SubmitMoveCommand(ctx, host.RoomCode, hostAccount, domain.Paper, "failed-command"); !errors.Is(err, domain.ErrDuplicateMove) {
		t.Fatalf("failed command error = %v", err)
	}

	var revision uint64
	var moves, receipts, outbox int
	var receiptStatus int
	var receiptLifetime time.Duration
	if err = pool.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1", host.RoomCode).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_moves WHERE room_code=$1", host.RoomCode).Scan(&moves); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM command_receipts WHERE idempotency_key='lost-response-move'").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT response_status,expires_at-created_at FROM command_receipts WHERE idempotency_key='lost-response-move'").Scan(&receiptStatus, &receiptLifetime); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_outbox WHERE room_code=$1", host.RoomCode).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	var failedReceipts int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM command_receipts WHERE idempotency_key='failed-command'").Scan(&failedReceipts); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || moves != 1 || receipts != 1 || outbox != 3 || failedReceipts != 0 || receiptStatus != 204 || receiptLifetime < application.CommandReceiptRetention {
		t.Fatalf("revision=%d moves=%d receipts=%d outbox=%d failed_receipts=%d status=%d lifetime=%s", revision, moves, receipts, outbox, failedReceipts, receiptStatus, receiptLifetime)
	}

	if err = restarted.SubmitMoveCommand(ctx, host.RoomCode, guestAccount, domain.Scissors, "settlement-guest-move"); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE command_receipts SET expires_at=created_at"); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.CleanupCommandReceipts(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var settlements int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM wallet_transactions WHERE business_key=$1", "settlement:"+host.RoomCode+":1").Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if settlements != 1 {
		t.Fatalf("settlements after receipt cleanup = %d", settlements)
	}
}

func TestPostgresSettlementRetryAfterReceiptCleanupRemainsIdempotent(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	roomCode, hostAccount, guestAccount := createFundedDeadlineRoom(t, repository, "receipt-cleanup")

	hostMove, _ := application.NewCommand("settlement-host-move", "room.move", struct {
		RoomCode string      `json:"room_code"`
		Move     domain.Move `json:"move"`
	}{roomCode, domain.Rock})
	if _, err := repository.SubmitMoveCommand(ctx, roomCode, hostAccount, domain.Rock, hostMove); err != nil {
		t.Fatal(err)
	}
	guestMove, _ := application.NewCommand("settlement-guest-move", "room.move", struct {
		RoomCode string      `json:"room_code"`
		Move     domain.Move `json:"move"`
	}{roomCode, domain.Scissors})
	if _, err := repository.SubmitMoveCommand(ctx, roomCode, guestAccount, domain.Scissors, guestMove); err != nil {
		t.Fatal(err)
	}
	before := readAuthorityState(t, pool, roomCode)
	assertSingleSettlement(t, pool, roomCode)

	if _, err := pool.Exec(ctx, "UPDATE command_receipts SET expires_at=created_at WHERE idempotency_key=$1", guestMove.Key); err != nil {
		t.Fatal(err)
	}
	if cleaned, err := repository.CleanupCommandReceipts(ctx, time.Now()); err != nil || cleaned != 1 {
		t.Fatalf("receipt cleanup=%d error=%v", cleaned, err)
	}
	var receipts int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM command_receipts WHERE idempotency_key=$1", guestMove.Key).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("settlement command receipts=%d error=%v", receipts, err)
	}

	if _, err := repository.SubmitMoveCommand(ctx, roomCode, guestAccount, domain.Scissors, guestMove); !errors.Is(err, domain.ErrRoundResolved) {
		t.Fatalf("post-cleanup settlement retry error=%v, want round resolved", err)
	}
	after := readAuthorityState(t, pool, roomCode)
	if after != before {
		t.Fatalf("authority changed after post-cleanup settlement retry: before=%+v after=%+v", before, after)
	}
	assertSingleSettlement(t, pool, roomCode)
}

func TestPostgresRepeatedRejectedCSRFMutationHasNoEffects(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	service := fixedService(repository, postgres.NewEvents(pool, slog.Default()))
	hostAccount := "00000000-0000-4000-8000-000000000001"
	created, err := service.CreateCommand(ctx, hostAccount, "csrf-runtime-create")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	sessions := auth.NewSessionService(auth.NewPostgresSessionStore(pool), rand.Reader, func() time.Time { return now })
	credentials, err := sessions.Create(ctx, hostAccount)
	if err != nil {
		t.Fatal(err)
	}
	security, err := auth.NewRequestSecurity("https://game.example", sessions)
	if err != nil {
		t.Fatal(err)
	}
	handler := roomhttp.NewSessionRouter(service, nil, sessions, security, nil, nil)
	before := readAuthorityState(t, pool, created.RoomCode)

	for attempt := 1; attempt <= 3; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/rooms/"+created.RoomCode+"/moves", strings.NewReader(`{"move":"rock"}`))
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token})
		request.Header.Set("Origin", "https://game.example")
		request.Header.Set(auth.CSRFHeaderName, "invalid-csrf-proof")
		request.Header.Set("Idempotency-Key", "rejected-csrf-retry")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("attempt %d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
	}

	after := readAuthorityState(t, pool, created.RoomCode)
	if after != before {
		t.Fatalf("rejected CSRF retries changed authority: before=%+v after=%+v", before, after)
	}
	var rejectedReceipts int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM command_receipts WHERE idempotency_key='rejected-csrf-retry'").Scan(&rejectedReceipts); err != nil || rejectedReceipts != 0 {
		t.Fatalf("rejected mutation receipts=%d error=%v", rejectedReceipts, err)
	}
}

func TestPostgresCleanedSessionCookieRemainsRejectedWithoutEffects(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	service := fixedService(repository, postgres.NewEvents(pool, slog.Default()))
	hostAccount := "00000000-0000-4000-8000-000000000001"
	created, err := service.CreateCommand(ctx, hostAccount, "session-cleanup-create")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	sessions := auth.NewSessionService(auth.NewPostgresSessionStore(pool), rand.Reader, func() time.Time { return now })
	credentials, err := sessions.Create(ctx, hostAccount)
	if err != nil {
		t.Fatal(err)
	}
	security, err := auth.NewRequestSecurity("https://game.example", sessions)
	if err != nil {
		t.Fatal(err)
	}
	handler := roomhttp.NewSessionRouter(service, nil, sessions, security, nil, nil)
	if err = sessions.Revoke(ctx, credentials.Token); err != nil {
		t.Fatal(err)
	}
	if cleaned, cleanupErr := sessions.Cleanup(ctx); cleanupErr != nil || cleaned != 1 {
		t.Fatalf("session cleanup=%d error=%v", cleaned, cleanupErr)
	}
	var sessionsLeft int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM web_sessions").Scan(&sessionsLeft); err != nil || sessionsLeft != 0 {
		t.Fatalf("sessions after cleanup=%d error=%v", sessionsLeft, err)
	}
	before := readAuthorityState(t, pool, created.RoomCode)

	stateRequest := httptest.NewRequest(http.MethodGet, "/api/rooms/"+created.RoomCode+"/state", nil)
	stateRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token})
	stateResponse := httptest.NewRecorder()
	handler.ServeHTTP(stateResponse, stateRequest)
	if stateResponse.Code != http.StatusUnauthorized || strings.Contains(stateResponse.Body.String(), created.RoomCode) {
		t.Fatalf("cleaned cookie state status=%d body=%s", stateResponse.Code, stateResponse.Body.String())
	}

	mutation := httptest.NewRequest(http.MethodPost, "/api/rooms/"+created.RoomCode+"/moves", strings.NewReader(`{"move":"rock"}`))
	mutation.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token})
	mutation.Header.Set("Origin", "https://game.example")
	mutation.Header.Set(auth.CSRFHeaderName, credentials.CSRFToken)
	mutation.Header.Set("Idempotency-Key", "cleaned-cookie-mutation")
	mutationResponse := httptest.NewRecorder()
	handler.ServeHTTP(mutationResponse, mutation)
	if mutationResponse.Code != http.StatusUnauthorized {
		t.Fatalf("cleaned cookie mutation status=%d body=%s", mutationResponse.Code, mutationResponse.Body.String())
	}
	after := readAuthorityState(t, pool, created.RoomCode)
	if after != before {
		t.Fatalf("cleaned cookie changed authority: before=%+v after=%+v", before, after)
	}
	var protectedReceipt int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM command_receipts WHERE idempotency_key='cleaned-cookie-mutation'").Scan(&protectedReceipt); err != nil || protectedReceipt != 0 {
		t.Fatalf("cleaned cookie receipt=%d error=%v", protectedReceipt, err)
	}
}

func TestPostgresMultiSocketPresenceUsesGenerationBoundGrace(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	service := fixedService(repository, postgres.NewEvents(pool, slog.Default()))
	hostAccount := "00000000-0000-4000-8000-000000000001"
	guestAccount := "00000000-0000-4000-8000-000000000002"
	host, err := service.CreateCommand(ctx, hostAccount, "presence-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.JoinCommand(ctx, host.RoomCode, guestAccount, "presence-join"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 16, 0, 0, 0, time.UTC)
	hostSession := insertDeadlineSession(t, pool, hostAccount, 1, now)
	guestSession := insertDeadlineSession(t, pool, guestAccount, 2, now)
	connections := []application.ConnectionLease{
		{ID: "10000000-0000-4000-8000-000000000001", RoomCode: host.RoomCode, AccountID: hostAccount, SessionDigest: hostSession, ConnectedAt: now, LeaseExpiresAt: now.Add(5 * time.Second)},
		{ID: "10000000-0000-4000-8000-000000000002", RoomCode: host.RoomCode, AccountID: hostAccount, SessionDigest: hostSession, ConnectedAt: now, LeaseExpiresAt: now.Add(time.Minute)},
		{ID: "20000000-0000-4000-8000-000000000001", RoomCode: host.RoomCode, AccountID: guestAccount, SessionDigest: guestSession, ConnectedAt: now, LeaseExpiresAt: now.Add(time.Minute)},
		{ID: "10000000-0000-4000-8000-000000000004", RoomCode: host.RoomCode, AccountID: hostAccount, SessionDigest: hostSession, ConnectedAt: now, LeaseExpiresAt: now.Add(5 * time.Second)},
	}
	for _, connection := range connections {
		if _, err = repository.OpenConnection(ctx, connection); err != nil {
			t.Fatal(err)
		}
	}
	if err = repository.RenewConnection(ctx, connections[3].ID, now.Add(4*time.Second), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if reaped, reapErr := repository.ReapExpiredConnections(ctx, now.Add(6*time.Second)); reapErr != nil || reaped != 1 {
		t.Fatalf("reaped connections = %d, error = %v", reaped, reapErr)
	}
	assertDeadlineCount(t, pool, host.RoomCode, "disconnect", "pending", 0)
	if _, err = repository.CloseConnection(ctx, connections[3].ID, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertDeadlineCount(t, pool, host.RoomCode, "disconnect", "pending", 0)
	closed, err := repository.CloseConnection(ctx, connections[1].ID, now.Add(7*time.Second))
	if err != nil || !closed.LastSocket || closed.GraceDueAt != now.Add(27*time.Second) {
		t.Fatalf("last close = %#v, error = %v", closed, err)
	}
	assertDeadlineCount(t, pool, host.RoomCode, "disconnect", "pending", 1)
	if _, err = repository.CloseConnection(ctx, connections[1].ID, now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertDeadlineCount(t, pool, host.RoomCode, "disconnect", "pending", 1)

	reconnect := application.ConnectionLease{ID: "10000000-0000-4000-8000-000000000003", RoomCode: host.RoomCode, AccountID: hostAccount, SessionDigest: hostSession, ConnectedAt: now.Add(10 * time.Second), LeaseExpiresAt: now.Add(time.Minute)}
	opened, err := repository.OpenConnection(ctx, reconnect)
	if err != nil || opened.Generation != closed.Generation {
		t.Fatalf("reconnect = %#v, error = %v", opened, err)
	}
	assertDeadlineCount(t, pool, host.RoomCode, "disconnect", "cancelled", 1)
	result, err := repository.ProcessNextDeadline(ctx, now.Add(time.Minute))
	if err != nil || result.Settled {
		t.Fatalf("stale reconnect deadline = %#v, error = %v", result, err)
	}
	snapshot, err := repository.SnapshotForAccount(ctx, host.RoomCode, hostAccount)
	if err != nil || snapshot.State.Resolved {
		t.Fatalf("snapshot after stale grace = %#v, error = %v", snapshot, err)
	}
	var outbox int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_outbox WHERE room_code=$1", host.RoomCode).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 6 || outbox != int(snapshot.Revision) {
		t.Fatalf("presence revision=%d outbox=%d", snapshot.Revision, outbox)
	}
}

func TestPostgresDeadlineRestartPrecedenceAndOutcomes(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	roomCode, hostAccount, guestAccount := createFundedDeadlineRoom(t, repository, "deadline")
	now := time.Date(2026, 9, 15, 17, 0, 0, 0, time.UTC)
	hostSession := insertDeadlineSession(t, pool, hostAccount, 3, now)
	guestSession := insertDeadlineSession(t, pool, guestAccount, 4, now)
	hostConnection := application.ConnectionLease{ID: "30000000-0000-4000-8000-000000000001", RoomCode: roomCode, AccountID: hostAccount, SessionDigest: hostSession, ConnectedAt: now, LeaseExpiresAt: now.Add(time.Minute)}
	guestConnection := application.ConnectionLease{ID: "40000000-0000-4000-8000-000000000001", RoomCode: roomCode, AccountID: guestAccount, SessionDigest: guestSession, ConnectedAt: now, LeaseExpiresAt: now.Add(time.Minute)}
	if _, err := repository.OpenConnection(ctx, hostConnection); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.OpenConnection(ctx, guestConnection); err != nil {
		t.Fatal(err)
	}
	closed, err := repository.CloseConnection(ctx, guestConnection.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE room_deadlines SET due_at=$2 WHERE room_code=$1 AND kind='inactivity' AND status='pending'", roomCode, closed.GraceDueAt); err != nil {
		t.Fatal(err)
	}

	restarted := postgres.NewRepository(pool)
	result, err := restarted.ProcessNextDeadline(ctx, closed.GraceDueAt)
	if err != nil || !result.Settled || result.Kind != application.DeadlineDisconnect {
		t.Fatalf("tie result = %#v, error = %v", result, err)
	}
	snapshot, err := restarted.SnapshotForAccount(ctx, roomCode, hostAccount)
	if err != nil || snapshot.State.Result != domain.PlayerOneWins || !snapshot.State.Forfeit {
		t.Fatalf("disconnect result = %#v, error = %v", snapshot, err)
	}
	assertSingleSettlement(t, pool, roomCode)

	zeroRoom, zeroHost, _ := createFundedDeadlineRoom(t, repository, "zero")
	forceInactivityDue(t, pool, zeroRoom, now)
	if result, err = restarted.ProcessNextDeadline(ctx, now); err != nil || !result.Settled || result.Kind != application.DeadlineInactivity {
		t.Fatalf("zero move inactivity = %#v, error = %v", result, err)
	}
	zeroSnapshot, _ := restarted.SnapshotForAccount(ctx, zeroRoom, zeroHost)
	if zeroSnapshot.State.Result != domain.Draw {
		t.Fatalf("zero move result = %#v", zeroSnapshot)
	}

	oneRoom, oneHost, _ := createFundedDeadlineRoom(t, repository, "one")
	move, _ := application.NewCommand("one-move", "room.move", struct {
		RoomCode string      `json:"room_code"`
		Move     domain.Move `json:"move"`
	}{oneRoom, domain.Rock})
	if _, err = repository.SubmitMoveCommand(ctx, oneRoom, oneHost, domain.Rock, move); err != nil {
		t.Fatal(err)
	}
	forceInactivityDue(t, pool, oneRoom, now)
	if result, err = restarted.ProcessNextDeadline(ctx, now); err != nil || !result.Settled {
		t.Fatalf("one move inactivity = %#v, error = %v", result, err)
	}
	oneSnapshot, _ := restarted.SnapshotForAccount(ctx, oneRoom, oneHost)
	if oneSnapshot.State.Result != domain.PlayerOneWins || oneSnapshot.State.Players[0].Wins != 1 {
		t.Fatalf("one move result = %#v", oneSnapshot)
	}
}

func TestPostgresMoveReconnectAndTwoWorkerDeadlineRacesSettleOnce(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	roomCode, hostAccount, guestAccount := createFundedDeadlineRoom(t, repository, "race")
	now := time.Date(2026, 9, 15, 18, 0, 0, 0, time.UTC)
	hostSession := insertDeadlineSession(t, pool, hostAccount, 5, now)
	guestSession := insertDeadlineSession(t, pool, guestAccount, 6, now)
	hostConnection := application.ConnectionLease{ID: "50000000-0000-4000-8000-000000000001", RoomCode: roomCode, AccountID: hostAccount, SessionDigest: hostSession, ConnectedAt: now, LeaseExpiresAt: now.Add(time.Minute)}
	guestConnection := application.ConnectionLease{ID: "60000000-0000-4000-8000-000000000001", RoomCode: roomCode, AccountID: guestAccount, SessionDigest: guestSession, ConnectedAt: now, LeaseExpiresAt: now.Add(time.Minute)}
	if _, err := repository.OpenConnection(ctx, hostConnection); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.OpenConnection(ctx, guestConnection); err != nil {
		t.Fatal(err)
	}
	closed, err := repository.CloseConnection(ctx, guestConnection.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	due := closed.GraceDueAt
	forceInactivityDue(t, pool, roomCode, due)

	start := make(chan struct{})
	errs := make(chan error, 4)
	go func() {
		<-start
		command, _ := application.NewCommand("racing-move", "room.move", struct {
			RoomCode string      `json:"room_code"`
			Move     domain.Move `json:"move"`
		}{roomCode, domain.Rock})
		_, moveErr := repository.SubmitMoveCommand(ctx, roomCode, hostAccount, domain.Rock, command)
		if moveErr != nil && !errors.Is(moveErr, domain.ErrRoundResolved) {
			errs <- moveErr
			return
		}
		errs <- nil
	}()
	go func() {
		<-start
		_, reconnectErr := repository.OpenConnection(ctx, application.ConnectionLease{ID: "60000000-0000-4000-8000-000000000002", RoomCode: roomCode, AccountID: guestAccount, SessionDigest: guestSession, ConnectedAt: due, LeaseExpiresAt: due.Add(time.Minute)})
		errs <- reconnectErr
	}()
	for range 2 {
		go func() {
			<-start
			_, workerErr := postgres.NewRepository(pool).ProcessNextDeadline(ctx, due)
			errs <- workerErr
		}()
	}
	close(start)
	for range 4 {
		if raceErr := <-errs; raceErr != nil {
			t.Fatal(raceErr)
		}
	}
	assertSingleSettlement(t, pool, roomCode)
	var history, settlementPostings, outbox int
	var revision uint64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM game_round_history WHERE room_code=$1 AND round_number=1", roomCode).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM wallet_postings p JOIN wallet_transactions t USING(transaction_id) WHERE t.business_key=$1", "settlement:"+roomCode+":1").Scan(&settlementPostings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM room_outbox WHERE room_code=$1", roomCode).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1", roomCode).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if history != 1 || settlementPostings != 3 || outbox != int(revision) {
		t.Fatalf("history=%d settlement_postings=%d outbox=%d revision=%d", history, settlementPostings, outbox, revision)
	}
	if cleaned, err := repository.CleanupDeadlines(ctx, due.Add(8*24*time.Hour)); err != nil || cleaned == 0 {
		t.Fatalf("cleanup=%d error=%v", cleaned, err)
	}
	assertSingleSettlement(t, pool, roomCode)
}

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

func TestPostgresRejectsLegacyNullableSeatsAndDuplicateAccountSeats(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO room_rooms(code) VALUES('OLD234')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO room_rounds(room_code,number) VALUES('OLD234',1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO room_seats(room_code,role,player_id) VALUES('OLD234','host','legacy-player')"); err == nil {
		t.Fatal("legacy seat without account owner was accepted")
	}

	repository := postgres.NewRepository(pool)
	aggregate, _ := domain.New("OWN234", "host-id")
	owner := "00000000-0000-4000-8000-000000000001"
	if _, err := repository.Recharge(ctx, owner, application.RoundStake, "owner-create-funding"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Create(ctx, aggregate, application.DigestToken("host-token"), owner); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	deadline := now.Add(13 * time.Second)
	_, _, err := repository.JoinFunded(ctx, "OWN234", "guest-id", application.DigestToken("guest-token"), owner, application.PresenceWindow{ObservedAt: now, ProofAfter: deadline, Deadline: deadline, EvaluateAt: deadline.Add(3 * time.Second)})
	if !errors.Is(err, application.ErrAccountSeated) {
		t.Fatalf("same-account join error = %v", err)
	}
}

func TestPostgresConcurrentJoinAllowsOneGuest(t *testing.T) {
	pool := integrationPool(t)
	repository := postgres.NewRepository(pool)
	aggregate, _ := domain.New("JOIN23", "host-id")
	for index := 1; index <= 3; index++ {
		owner := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		if _, err := repository.Recharge(context.Background(), owner, 1_000, fmt.Sprintf("concurrent-join-funding-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
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
			_, _, err := repository.JoinFunded(context.Background(), "JOIN23", fmt.Sprintf("guest-%d", i), application.DigestToken(fmt.Sprintf("token-%d", i)), fmt.Sprintf("00000000-0000-4000-8000-%012d", i+2), application.PresenceWindow{ObservedAt: now, ProofAfter: deadline, Deadline: deadline, EvaluateAt: deadline.Add(3 * time.Second)})
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

func TestPostgresPaidRoundIsAtomicBalancedAndDurable(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	repository := postgres.NewRepository(pool)
	hostOwner := "00000000-0000-4000-8000-000000000001"
	guestOwner := "00000000-0000-4000-8000-000000000002"
	hostDigest := application.DigestToken("wallet-host-token")
	guestDigest := application.DigestToken("wallet-guest-token")
	aggregate, _ := domain.New("WAL234", "wallet-host")
	if _, err := repository.Create(ctx, aggregate, hostDigest, hostOwner); !errors.Is(err, application.ErrInsufficientFunds) {
		t.Fatalf("underfunded create error=%v", err)
	}
	if _, err := repository.Recharge(ctx, hostOwner, 100, "wallet-host-credit"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Recharge(ctx, guestOwner, 49, "wallet-guest-partial-credit"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Create(ctx, aggregate, hostDigest, hostOwner); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	deadline := now.Add(13 * time.Second)
	window := application.PresenceWindow{ObservedAt: now, ProofAfter: deadline, Deadline: deadline, EvaluateAt: deadline.Add(3 * time.Second)}
	if _, _, err := repository.JoinFunded(ctx, "WAL234", "wallet-guest", guestDigest, guestOwner, window); !errors.Is(err, application.ErrInsufficientFunds) {
		t.Fatalf("underfunded join error=%v", err)
	}
	var seats int
	var funded bool
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM room_seats WHERE room_code='WAL234'").Scan(&seats); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT funded FROM room_rounds WHERE room_code='WAL234' AND number=1").Scan(&funded); err != nil {
		t.Fatal(err)
	}
	hostBalance, _ := repository.Balance(ctx, hostOwner)
	if seats != 1 || funded || hostBalance != 100 {
		t.Fatalf("failed funding left seats=%d funded=%v host_balance=%d", seats, funded, hostBalance)
	}
	if _, err := repository.Recharge(ctx, guestOwner, 1, "wallet-guest-final-credit"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.JoinFunded(ctx, "WAL234", "wallet-guest", guestDigest, guestOwner, window); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SubmitMoveAndSettle(ctx, "WAL234", hostDigest, hostOwner, domain.Rock); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SubmitMoveAndSettle(ctx, "WAL234", guestDigest, guestOwner, domain.Scissors); err != nil {
		t.Fatal(err)
	}
	guestBalance, _ := repository.Balance(ctx, guestOwner)
	var houseBalance, escrowBalance, historyCount, unbalanced int64
	if err := pool.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id='house'").Scan(&houseBalance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT balance FROM wallet_accounts WHERE account_id='escrow:WAL234:1'").Scan(&escrowBalance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM game_round_history WHERE room_code='WAL234'").Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM (SELECT transaction_id FROM wallet_postings GROUP BY transaction_id HAVING sum(amount) <> 0) unbalanced").Scan(&unbalanced); err != nil {
		t.Fatal(err)
	}
	hostBalance, _ = repository.Balance(ctx, hostOwner)
	if hostBalance != 125 || guestBalance != 0 || houseBalance != 25 || escrowBalance != 0 || historyCount != 1 || unbalanced != 0 {
		t.Fatalf("balances host=%d guest=%d house=%d escrow=%d history=%d unbalanced=%d", hostBalance, guestBalance, houseBalance, escrowBalance, historyCount, unbalanced)
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
	if _, err = pool.Exec(context.Background(), "TRUNCATE web_sessions,command_receipts,room_rooms,game_round_history,wallet_postings,wallet_transactions,wallet_accounts CASCADE; INSERT INTO wallet_accounts(account_id,account_type) VALUES('house','house'),('mint','mint')"); err != nil {
		t.Fatal(err)
	}
	return pool
}
func fixedService(repository application.Repository, events application.Events) *application.Service {
	values := []string{"host-token", "host-id", "guest-token", "guest-id", "guest-token-2", "guest-id-2"}
	var mu sync.Mutex
	next := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		value := values[0]
		values = values[1:]
		return value, nil
	}
	service := application.NewServiceWithGenerators(repository, events, func() (string, error) { return "PGT234", nil }, next, next)
	for index, owner := range []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"} {
		if _, err := service.Recharge(context.Background(), owner, 1_000, fmt.Sprintf("postgres-test-funding-%d", index)); err != nil {
			panic(err)
		}
	}
	return service
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

func insertDeadlineSession(t *testing.T, pool *pgxpool.Pool, accountID string, marker byte, now time.Time) []byte {
	t.Helper()
	digest := make([]byte, 32)
	csrf := make([]byte, 32)
	for index := range digest {
		digest[index] = marker
		csrf[index] = marker + 20
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO web_sessions(session_digest,account_id,csrf_digest,created_at,last_seen_at,idle_expires_at,absolute_expires_at)
		VALUES($1,$2,$3,$4,$4,$5,$6)`, digest, accountID, csrf, now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return digest
}

func createFundedDeadlineRoom(t *testing.T, repository *postgres.Repository, name string) (string, string, string) {
	t.Helper()
	codes := map[string]string{"deadline": "DUE234", "zero": "ZER234", "one": "ONE234", "race": "RCE234", "receipt-cleanup": "RCP234"}
	code := codes[name]
	hostAccount := "00000000-0000-4000-8000-000000000001"
	guestAccount := "00000000-0000-4000-8000-000000000002"
	ctx := context.Background()
	for index, accountID := range []string{hostAccount, guestAccount} {
		if _, err := repository.Recharge(ctx, accountID, 1_000, fmt.Sprintf("%s-funding-%d", name, index)); err != nil {
			t.Fatal(err)
		}
	}
	aggregate, err := domain.New(code, name+"-host")
	if err != nil {
		t.Fatal(err)
	}
	createCommand, _ := application.NewCommand(name+"-create", "room.create", struct{}{})
	if _, err = repository.CreateCommand(ctx, aggregate, hostAccount, createCommand); err != nil {
		t.Fatal(err)
	}
	joinCommand, _ := application.NewCommand(name+"-join", "room.join", struct {
		RoomCode string `json:"room_code"`
	}{code})
	now := time.Now()
	window := application.PresenceWindow{ObservedAt: now, ProofAfter: now.Add(time.Minute), Deadline: now.Add(time.Minute), EvaluateAt: now.Add(2 * time.Minute)}
	if _, err = repository.JoinCommand(ctx, code, name+"-guest", guestAccount, window, joinCommand); err != nil {
		t.Fatal(err)
	}
	return code, hostAccount, guestAccount
}

func forceInactivityDue(t *testing.T, pool *pgxpool.Pool, roomCode string, due time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "UPDATE room_deadlines SET due_at=$2 WHERE room_code=$1 AND kind='inactivity' AND status='pending'", roomCode, due); err != nil {
		t.Fatal(err)
	}
}

func assertDeadlineCount(t *testing.T, pool *pgxpool.Pool, roomCode, kind, status string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM room_deadlines WHERE room_code=$1 AND kind=$2 AND status=$3", roomCode, kind, status).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s %s deadlines = %d, want %d", kind, status, count, want)
	}
}

func assertSingleSettlement(t *testing.T, pool *pgxpool.Pool, roomCode string) {
	t.Helper()
	var settlements int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM wallet_transactions WHERE business_key=$1", "settlement:"+roomCode+":1").Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if settlements != 1 {
		t.Fatalf("settlements = %d, want 1", settlements)
	}
}

type authorityState struct {
	Revision     uint64
	Moves        int
	Outbox       int
	Transactions int
	Postings     int
	History      int
	BalanceTotal int64
}

func readAuthorityState(t *testing.T, pool *pgxpool.Pool, roomCode string) authorityState {
	t.Helper()
	ctx := context.Background()
	var state authorityState
	if err := pool.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1", roomCode).Scan(&state.Revision); err != nil {
		t.Fatal(err)
	}
	queries := []struct {
		query string
		value *int
	}{
		{"SELECT count(*) FROM room_moves WHERE room_code=$1", &state.Moves},
		{"SELECT count(*) FROM room_outbox WHERE room_code=$1", &state.Outbox},
		{"SELECT count(*) FROM wallet_transactions WHERE $1::text IS NOT NULL", &state.Transactions},
		{"SELECT count(*) FROM wallet_postings WHERE $1::text IS NOT NULL", &state.Postings},
		{"SELECT count(*) FROM game_round_history WHERE room_code=$1", &state.History},
	}
	for _, item := range queries {
		if err := pool.QueryRow(ctx, item.query, roomCode).Scan(item.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, "SELECT COALESCE(sum(balance),0) FROM wallet_accounts").Scan(&state.BalanceTotal); err != nil {
		t.Fatal(err)
	}
	return state
}
