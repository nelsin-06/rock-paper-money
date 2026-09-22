package realtime

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/auth"
	roomhttp "example.com/rock-paper-money/internal/room/adapter/http"
	roompostgres "example.com/rock-paper-money/internal/room/adapter/postgres"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
	"example.com/rock-paper-money/internal/worker"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
)

const testRoom = "ROOM77"

func TestWebSocketAdmissionRequiresExactOriginSessionAndCurrentSeat(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		authorized bool
		wantStatus int
	}{
		{name: "authorized player", authorized: true},
		{name: "missing origin", authorized: true, wantStatus: http.StatusForbidden},
		{name: "foreign origin", origin: "https://attacker.example", authorized: true, wantStatus: http.StatusForbidden},
		{name: "non player", origin: "allowed", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			harness := newWebSocketHarness(t, WebSocketConfig{RevalidateInterval: time.Hour})
			harness.repository.authorized = tt.authorized
			origin := tt.origin
			if origin == "allowed" || (origin == "" && tt.wantStatus == 0) {
				origin = harness.server.URL
			}
			connection, response, err := harness.dial(origin)
			if tt.wantStatus != 0 {
				if err == nil || response == nil || response.StatusCode != tt.wantStatus {
					t.Fatalf("dial error=%v status=%v, want %d", err, status(response), tt.wantStatus)
				}
				if harness.repository.opens != 0 || harness.subscriber.subscribeCalls != 0 {
					t.Fatalf("rejected admission changed presence or subscribed: opens=%d subscriptions=%d", harness.repository.opens, harness.subscriber.subscribeCalls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer connection.CloseNow()
			frame := readFrame(t, connection, time.Second)
			if frame.Type != "snapshot" || frame.Revision != 1 || frame.Room.Code != testRoom {
				t.Fatalf("initial frame = %#v", frame)
			}
			if frame.Player.AccountID != "player-1" || frame.Player.Role != "host" {
				t.Fatalf("authoritative player identity = %#v", frame.Player)
			}
			if !harness.subscriber.confirmedBeforeReturn || harness.repository.loadBeforeSubscription {
				t.Fatalf("snapshot loaded before confirmed subscription")
			}
			if harness.repository.opens != 1 {
				t.Fatalf("connection leases opened = %d, want 1", harness.repository.opens)
			}
		})
	}
}

func TestWebSocketAdmissionRejectsRevokedSessionBeforeRoomAccess(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{})
	harness.sessions.revoke()
	connection, response, err := harness.dial(harness.server.URL)
	if connection != nil {
		connection.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dial error=%v status=%v, want 401", err, status(response))
	}
	if harness.subscriber.subscribeCalls != 0 || harness.repository.opens != 0 {
		t.Fatal("revoked admission reached protected room infrastructure")
	}
}

func TestFailedWebSocketUpgradeHasNoDurableEffects(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{})
	request, err := http.NewRequest(http.MethodGet, harness.server.URL+"/api/rooms/"+testRoom+"/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", harness.server.URL)
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: harness.token})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("non-upgrade request unexpectedly switched protocols")
	}
	closes, active := harness.repository.connectionCounts()
	if harness.repository.opens != 0 || closes != 0 || active != 0 || harness.subscriber.subscribeCalls != 0 {
		t.Fatalf("failed upgrade effects: opens=%d closes=%d active=%d subscriptions=%d", harness.repository.opens, closes, active, harness.subscriber.subscribeCalls)
	}
}

func TestWebSocketLifecycleShutdownClosesActiveSocketAndDurableLease(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	harness := newWebSocketHarness(t, WebSocketConfig{LifecycleContext: root, RevalidateInterval: time.Hour})
	connection, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	_ = readFrame(t, connection, time.Second)

	cancelRoot()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err = harness.handler.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	closes, active := harness.repository.connectionCounts()
	if closes != 1 || active != 0 {
		t.Fatalf("shutdown returned before durable close: closes=%d active=%d", closes, active)
	}
	readContext, cancelRead := context.WithTimeout(context.Background(), time.Second)
	defer cancelRead()
	if _, _, err = connection.Read(readContext); err == nil {
		t.Fatal("active socket remained open after lifecycle shutdown")
	}
}

