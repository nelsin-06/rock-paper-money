package application_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"example.com/rock-paper-money/internal/room/adapter/memory"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

func TestServiceUsesOpaqueCredentialsAndRevisions(t *testing.T) {
	service := testService()
	host, err := service.Create(context.Background(), "host-user")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(context.Background(), host.RoomCode, "guest-user")
	if err != nil {
		t.Fatal(err)
	}
	if host.PlayerToken == guest.PlayerToken {
		t.Fatal("tokens reused")
	}
	if _, _, err = service.Authenticate(context.Background(), host.RoomCode, host.PlayerToken, "host-user"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.Authenticate(context.Background(), host.RoomCode, "wrong", "host-user"); err != nil {
		t.Fatalf("account seat was not authoritative: %v", err)
	}
	if _, _, err = service.Authenticate(context.Background(), host.RoomCode, host.PlayerToken, "other-user"); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("legacy token granted another account access: %v", err)
	}
	if err = service.SubmitMove(context.Background(), host.RoomCode, host.PlayerToken, "host-user", domain.Rock); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(context.Background(), host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 3 || len(snapshot.State.Moves) != 0 || !snapshot.State.Players[0].Submitted {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	for _, p := range snapshot.State.Players {
		if p.ID == host.PlayerToken || p.ID == guest.PlayerToken {
			t.Fatal("credential used as player identity")
		}
	}
}

func TestServiceBindsRoomCredentialsToAccountIdentity(t *testing.T) {
	service := testService()
	host, err := service.Create(context.Background(), "host-user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Join(context.Background(), host.RoomCode, "host-user"); !errors.Is(err, application.ErrAccountSeated) {
		t.Fatalf("same-account join error = %v", err)
	}
	if _, _, err = service.Authenticate(context.Background(), host.RoomCode, host.PlayerToken, "other-user"); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("cross-account authentication error = %v", err)
	}
}

func TestCreateRequiresRoundStakeWithoutChargingIt(t *testing.T) {
	service := unfundedTestService()
	ctx := context.Background()

	if _, err := service.Create(ctx, "host-user"); !errors.Is(err, application.ErrInsufficientFunds) {
		t.Fatalf("zero-balance create error = %v", err)
	}
	if _, err := service.Recharge(ctx, "host-user", application.RoundStake-1, "partial-create-funding"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, "host-user"); !errors.Is(err, application.ErrInsufficientFunds) {
		t.Fatalf("underfunded create error = %v", err)
	}
	if _, err := service.Recharge(ctx, "host-user", 1, "final-create-funding"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, "host-user"); err != nil {
		t.Fatal(err)
	}
	if balance, err := service.Balance(ctx, "host-user"); err != nil || balance != application.RoundStake {
		t.Fatalf("balance after create = %d, error = %v; want %d", balance, err, application.RoundStake)
	}
}

func TestPaidRoundsRechargeAndSettlementPolicy(t *testing.T) {
	service := unfundedTestService()
	ctx := context.Background()
	if balance, err := service.Balance(ctx, "host-user"); err != nil || balance != 0 {
		t.Fatalf("initial balance=%d error=%v", balance, err)
	}
	if balance, err := service.Recharge(ctx, "host-user", 100, "host-credit"); err != nil || balance != 100 {
		t.Fatalf("host recharge balance=%d error=%v", balance, err)
	}
	if balance, err := service.Recharge(ctx, "host-user", 100, "host-credit"); err != nil || balance != 100 {
		t.Fatalf("idempotent recharge balance=%d error=%v", balance, err)
	}
	if _, err := service.Recharge(ctx, "host-user", 101, "host-credit"); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("reused idempotency key error=%v", err)
	}
	host, err := service.Create(ctx, "host-user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Recharge(ctx, "guest-user", 49, "guest-partial-credit"); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Join(ctx, host.RoomCode, "guest-user"); !errors.Is(err, application.ErrInsufficientFunds) {
		t.Fatalf("underfunded join error=%v", err)
	}
	state, _ := service.Snapshot(ctx, host.RoomCode)
	if len(state.State.Players) != 1 {
		t.Fatalf("failed funding changed room=%#v", state)
	}
	if balance, _ := service.Balance(ctx, "host-user"); balance != 100 {
		t.Fatalf("failed funding debited host=%d", balance)
	}
	if _, err = service.Recharge(ctx, "guest-user", 1, "guest-final-credit"); err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(ctx, host.RoomCode, "guest-user")
	if err != nil {
		t.Fatal(err)
	}
	if hostBalance, _ := service.Balance(ctx, "host-user"); hostBalance != 50 {
		t.Fatalf("funded host balance=%d", hostBalance)
	}
	if guestBalance, _ := service.Balance(ctx, "guest-user"); guestBalance != 0 {
		t.Fatalf("funded guest balance=%d", guestBalance)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, host.PlayerToken, "host-user", domain.Rock); err != nil {
		t.Fatal(err)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, guest.PlayerToken, "guest-user", domain.Scissors); err != nil {
		t.Fatal(err)
	}
	if hostBalance, _ := service.Balance(ctx, "host-user"); hostBalance != 125 {
		t.Fatalf("winner balance=%d", hostBalance)
	}
	report, err := service.Analytics(ctx, "guest-user")
	if err != nil || report.TotalHouseEarnings != 25 || len(report.Rounds) != 1 || report.Rounds[0].WinnerRole != "host" || report.Rounds[0].HouseEarnings != 25 {
		t.Fatalf("analytics=%#v error=%v", report, err)
	}
	if err = service.RequestNextRound(ctx, host.RoomCode, host.PlayerToken, "host-user", 1); err != nil {
		t.Fatal(err)
	}
	if err = service.RequestNextRound(ctx, host.RoomCode, guest.PlayerToken, "guest-user", 1); !errors.Is(err, application.ErrInsufficientFunds) {
		t.Fatalf("underfunded next round error=%v", err)
	}
	state, _ = service.Snapshot(ctx, host.RoomCode)
	if state.State.Round != 1 || !state.State.Resolved || !state.State.Players[0].WantsNextRound {
		t.Fatalf("failed next-round funding changed consensus=%#v", state)
	}
	if _, err = service.Recharge(ctx, "guest-user", 100, "guest-round-two-credit"); err != nil {
		t.Fatal(err)
	}
	if err = service.RequestNextRound(ctx, host.RoomCode, guest.PlayerToken, "guest-user", 1); err != nil {
		t.Fatal(err)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, host.PlayerToken, "host-user", domain.Rock); err != nil {
		t.Fatal(err)
	}
	if err = service.SubmitMove(ctx, host.RoomCode, guest.PlayerToken, "guest-user", domain.Rock); err != nil {
		t.Fatal(err)
	}
	if hostBalance, _ := service.Balance(ctx, "host-user"); hostBalance != 125 {
		t.Fatalf("draw host balance=%d", hostBalance)
	}
	if guestBalance, _ := service.Balance(ctx, "guest-user"); guestBalance != 100 {
		t.Fatalf("draw guest balance=%d", guestBalance)
	}
	report, err = service.Analytics(ctx, "host-user")
	if err != nil || report.TotalHouseEarnings != 25 || len(report.Rounds) != 2 || report.Rounds[0].Result != domain.Draw || report.Rounds[0].HouseEarnings != 0 {
		t.Fatalf("draw analytics=%#v error=%v", report, err)
	}
}

