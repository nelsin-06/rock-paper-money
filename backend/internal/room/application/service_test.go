package application_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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
	if _, _, err = service.Authenticate(context.Background(), host.RoomCode, "wrong", "host-user"); !errors.Is(err, application.ErrUnauthorized) {
		t.Fatalf("error = %v", err)
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

func TestSubscriptionCoalescesChanges(t *testing.T) {
	service := testService()
	host, _ := service.Create(context.Background(), "host-user")
	initial, changes, unsubscribe, err := service.Subscribe(context.Background(), host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	if initial.Revision != 1 {
		t.Fatalf("revision = %d", initial.Revision)
	}
	_, _ = service.Join(context.Background(), host.RoomCode, "guest-user")
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("missing notification")
	}
}

func TestPresenceWaitsFullGraceAndAwardsRefreshingOpponent(t *testing.T) {
	service, scheduler, host, _ := newTimedGame(t)
	scheduler.advance(4 * time.Second)
	if err := service.RefreshPresence(context.Background(), host.RoomCode, host.PlayerToken, "host-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(6 * time.Second)
	if err := service.RefreshPresence(context.Background(), host.RoomCode, host.PlayerToken, "host-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(2900 * time.Millisecond)
	assertUnresolved(t, service, host.RoomCode)
	scheduler.advance(2100 * time.Millisecond)
	assertUnresolved(t, service, host.RoomCode)
	if err := service.RefreshPresence(context.Background(), host.RoomCode, host.PlayerToken, "host-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(time.Second)

	snapshot, err := service.Snapshot(context.Background(), host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.State.Resolved || !snapshot.State.Forfeit || snapshot.State.Result != domain.PlayerOneWins || snapshot.State.Players[0].Wins != 1 {
		t.Fatalf("forfeit snapshot = %#v", snapshot)
	}
	report, err := service.Analytics(context.Background(), "host-user")
	if err != nil || report.TotalHouseEarnings != 25 || len(report.Rounds) != 1 || !report.Rounds[0].Forfeit {
		t.Fatalf("forfeit analytics=%#v error=%v", report, err)
	}
	if application.PresenceHeartbeatInterval != 3*time.Second || application.DefaultPresenceGracePeriod != 10*time.Second {
		t.Fatalf("presence timing = heartbeat %v grace %v", application.PresenceHeartbeatInterval, application.DefaultPresenceGracePeriod)
	}
}

func TestReconnectInvalidatesStaleExpiry(t *testing.T) {
	service, scheduler, host, guest := newTimedGame(t)
	staleGuestTimer := scheduler.timers[1]
	scheduler.advance(12 * time.Second)
	if err := service.RefreshPresence(context.Background(), host.RoomCode, host.PlayerToken, "host-user"); err != nil {
		t.Fatal(err)
	}
	if err := service.RefreshPresence(context.Background(), host.RoomCode, guest.PlayerToken, "guest-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(4 * time.Second)
	staleGuestTimer.run()
	assertUnresolved(t, service, host.RoomCode)
}

func TestReconnectAfterBothPlayersExpireReevaluatesAbsentOpponent(t *testing.T) {
	service, scheduler, host, _ := newTimedGame(t)
	scheduler.advance(16 * time.Second)
	assertUnresolved(t, service, host.RoomCode)

	if err := service.RefreshPresence(context.Background(), host.RoomCode, host.PlayerToken, "host-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(0)

	snapshot, err := service.Snapshot(context.Background(), host.RoomCode)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.State.Resolved || !snapshot.State.Forfeit || snapshot.State.Result != domain.PlayerOneWins || snapshot.State.Players[0].Wins != 1 {
		t.Fatalf("reconnected host forfeit = %#v", snapshot)
	}
}

func TestJoinActivatesBothPlayersWithoutAnotherHostHeartbeat(t *testing.T) {
	repository, events := memory.New()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	scheduler := &fakeScheduler{now: &now}
	service := configuredTestService(repository, events, &now, scheduler, nil)
	fundTestPlayers(t, service)
	host, _ := service.Create(context.Background(), "host-user")
	if err := service.RefreshPresence(context.Background(), host.RoomCode, host.PlayerToken, "host-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(time.Second)
	guest, err := service.Join(context.Background(), host.RoomCode, "guest-user")
	if err != nil {
		t.Fatal(err)
	}
	scheduler.advance(4 * time.Second)
	if err = service.RefreshPresence(context.Background(), host.RoomCode, guest.PlayerToken, "guest-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(6 * time.Second)
	if err = service.RefreshPresence(context.Background(), host.RoomCode, guest.PlayerToken, "guest-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(4 * time.Second)
	if err = service.RefreshPresence(context.Background(), host.RoomCode, guest.PlayerToken, "guest-user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(2 * time.Second)

	snapshot, _ := service.Snapshot(context.Background(), host.RoomCode)
	if !snapshot.State.Resolved || snapshot.State.Result != domain.PlayerTwoWins || snapshot.State.Players[1].Wins != 1 {
		t.Fatalf("join-activated host forfeit = %#v", snapshot)
	}
}

func TestOutOfOrderRefreshCannotReplaceNewerTimer(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	scheduler := &fakeScheduler{now: &now}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	var expiredGeneration uint64
	repository := &presenceRepositoryStub{}
	repository.refresh = func(context.Context, string, application.CredentialDigest, application.PresenceWindow) ([]application.PresenceLease, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return []application.PresenceLease{{RoomCode: "ABC234", PlayerID: "host-id", Round: 1, Generation: uint64(call), Deadline: now.Add(13 * time.Second), EvaluateAt: now.Add(16 * time.Second), Active: true}}, nil
	}
	repository.forfeit = func(_ context.Context, lease application.PresenceLease, _ time.Time) (application.Snapshot, bool, error) {
		expiredGeneration = lease.Generation
		return application.Snapshot{}, false, nil
	}
	service := configuredTestService(repository, nil, &now, scheduler, nil)
	firstResult := make(chan error, 1)
	go func() { firstResult <- service.RefreshPresence(context.Background(), "ABC234", "token", "user") }()
	<-firstStarted
	if err := service.RefreshPresence(context.Background(), "ABC234", "token", "user"); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if len(scheduler.timers) != 1 {
		t.Fatalf("scheduled timers = %d, want 1", len(scheduler.timers))
	}
	scheduler.advance(16 * time.Second)
	if expiredGeneration != 2 {
		t.Fatalf("expired generation = %d, want 2", expiredGeneration)
	}
}

func TestNewRoundAcceptsLowerGenerationFromNewLeaseEpoch(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	scheduler := &fakeScheduler{now: &now}
	repository := &presenceRepositoryStub{}
	calls := 0
	repository.refresh = func(_ context.Context, _ string, _ application.CredentialDigest, window application.PresenceWindow) ([]application.PresenceLease, error) {
		calls++
		if calls == 1 {
			return []application.PresenceLease{{RoomCode: "ABC234", PlayerID: "host-id", Round: 1, Generation: 100, Deadline: window.Deadline, EvaluateAt: window.EvaluateAt, Active: true}}, nil
		}
		return []application.PresenceLease{{RoomCode: "ABC234", PlayerID: "host-id", Round: 2, Generation: 1, Deadline: window.Deadline, EvaluateAt: window.EvaluateAt, Active: true}}, nil
	}
	var expired application.PresenceLease
	repository.forfeit = func(_ context.Context, lease application.PresenceLease, _ time.Time) (application.Snapshot, bool, error) {
		expired = lease
		return application.Snapshot{}, false, nil
	}
	service := configuredTestService(repository, nil, &now, scheduler, nil)
	if err := service.RefreshPresence(context.Background(), "ABC234", "token", "user"); err != nil {
		t.Fatal(err)
	}
	if err := service.RefreshPresence(context.Background(), "ABC234", "token", "user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(16 * time.Second)
	if expired.Round != 2 || expired.Generation != 1 {
		t.Fatalf("expired lease = %#v, want round 2 generation 1", expired)
	}
}

func TestExpiryFailureRetriesAreBoundedAndReported(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	scheduler := &fakeScheduler{now: &now}
	repository := &presenceRepositoryStub{}
	repository.refresh = func(_ context.Context, _ string, _ application.CredentialDigest, window application.PresenceWindow) ([]application.PresenceLease, error) {
		return []application.PresenceLease{{RoomCode: "ABC234", PlayerID: "host-id", Round: 1, Generation: 1, Deadline: window.Deadline, EvaluateAt: window.EvaluateAt, Active: true}}, nil
	}
	attempts := 0
	repository.forfeit = func(context.Context, application.PresenceLease, time.Time) (application.Snapshot, bool, error) {
		attempts++
		if attempts < 3 {
			return application.Snapshot{}, false, errors.New("temporary repository failure")
		}
		return application.Snapshot{}, false, nil
	}
	var reported []error
	service := configuredTestService(repository, nil, &now, scheduler, func(err error) { reported = append(reported, err) })
	if err := service.RefreshPresence(context.Background(), "ABC234", "token", "user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(16 * time.Second)
	scheduler.advance(time.Second)
	scheduler.advance(time.Second)
	if attempts != 3 || len(reported) != 2 {
		t.Fatalf("attempts=%d reported=%d, want 3 and 2", attempts, len(reported))
	}
}

func TestExpiryFailureStopsAfterRetryLimit(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	scheduler := &fakeScheduler{now: &now}
	repository := &presenceRepositoryStub{}
	repository.refresh = func(_ context.Context, _ string, _ application.CredentialDigest, window application.PresenceWindow) ([]application.PresenceLease, error) {
		return []application.PresenceLease{{RoomCode: "ABC234", PlayerID: "host-id", Round: 1, Generation: 1, Deadline: window.Deadline, EvaluateAt: window.EvaluateAt, Active: true}}, nil
	}
	attempts := 0
	repository.forfeit = func(context.Context, application.PresenceLease, time.Time) (application.Snapshot, bool, error) {
		attempts++
		return application.Snapshot{}, false, errors.New("persistent repository failure")
	}
	reported := 0
	service := configuredTestService(repository, nil, &now, scheduler, func(error) { reported++ })
	if err := service.RefreshPresence(context.Background(), "ABC234", "token", "user"); err != nil {
		t.Fatal(err)
	}
	scheduler.advance(16 * time.Second)
	scheduler.advance(time.Second)
	scheduler.advance(time.Second)
	scheduler.advance(time.Hour)
	if attempts != 3 || reported != 3 {
		t.Fatalf("attempts=%d reported=%d, want bounded 3 and 3", attempts, reported)
	}
}

func newTimedGame(t *testing.T) (*application.Service, *fakeScheduler, application.Credentials, application.Credentials) {
	t.Helper()
	repository, events := memory.New()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	scheduler := &fakeScheduler{now: &now}
	service := configuredTestService(repository, events, &now, scheduler, nil)
	fundTestPlayers(t, service)
	host, err := service.Create(context.Background(), "host-user")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := service.Join(context.Background(), host.RoomCode, "guest-user")
	if err != nil {
		t.Fatal(err)
	}
	return service, scheduler, host, guest
}

func configuredTestService(repository application.Repository, events application.Events, now *time.Time, scheduler *fakeScheduler, report func(error)) *application.Service {
	values := []string{"HOSTTOKEN", "host-id", "GUESTTOKEN", "guest-id", "GUESTTOKEN2", "guest-id-2"}
	generator := func() (string, error) {
		value := values[0]
		values = values[1:]
		return value, nil
	}
	return application.NewServiceWithPresenceConfig(repository, events, func() (string, error) { return "ABC234", nil }, generator, generator, application.PresenceConfig{Now: func() time.Time { return *now }, Schedule: scheduler.schedule, GracePeriod: 10 * time.Second, HeartbeatInterval: 3 * time.Second, RetryDelay: time.Second, MaxRetries: 2, ReportError: report})
}

type presenceRepositoryStub struct {
	application.Repository
	refresh func(context.Context, string, application.CredentialDigest, application.PresenceWindow) ([]application.PresenceLease, error)
	forfeit func(context.Context, application.PresenceLease, time.Time) (application.Snapshot, bool, error)
}

func (r *presenceRepositoryStub) RefreshPresence(ctx context.Context, code string, digest application.CredentialDigest, _ string, window application.PresenceWindow) ([]application.PresenceLease, error) {
	return r.refresh(ctx, code, digest, window)
}

func (r *presenceRepositoryStub) ForfeitExpired(ctx context.Context, lease application.PresenceLease, now time.Time) (application.Snapshot, bool, error) {
	return r.forfeit(ctx, lease, now)
}

func assertUnresolved(t *testing.T, service *application.Service, code string) {
	t.Helper()
	snapshot, err := service.Snapshot(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State.Resolved {
		t.Fatalf("room resolved before grace: %#v", snapshot)
	}
}

type fakeTimer struct {
	due     time.Time
	run     func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

type fakeScheduler struct {
	now    *time.Time
	timers []*fakeTimer
}

func (s *fakeScheduler) schedule(delay time.Duration, run func()) application.Timer {
	timer := &fakeTimer{due: s.now.Add(delay), run: run}
	s.timers = append(s.timers, timer)
	return timer
}

func (s *fakeScheduler) advance(duration time.Duration) {
	*s.now = s.now.Add(duration)
	for _, timer := range s.timers {
		if !timer.stopped && !timer.due.After(*s.now) {
			timer.stopped = true
			timer.run()
		}
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