func TestWebSocketShutdownReportsDurableCloseFailureAfterCleanupJoins(t *testing.T) {
	want := errors.New("durable close failed")
	harness := newWebSocketHarness(t, WebSocketConfig{RevalidateInterval: time.Hour})
	harness.repository.closeErr = want
	harness.repository.closeFinished = make(chan struct{}, 1)
	connection, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	_ = readFrame(t, connection, time.Second)

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	err = harness.handler.Shutdown(shutdownContext)
	if !errors.Is(err, want) {
		t.Fatalf("Shutdown() error = %v, want durable close failure", err)
	}
	select {
	case <-harness.repository.closeFinished:
	default:
		t.Fatal("Shutdown returned before close cleanup completed")
	}
	closes, _ := harness.repository.connectionCounts()
	if closes != 1 {
		t.Fatalf("close attempts = %d, want 1", closes)
	}
}

func TestWebSocketShutdownJoinsCleanupAfterCallerDeadline(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{OperationTimeout: 100 * time.Millisecond, RevalidateInterval: time.Hour})
	harness.repository.closeStarted = make(chan struct{}, 1)
	harness.repository.closeRelease = make(chan struct{})
	harness.repository.closeFinished = make(chan struct{}, 1)
	connection, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	_ = readFrame(t, connection, time.Second)

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelShutdown()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- harness.handler.Shutdown(shutdownContext) }()
	select {
	case <-harness.repository.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("close cleanup did not start")
	}
	<-shutdownContext.Done()
	select {
	case err = <-shutdownResult:
		t.Fatalf("Shutdown returned before cleanup joined: %v", err)
	default:
	}
	select {
	case err = <-shutdownResult:
	case <-time.After(time.Second):
		t.Fatal("internally bounded cleanup did not complete")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline exceeded", err)
	}
	select {
	case <-harness.repository.closeFinished:
	default:
		t.Fatal("Shutdown returned before close cleanup completed")
	}
}

func TestWebSocketShutdownDoesNotRepeatNormalDisconnectCleanup(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{RevalidateInterval: time.Hour})
	connection, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = readFrame(t, connection, time.Second)
	if err = connection.Close(websocket.StatusNormalClosure, "test disconnect"); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool {
		closes, _ := harness.repository.connectionCounts()
		return closes == 1
	})

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err = harness.handler.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	closes, active := harness.repository.connectionCounts()
	if closes != 1 || active != 0 {
		t.Fatalf("normal disconnect cleanup repeated: closes=%d active=%d", closes, active)
	}
}