func TestConcurrentMovesResolveExactlyOnce(t *testing.T) {
	service := testService()
	host, _ := service.Create(context.Background(), "host-user")
	guest, _ := service.Join(context.Background(), host.RoomCode, "guest-user")
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- service.SubmitMove(context.Background(), host.RoomCode, host.PlayerToken, "host-user", domain.Rock)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs <- service.SubmitMove(context.Background(), host.RoomCode, guest.PlayerToken, "guest-user", domain.Scissors)
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := service.Snapshot(context.Background(), host.RoomCode)
	if snapshot.Revision != 4 || snapshot.State.Players[0].Wins != 1 || !snapshot.State.Resolved {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func testService() *application.Service {
	service := unfundedTestService()
	fundTestPlayers(nil, service)
	return service
}

func unfundedTestService() *application.Service {
	repository, events := memory.New()
	values := []string{"HOSTTOKEN", "host-id", "GUESTTOKEN", "guest-id", "GUESTTOKEN2", "guest-id-2"}
	var mu sync.Mutex
	generator := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		value := values[0]
		values = values[1:]
		return value, nil
	}
	return application.NewServiceWithGenerators(repository, events, func() (string, error) { return "ABC234", nil }, generator, generator)
}

func fundTestPlayers(t *testing.T, service *application.Service) {
	if t != nil {
		t.Helper()
	}
	for index, owner := range []string{"host-user", "guest-user"} {
		if _, err := service.Recharge(context.Background(), owner, 1_000, fmt.Sprintf("test-funding-%d", index)); err != nil {
			if t != nil {
				t.Fatal(err)
			}
			panic(err)
		}
	}
}