func TestWebSocketSetupFailureCleansOnlyCreatedState(t *testing.T) {
	t.Run("before lease creation", func(t *testing.T) {
		harness := newWebSocketHarness(t, WebSocketConfig{NewConnectionID: func() (string, error) {
			return "", errors.New("connection ID unavailable")
		}})
		connection, _, err := harness.dial(harness.server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.CloseNow()
		readContext, cancelRead := context.WithTimeout(context.Background(), time.Second)
		defer cancelRead()
		if _, _, err = connection.Read(readContext); err == nil {
			t.Fatal("socket remained open after setup failure")
		}
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
		defer cancelShutdown()
		if err = harness.handler.Shutdown(shutdownContext); err != nil {
			t.Fatal(err)
		}
		closes, active := harness.repository.connectionCounts()
		if harness.repository.opens != 0 || closes != 0 || active != 0 || harness.subscriber.closed != 1 {
			t.Fatalf("pre-lease cleanup: opens=%d closes=%d active=%d subscriptions_closed=%d", harness.repository.opens, closes, active, harness.subscriber.closed)
		}
	})

	t.Run("after lease creation", func(t *testing.T) {
		harness := newWebSocketHarness(t, WebSocketConfig{})
		harness.repository.failLoadAt = 2
		connection, _, err := harness.dial(harness.server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.CloseNow()
		readContext, cancelRead := context.WithTimeout(context.Background(), time.Second)
		defer cancelRead()
		if _, _, err = connection.Read(readContext); err == nil {
			t.Fatal("socket remained open after reconciliation failure")
		}
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
		defer cancelShutdown()
		if err = harness.handler.Shutdown(shutdownContext); err != nil {
			t.Fatal(err)
		}
		closes, active := harness.repository.connectionCounts()
		if harness.repository.opens != 1 || closes != 1 || active != 0 || harness.subscriber.closed != 1 {
			t.Fatalf("post-lease cleanup: opens=%d closes=%d active=%d subscriptions_closed=%d", harness.repository.opens, closes, active, harness.subscriber.closed)
		}
	})
}

func TestWebSocketRevalidatesSessionAndSeatBeforeDelivery(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{
		RevalidateInterval: 20 * time.Millisecond,
		LeaseDuration:      50 * time.Millisecond,
		OperationTimeout:   100 * time.Millisecond,
	})
	connection, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	_ = readFrame(t, connection, time.Second)

	harness.sessions.revoke()
	harness.repository.setSnapshot(2)
	harness.subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: testRoom, Revision: 2}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err = connection.Read(ctx)
	if err == nil {
		t.Fatal("revoked socket received a protected frame")
	}
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v, want policy violation; err=%v", websocket.CloseStatus(err), err)
	}
	eventually(t, time.Second, func() bool {
		harness.repository.mu.Lock()
		defer harness.repository.mu.Unlock()
		return harness.repository.closes == 1
	})
	if harness.repository.renews == 0 || harness.repository.closes != 1 {
		t.Fatalf("lease renews=%d closes=%d", harness.repository.renews, harness.repository.closes)
	}
}

func TestWebSocketClosesAfterSeatRemovalWithoutFurtherDelivery(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{RevalidateInterval: 20 * time.Millisecond, LeaseDuration: 50 * time.Millisecond})
	connection, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	_ = readFrame(t, connection, time.Second)
	harness.repository.mu.Lock()
	harness.repository.authorized = false
	harness.repository.mu.Unlock()
	harness.repository.setSnapshot(2)
	harness.subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: testRoom, Revision: 2}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err = connection.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v, want policy violation; err=%v", websocket.CloseStatus(err), err)
	}
}

func TestWebSocketMultiSocketLifecycleAndMonotonicGapReconciliation(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{RevalidateInterval: time.Hour})
	first, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.CloseNow()
	_ = readFrame(t, first, time.Second)
	_ = readFrame(t, second, time.Second)
	if harness.repository.opens != 2 || harness.subscriber.subscribeCalls != 1 {
		t.Fatalf("opens=%d subscriptions=%d", harness.repository.opens, harness.subscriber.subscribeCalls)
	}

	_ = first.Close(websocket.StatusNormalClosure, "test close")
	eventually(t, time.Second, func() bool { closes, _ := harness.repository.connectionCounts(); return closes == 1 })
	_, active := harness.repository.connectionCounts()
	if active != 1 {
		t.Fatalf("active sockets after one close = %d, want 1", active)
	}

	harness.repository.setSnapshot(3)
	harness.subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: testRoom, Revision: 3}})
	frame := readFrame(t, second, time.Second)
	if frame.Revision != 3 {
		t.Fatalf("gap reconciliation revision = %d, want 3", frame.Revision)
	}
	harness.subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: testRoom, Revision: 2}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, readErr := second.Read(ctx); readErr == nil {
		t.Fatal("stale revision produced another frame")
	}
}

func TestWebSocketRejectsMutationAndOversizedFramesWithoutBusinessEffects(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		limit   int64
		status  websocket.StatusCode
	}{
		{name: "mutation", payload: `{"type":"move","move":"rock"}`, limit: 128, status: websocket.StatusPolicyViolation},
		{name: "oversized", payload: "0123456789abcdef", limit: 8, status: websocket.StatusMessageTooBig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			harness := newWebSocketHarness(t, WebSocketConfig{ReadLimit: tt.limit, RevalidateInterval: time.Hour})
			connection, _, err := harness.dial(harness.server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.CloseNow()
			_ = readFrame(t, connection, time.Second)
			if err = connection.Write(context.Background(), websocket.MessageText, []byte(tt.payload)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, err = connection.Read(ctx)
			if websocket.CloseStatus(err) != tt.status {
				t.Fatalf("close status=%v, want %v; err=%v", websocket.CloseStatus(err), tt.status, err)
			}
			if harness.repository.businessEffects != 0 {
				t.Fatalf("business effects = %d", harness.repository.businessEffects)
			}
		})
	}
}

func TestSnapshotQueueCoalescesToLatestAndRegistryIsBounded(t *testing.T) {
	queue := newSnapshotQueue()
	queue.Offer(application.Snapshot{Revision: 1})
	queue.Offer(application.Snapshot{Revision: 2})
	queue.Offer(application.Snapshot{Revision: 3})
	if snapshot := <-queue.C(); snapshot.Revision != 3 {
		t.Fatalf("coalesced revision = %d, want 3", snapshot.Revision)
	}

	harness := newWebSocketHarness(t, WebSocketConfig{MaxConnections: 1, RevalidateInterval: time.Hour})
	first, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.CloseNow()
	_ = readFrame(t, first, time.Second)
	second, response, err := harness.dial(harness.server.URL)
	if second != nil {
		second.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second dial error=%v status=%v", err, status(response))
	}
}

func TestWebSocketWriteDeadlineClosesBackpressuredConnection(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{WriteTimeout: 5 * time.Millisecond, RevalidateInterval: time.Hour})
	players := make([]domain.Player, 200_000)
	harness.repository.mu.Lock()
	harness.repository.snapshot.State.Players = players
	harness.repository.mu.Unlock()
	connection, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	eventually(t, 2*time.Second, func() bool { closes, _ := harness.repository.connectionCounts(); return closes == 1 })
}

func TestWebSocketPingPongKeepsResponsiveIdleConnectionAlive(t *testing.T) {
	harness := newWebSocketHarness(t, WebSocketConfig{PingInterval: 10 * time.Millisecond, WriteTimeout: 50 * time.Millisecond, RevalidateInterval: time.Hour})
	connection, _, err := harness.dial(harness.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	_ = readFrame(t, connection, time.Second)
	time.AfterFunc(50*time.Millisecond, func() {
		harness.repository.setSnapshot(2)
		harness.subscriber.emit(SubscriptionEvent{Hint: &RevisionHint{RoomID: testRoom, Revision: 2}})
	})
	if frame := readFrame(t, connection, time.Second); frame.Revision != 2 {
		t.Fatalf("post-ping revision = %d, want 2", frame.Revision)
	}
}

func TestPostgresRedisWebSocketRuntime(t *testing.T) {
	if os.Getenv("WU5_RUNTIME") != "1" {
		t.Skip("WU5_RUNTIME is not enabled")
	}
	databaseURL, redisAddress := os.Getenv("TEST_DATABASE_URL"), os.Getenv("REDIS_ADDR")
	if databaseURL == "" || redisAddress == "" {
		t.Fatal("TEST_DATABASE_URL and REDIS_ADDR are required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = roompostgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repository := roompostgres.NewRepository(pool)
	rooms := application.NewService(repository, nil)
	runID, err := randomConnectionID()
	if err != nil {
		t.Fatal(err)
	}
	accountID := "11111111-1111-4111-8111-111111111111"
	if _, err = rooms.Recharge(ctx, accountID, 1_000, "wu5-runtime-funding-"+runID); err != nil {
		t.Fatal(err)
	}
	created, err := rooms.CreateCommand(ctx, accountID, "wu5-runtime-room-"+runID)
	if err != nil {
		t.Fatal(err)
	}
	guestAccount := "22222222-2222-4222-8222-222222222222"
	if _, err = rooms.Recharge(ctx, guestAccount, 1_000, "wu5-runtime-guest-funding-"+runID); err != nil {
		t.Fatal(err)
	}
	if _, err = rooms.JoinCommand(ctx, created.RoomCode, guestAccount, "wu5-runtime-join-"+runID); err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessionService(auth.NewPostgresSessionStore(pool), nil, nil)
	credentials, err := sessions.Create(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeSession, err := sessions.Authenticate(ctx, credentials.Token)
	if err != nil {
		t.Fatal(err)
	}
	type instance struct {
		transport *RedisTransport
		dial      func() *websocket.Conn
		url       string
	}
	newInstance := func() instance {
		redisClient := redis.NewClient(&redis.Options{Addr: redisAddress})
		t.Cleanup(func() { _ = redisClient.Close() })
		transport := NewRedisTransport(redisClient, 20*time.Millisecond)
		transport.healthInterval = 20 * time.Millisecond
		hub := NewHub(transport, repository)
		var handler http.Handler
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
		t.Cleanup(server.Close)
		security, securityErr := auth.NewRequestSecurity(server.URL, sessions)
		if securityErr != nil {
			t.Fatal(securityErr)
		}
		socket := NewWebSocketHandler(security, sessions, repository, hub, WebSocketConfig{RevalidateInterval: 100 * time.Millisecond, LeaseDuration: 300 * time.Millisecond})
		handler = roomhttp.NewSessionRouterWithWebSocket(rooms, nil, sessions, security, nil, nil, socket)
		return instance{transport: transport, url: server.URL, dial: func() *websocket.Conn {
			header := http.Header{"Origin": []string{server.URL}, "Cookie": []string{(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token}).String()}}
			connection, _, dialErr := websocket.Dial(ctx, "ws"+server.URL[4:]+"/api/rooms/"+created.RoomCode+"/socket", &websocket.DialOptions{HTTPHeader: header})
			if dialErr != nil {
				t.Fatal(dialErr)
			}
			return connection
		}}
	}
	firstInstance, secondInstance := newInstance(), newInstance()
	first, second := firstInstance.dial(), secondInstance.dial()
	defer first.CloseNow()
	defer second.CloseNow()
	initial := readFrame(t, first, time.Second)
	if initial.Player.AccountID != accountID || initial.Player.Role != "host" {
		t.Fatalf("runtime player identity = %#v", initial.Player)
	}
	_ = readFrame(t, second, time.Second)
	mutation, err := http.NewRequest(http.MethodPost, firstInstance.url+"/api/rooms/"+created.RoomCode+"/moves", strings.NewReader(`{"move":"rock"}`))
	if err != nil {
		t.Fatal(err)
	}
	mutation.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token})
	mutation.Header.Set("Origin", firstInstance.url)
	mutation.Header.Set(auth.CSRFHeaderName, credentials.CSRFToken)
	mutationKey := "runtime-rest-to-websocket-move-" + runID
	mutation.Header.Set("Idempotency-Key", mutationKey)
	response, err := http.DefaultClient.Do(mutation)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("REST mutation status=%d", response.StatusCode)
	}
	var revision uint64
	var receipt, matchingOutbox int
	if err = pool.QueryRow(ctx, "SELECT revision FROM room_rooms WHERE code=$1", created.RoomCode).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM command_receipts WHERE idempotency_key=$1", mutationKey).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM room_outbox WHERE room_code=$1 AND revision=$2", created.RoomCode, revision).Scan(&matchingOutbox); err != nil {
		t.Fatal(err)
	}
	if receipt != 1 || matchingOutbox != 1 {
		t.Fatalf("committed mutation receipt=%d matching_outbox=%d revision=%d", receipt, matchingOutbox, revision)
	}
	dispatcher := worker.NewOutboxWorker(repository, firstInstance.transport, worker.OutboxConfig{Now: time.Now, BatchSize: 100})
	report, err := dispatcher.RunOnce(ctx)
	if err != nil || report.Published == 0 {
		t.Fatalf("outbox dispatch report=%#v error=%v", report, err)
	}
	if reconciled := readFrameAtLeast(t, first, revision, 5*time.Second); reconciled.Revision != revision || reconciled.Revision <= initial.Revision || len(reconciled.Room.Players) != 2 || !reconciled.Room.Players[0].Submitted {
		t.Fatalf("REST-to-socket snapshot=%#v initial=%d authoritative=%d", reconciled, initial.Revision, revision)
	}
	if reconciled := readFrameAtLeast(t, second, revision, 5*time.Second); reconciled.Revision != revision || !reconciled.Room.Players[0].Submitted {
		t.Fatalf("second socket snapshot=%#v authoritative=%d", reconciled, revision)
	}
	_ = first.Close(websocket.StatusNormalClosure, "runtime close")
	eventually(t, time.Second, func() bool {
		var active int
		return pool.QueryRow(ctx, "SELECT count(*) FROM room_connections WHERE room_code=$1 AND session_digest=$2 AND closed_at IS NULL", created.RoomCode, runtimeSession.Digest[:]).Scan(&active) == nil && active == 1
	})
	if err = sessions.Revoke(ctx, credentials.Token); err != nil {
		t.Fatal(err)
	}
	closeContext, cancelClose := context.WithTimeout(ctx, 2*time.Second)
	defer cancelClose()
	if _, _, err = second.Read(closeContext); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("revoked runtime socket close=%v err=%v", websocket.CloseStatus(err), err)
	}
	eventually(t, time.Second, func() bool {
		var active int
		return pool.QueryRow(ctx, "SELECT count(*) FROM room_connections WHERE room_code=$1 AND session_digest=$2 AND closed_at IS NULL", created.RoomCode, runtimeSession.Digest[:]).Scan(&active) == nil && active == 0
	})
}

type webSocketHarness struct {
	server     *httptest.Server
	handler    *WebSocketHandler
	sessions   *sessionStoreStub
	repository *socketRepositoryStub
	subscriber *subscriberStub
	token      string
}

func newWebSocketHarness(t *testing.T, config WebSocketConfig) *webSocketHarness {
	t.Helper()
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	sessions := &sessionStoreStub{session: auth.Session{AccountID: "player-1", Digest: auth.Digest{1}}}
	service := auth.NewSessionService(sessions, nil, time.Now)
	repository := &socketRepositoryStub{authorized: true, snapshot: testSnapshot(1)}
	subscriber := &subscriberStub{}
	repository.subscribed = func() bool { return subscriber.confirmedBeforeReturn }
	hub := NewHub(subscriber, repository)
	var handler http.Handler
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	security, err := auth.NewRequestSecurity(server.URL, service)
	if err != nil {
		t.Fatal(err)
	}
	socket := NewWebSocketHandler(security, service, repository, hub, config)
	mux := http.NewServeMux()
	mux.Handle("GET /api/rooms/{code}/socket", socket)
	handler = mux
	t.Cleanup(server.Close)
	return &webSocketHarness{server: server, handler: socket, sessions: sessions, repository: repository, subscriber: subscriber, token: token}
}

func (h *webSocketHarness) dial(origin string) (*websocket.Conn, *http.Response, error) {
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	header.Set("Cookie", (&http.Cookie{Name: auth.SessionCookieName, Value: h.token}).String())
	return websocket.Dial(context.Background(), "ws"+h.server.URL[4:]+"/api/rooms/"+testRoom+"/socket", &websocket.DialOptions{HTTPHeader: header})
}

type sessionStoreStub struct {
	mu      sync.Mutex
	session auth.Session
	revoked bool
}

func (s *sessionStoreStub) CreateSession(context.Context, auth.Session) error { return nil }
func (s *sessionStoreStub) UseSession(context.Context, auth.Digest, time.Time, time.Time) (auth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked {
		return auth.Session{}, auth.ErrInvalidSession
	}
	return s.session, nil
}
func (s *sessionStoreStub) RevokeSession(context.Context, auth.Digest, time.Time) error {
	s.revoke()
	return nil
}
func (s *sessionStoreStub) DeleteExpiredSessions(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (s *sessionStoreStub) revoke() { s.mu.Lock(); s.revoked = true; s.mu.Unlock() }

type socketRepositoryStub struct {
	mu                     sync.Mutex
	authorized             bool
	snapshot               application.Snapshot
	subscribed             func() bool
	loadBeforeSubscription bool
	loadCalls              int
	failLoadAt             int
	opens                  int
	renews                 int
	closes                 int
	active                 int
	businessEffects        int
	closeErr               error
	closeStarted           chan struct{}
	closeRelease           chan struct{}
	closeFinished          chan struct{}
}

func (r *socketRepositoryStub) SnapshotForAccount(context.Context, string, string) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorized {
		return application.Snapshot{}, application.ErrUnauthorized
	}
	return r.snapshot, nil
}
func (r *socketRepositoryStub) AuthorizeRoom(context.Context, string, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorized {
		return "", application.ErrUnauthorized
	}
	return "host", nil
}
func (r *socketRepositoryStub) LoadRoom(context.Context, string) (application.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadCalls++
	if r.failLoadAt > 0 && r.loadCalls >= r.failLoadAt {
		return application.Snapshot{}, errors.New("load failed")
	}
	if r.subscribed != nil && !r.subscribed() {
		r.loadBeforeSubscription = true
	}
	return r.snapshot, nil
}
func (r *socketRepositoryStub) OpenConnection(context.Context, application.ConnectionLease) (application.PresenceTransition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorized {
		return application.PresenceTransition{}, application.ErrUnauthorized
	}
	r.opens++
	r.active++
	return application.PresenceTransition{RoomCode: testRoom}, nil
}
func (r *socketRepositoryStub) RenewConnection(context.Context, string, time.Time, time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renews++
	if !r.authorized {
		return application.ErrUnauthorized
	}
	return nil
}
func (r *socketRepositoryStub) CloseConnection(ctx context.Context, _ string, _ time.Time) (application.PresenceTransition, error) {
	r.mu.Lock()
	r.closes++
	closeErr := r.closeErr
	closeStarted := r.closeStarted
	closeRelease := r.closeRelease
	closeFinished := r.closeFinished
	r.mu.Unlock()
	if closeStarted != nil {
		select {
		case closeStarted <- struct{}{}:
		default:
		}
	}
	if closeRelease != nil {
		select {
		case <-closeRelease:
		case <-ctx.Done():
			closeErr = errors.Join(closeErr, ctx.Err())
		}
	}
	if closeFinished != nil {
		closeFinished <- struct{}{}
	}
	if closeErr != nil {
		return application.PresenceTransition{}, closeErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active > 0 {
		r.active--
	}
	return application.PresenceTransition{RoomCode: testRoom, LastSocket: r.active == 0}, nil
}
func (r *socketRepositoryStub) setSnapshot(revision uint64) {
	r.mu.Lock()
	r.snapshot = testSnapshot(revision)
	r.mu.Unlock()
}
func (r *socketRepositoryStub) connectionCounts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes, r.active
}

func testSnapshot(revision uint64) application.Snapshot {
	return application.Snapshot{Revision: revision, State: domain.State{Code: testRoom, Round: 1, Players: []domain.Player{{ID: "secret-player", Submitted: true}}}, Presence: map[string]bool{"host": true}}
}

type socketFrameForTest struct {
	Type     string `json:"type"`
	Revision uint64 `json:"revision"`
	Room     struct {
		Code    string `json:"room_code"`
		Players []struct {
			Role      string `json:"role"`
			Submitted bool   `json:"submitted"`
		} `json:"players"`
	} `json:"room"`
	Player struct {
		AccountID string `json:"account_id"`
		Role      string `json:"role"`
	} `json:"player"`
}

func readFrame(t *testing.T, connection *websocket.Conn, timeout time.Duration) socketFrameForTest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var frame socketFrameForTest
	if err := wsjson.Read(ctx, connection, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func readFrameAtLeast(t *testing.T, connection *websocket.Conn, revision uint64, timeout time.Duration) socketFrameForTest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame := readFrame(t, connection, time.Until(deadline))
		if frame.Revision >= revision {
			return frame
		}
	}
	t.Fatalf("socket did not reach revision %d", revision)
	return socketFrameForTest{}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition did not become true")
	}
}

func status(response *http.Response) any {
	if response == nil {
		return nil
	}
	return response.StatusCode
}
