package roomhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/auth"
	"example.com/rock-paper-money/internal/room/adapter/memory"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

func TestHTTPContractAndPrivacy(t *testing.T) {
	service := fixedService()
	handler := NewRouter(service, testVerifier{}, func(context.Context) error { return nil }, nil)
	created := request(t, handler, http.MethodPost, "/api/rooms", "", "")
	assertStatus(t, created, http.StatusCreated)
	host := credentials(t, created)
	joined := request(t, handler, http.MethodPost, "/api/rooms/%20abc234%20/join", "", "")
	assertStatus(t, joined, http.StatusCreated)
	guest := credentials(t, joined)
	state := request(t, handler, http.MethodGet, "/api/rooms/ABC234/state", "", "")
	assertStatus(t, state, http.StatusOK)
	assertPrivate(t, state.Body.String(), host.PlayerToken, guest.PlayerToken)
	move := request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", host.PlayerToken, `{"move":"rock"}`)
	assertStatus(t, move, http.StatusNoContent)
	waiting := request(t, handler, http.MethodGet, "/api/rooms/ABC234/state", "", "")
	if strings.Contains(waiting.Body.String(), "rock") || strings.Contains(waiting.Body.String(), `"moves"`) {
		t.Fatalf("unresolved move leaked: %s", waiting.Body.String())
	}
	assertStatus(t, requestAs(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "guest-user", guest.PlayerToken, `{"move":"scissors"}`), http.StatusNoContent)
	resolved := request(t, handler, http.MethodGet, "/api/rooms/ABC234/state", "", "")
	if !strings.Contains(resolved.Body.String(), "player_one_wins") || !strings.Contains(resolved.Body.String(), "scissors") {
		t.Fatalf("resolved state = %s", resolved.Body.String())
	}
	assertPrivate(t, resolved.Body.String(), host.PlayerToken, guest.PlayerToken)
	analytics := request(t, handler, http.MethodGet, "/api/analytics/rounds", "", "")
	assertStatus(t, analytics, http.StatusOK)
	if !strings.Contains(analytics.Body.String(), `"total_house_earnings":"25"`) || !strings.Contains(analytics.Body.String(), `"winner_role":"host"`) || strings.Contains(analytics.Body.String(), "host-user") {
		t.Fatalf("analytics response = %s", analytics.Body.String())
	}
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", host.PlayerToken, `{"move":"paper"}`), http.StatusConflict)
	assertStatus(t, requestAs(t, handler, http.MethodPost, "/api/rooms/ABC234/next-round", "guest-user", guest.PlayerToken, `{"round":1}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/next-round", host.PlayerToken, `{"round":1}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", host.PlayerToken, `{"move":"paper"}`), http.StatusNoContent)
	assertStatus(t, requestAs(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "guest-user", guest.PlayerToken, `{"move":"rock"}`), http.StatusNoContent)
	assertStatus(t, requestAs(t, handler, http.MethodPost, "/api/rooms/ABC234/leave", "guest-user", guest.PlayerToken, ""), http.StatusNoContent)
}

func TestAuthenticationAndStableErrors(t *testing.T) {
	service := fixedService()
	handler := NewRouter(service, testVerifier{}, nil, nil)
	created := request(t, handler, http.MethodPost, "/api/rooms", "", "")
	host := credentials(t, created)
	_ = host
	tests := []struct {
		name, path, token, body string
		status                  int
		code, message           string
		hasRaw                  bool
	}{
		{"unknown room", "/api/rooms/NONE23/moves", "bad", `{"move":"rock"}`, 404, "room_not_found", "Room not found.", true},
		{"invalid move", "/api/rooms/ABC234/moves", host.PlayerToken, `{"move":"lizard"}`, 400, "invalid_move", "Invalid move.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := request(t, handler, http.MethodPost, tt.path, tt.token, tt.body)
			assertStatus(t, response, tt.status)
			assertAPIError(t, response, tt.code, tt.message, tt.hasRaw)
		})
	}
}

func TestRoomMutationsRequireIdempotencyAndAccountSeatAuthority(t *testing.T) {
	handler := NewRouter(fixedService(), testVerifier{}, nil, nil)

	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{"create", "/api/rooms", ""},
		{"join", "/api/rooms/ABC234/join", ""},
		{"move", "/api/rooms/ABC234/moves", `{"move":"rock"}`},
		{"next round", "/api/rooms/ABC234/next-round", `{"round":1}`},
		{"leave", "/api/rooms/ABC234/leave", ""},
		{"recharge", "/api/wallet/recharges", `{"amount":1}`},
	} {
		t.Run("missing key for "+test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer host-user")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertStatus(t, response, http.StatusBadRequest)
			assertAPIError(t, response, "idempotency_key_required", "A valid Idempotency-Key header is required.", false)
		})
	}

	created := requestAs(t, handler, http.MethodPost, "/api/rooms", "host-user", "", "")
	assertStatus(t, created, http.StatusCreated)
	legacyClaim := requestAs(t, handler, http.MethodGet, "/api/rooms/ABC234/state", "other-user", "host-secret-token", "")
	assertStatus(t, legacyClaim, http.StatusNotFound)
	if strings.Contains(legacyClaim.Body.String(), "ABC234") || strings.Contains(legacyClaim.Body.String(), "host") {
		t.Fatalf("non-player response disclosed room authority: %s", legacyClaim.Body.String())
	}

	ownerState := requestAs(t, handler, http.MethodGet, "/api/rooms/ABC234/state", "host-user", "fabricated-token", "")
	assertStatus(t, ownerState, http.StatusOK)
}

func TestCanonicalMutationReplayAndConflict(t *testing.T) {
	handler := NewRouter(fixedService(), testVerifier{}, nil, nil)
	created := commandRequest(t, handler, http.MethodPost, "/api/rooms", "host-user", "canonical-create", "")
	assertStatus(t, created, http.StatusCreated)
	joined := commandRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/join", "guest-user", "canonical-join", "")
	assertStatus(t, joined, http.StatusCreated)

	first := commandRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "host-user", "canonical-move", `{"move":"rock"}`)
	assertStatus(t, first, http.StatusNoContent)
	replay := commandRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "host-user", "canonical-move", " { \"move\" : \"rock\" } ")
	assertStatus(t, replay, http.StatusNoContent)
	conflict := commandRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "host-user", "canonical-move", `{"move":"paper"}`)
	assertStatus(t, conflict, http.StatusConflict)
	crossAccount := commandRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "guest-user", "canonical-move", `{"move":"scissors"}`)
	assertStatus(t, crossAccount, http.StatusConflict)
	if crossAccount.Body.String() != conflict.Body.String() {
		var conflictBody, crossAccountBody apiErrorResponse
		_ = json.Unmarshal(conflict.Body.Bytes(), &conflictBody)
		_ = json.Unmarshal(crossAccount.Body.Bytes(), &crossAccountBody)
		conflictBody.Meta = apiErrorMeta{}
		crossAccountBody.Meta = apiErrorMeta{}
		if conflictBody != crossAccountBody {
			t.Fatalf("cross-account conflict disclosed a distinct result: %s vs %s", crossAccount.Body.String(), conflict.Body.String())
		}
	}
}

func TestSubmitMoveUsesSingleAuthoritativeRepositoryMutation(t *testing.T) {
	repository := &submitOnlyRepository{}
	service := application.NewService(repository, nil)
	handler := NewRouter(service, testVerifier{}, nil, nil)

	response := requestAs(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "host-user", "room-token", `{"move":"rock"}`)

	assertStatus(t, response, http.StatusNoContent)
	if repository.calls != 1 {
		t.Fatalf("SubmitMoveAndSettle calls = %d, want 1", repository.calls)
	}
}

func TestWalletRechargeIsSelfScopedAndIdempotent(t *testing.T) {
	handler := NewRouter(fixedService(), testVerifier{}, nil, nil)
	initial := requestAs(t, handler, http.MethodGet, "/api/wallet", "host-user", "", "")
	assertStatus(t, initial, http.StatusOK)
	if !strings.Contains(initial.Body.String(), `"balance":"1000"`) {
		t.Fatalf("initial wallet=%s", initial.Body.String())
	}

	recharged := walletRechargeRequest(t, handler, "host-user", "recharge-1", `{"amount":25}`)
	assertStatus(t, recharged, http.StatusOK)
	if !strings.Contains(recharged.Body.String(), `"balance":"1025"`) {
		t.Fatalf("recharged wallet=%s", recharged.Body.String())
	}
	duplicate := walletRechargeRequest(t, handler, "host-user", "recharge-1", `{"amount":25}`)
	assertStatus(t, duplicate, http.StatusOK)
	if !strings.Contains(duplicate.Body.String(), `"balance":"1025"`) {
		t.Fatalf("duplicate recharge wallet=%s", duplicate.Body.String())
	}
	conflict := walletRechargeRequest(t, handler, "host-user", "recharge-1", `{"amount":26}`)
	assertStatus(t, conflict, http.StatusConflict)
	assertAPIError(t, conflict, "idempotency_conflict", "Idempotency key was already used with different data.", true)
	guestRecharge := walletRechargeRequest(t, handler, "guest-user", "recharge-1", `{"amount":5}`)
	assertStatus(t, guestRecharge, http.StatusConflict)
	guest := requestAs(t, handler, http.MethodGet, "/api/wallet", "guest-user", "", "")
	if !strings.Contains(guest.Body.String(), `"balance":"1000"`) {
		t.Fatalf("guest wallet was changed=%s", guest.Body.String())
	}
}

func TestCreateRoomRejectsInsufficientBalanceWithSafeError(t *testing.T) {
	repository, events := memory.New()
	service := application.NewServiceWithGenerators(
		repository,
		events,
		func() (string, error) { return "LOW234", nil },
		func() (string, error) { return "unused-token", nil },
		func() (string, error) { return "unused-player", nil },
	)
	handler := NewRouter(service, testVerifier{}, nil, nil)

	response := requestAs(t, handler, http.MethodPost, "/api/rooms", "host-user", "", "")
	assertStatus(t, response, http.StatusConflict)
	assertAPIError(t, response, "insufficient_balance", "Insufficient coin balance.", true)
	if strings.Contains(response.Body.String(), "host-user") {
		t.Fatalf("error exposed account identity: %s", response.Body.String())
	}
}

func TestKnownRouteWrongMethodReturnsUniformMethodNotAllowed(t *testing.T) {
	handler := NewRouter(fixedService(), testVerifier{}, nil, nil)
	tests := []struct {
		name, method, path, allow string
		status                    int
		code, message             string
	}{
		{name: "wrong method on health", method: http.MethodPost, path: "/api/health", allow: "GET, HEAD", status: http.StatusMethodNotAllowed, code: "method_not_allowed", message: "Method not allowed."},
		{name: "wrong method on create", method: http.MethodGet, path: "/api/rooms", allow: "POST", status: http.StatusMethodNotAllowed, code: "method_not_allowed", message: "Method not allowed."},
		{name: "unknown path", method: http.MethodGet, path: "/api/unknown", status: http.StatusNotFound, code: "endpoint_not_found", message: "Endpoint not found."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := request(t, handler, tt.method, tt.path, "", "")
			assertStatus(t, response, tt.status)
			assertAPIError(t, response, tt.code, tt.message, false)
			if response.Header().Get("Allow") != tt.allow {
				t.Fatalf("Allow = %q, want %q", response.Header().Get("Allow"), tt.allow)
			}
		})
	}
}

func TestRouteInternalErrorLogsCauseOnceWithoutDisclosingIt(t *testing.T) {
	repository, events := memory.New()
	cause := errors.New("entropy source unavailable")
	service := application.NewServiceWithGenerators(
		repository,
		events,
		func() (string, error) { return "", cause },
		func() (string, error) { return "unused-token", nil },
		func() (string, error) { return "unused-id", nil },
	)
	if _, err := service.Recharge(context.Background(), "host-user", application.RoundStake, "internal-error-test-funding"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := NewRouter(service, testVerifier{}, nil, logger)

	response := request(t, handler, http.MethodPost, "/api/rooms", "", "")
	assertStatus(t, response, http.StatusInternalServerError)
	if strings.Contains(response.Body.String(), cause.Error()) {
		t.Fatalf("internal cause leaked in response: %s", response.Body.String())
	}
	assertAPIError(t, response, "internal_error", "An internal error occurred.", false)

	entries := errorLogEntries(t, logs.String())
	if len(entries) != 1 || len(logEntries(t, logs.String())) != 1 {
		t.Fatalf("log counts = total %d, errors %d, want 1 each; logs=%s", len(logEntries(t, logs.String())), len(entries), logs.String())
	}
	entry := entries[0]
	if entry["error"] != cause.Error() || entry["operation"] != "create_room" || entry["request_id"] != response.Header().Get("X-Request-ID") {
		t.Fatalf("internal route error log = %#v", entry)
	}
}

func TestProtectedRoutesRequireAccountAndMatchingRoomOwner(t *testing.T) {
	handler := NewRouter(fixedService(), testVerifier{}, nil, nil)

	withoutAccount := httptest.NewRequest(http.MethodPost, "/api/rooms", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, withoutAccount)
	assertStatus(t, response, http.StatusUnauthorized)

}

func TestLeaveRejectedDuringUnfinishedGame(t *testing.T) {
	service := fixedService()
	handler := NewRouter(service, testVerifier{}, nil, nil)
	host := credentials(t, request(t, handler, http.MethodPost, "/api/rooms", "", ""))
	guest := credentials(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/join", "", ""))

	response := request(t, handler, http.MethodPost, "/api/rooms/ABC234/leave", guest.PlayerToken, "")
	assertStatus(t, response, http.StatusConflict)
	assertAPIError(t, response, "game_unfinished", "Game is unfinished.", true)
	_ = host
}

func TestSoleWaitingPlayerMayLeaveRoom(t *testing.T) {
	handler := NewRouter(fixedService(), testVerifier{}, nil, nil)
	host := credentials(t, request(t, handler, http.MethodPost, "/api/rooms", "", ""))
	response := request(t, handler, http.MethodPost, "/api/rooms/ABC234/leave", host.PlayerToken, "")
	assertStatus(t, response, http.StatusNoContent)
	state := request(t, handler, http.MethodGet, "/api/rooms/ABC234/state", "", "")
	assertStatus(t, state, http.StatusOK)
	if !strings.Contains(state.Body.String(), `"closed":true`) {
		t.Fatalf("state = %s", state.Body.String())
	}
}

func TestHealthAndReadinessDiffer(t *testing.T) {
	handler := NewRouter(fixedService(), testVerifier{}, func(context.Context) error { return errors.New("database unavailable") }, nil)
	assertStatus(t, request(t, handler, http.MethodGet, "/api/health", "", ""), 200)
	response := request(t, handler, http.MethodGet, "/api/ready", "", "")
	assertStatus(t, response, 503)
}

func TestOperationalReadinessDistinguishesAuthorityFromRealtimeDegradation(t *testing.T) {
	status := func(context.Context) (ReadinessStatus, int) {
		return ReadinessStatus{Status: "degraded", PostgreSQL: "authoritative", Realtime: "redis_unavailable"}, http.StatusServiceUnavailable
	}
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("rpm_active_sockets 0\n")) })
	handler := NewOperationalRouter(fixedService(), testVerifier{}, nil, nil, status, nil, nil, metrics)
	ready := request(t, handler, http.MethodGet, "/api/ready", "", "")
	assertStatus(t, ready, http.StatusServiceUnavailable)
	if body := ready.Body.String(); !strings.Contains(body, `"postgresql":"authoritative"`) || !strings.Contains(body, `"realtime":"redis_unavailable"`) {
		t.Fatalf("readiness body = %s", body)
	}
	metricResponse := request(t, handler, http.MethodGet, "/metrics", "", "")
	assertStatus(t, metricResponse, http.StatusOK)
}

func TestWebSocketRouteIsWiredWithoutBearerMiddleware(t *testing.T) {
	called := false
	socket := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.PathValue("code") != "ABC234" {
			t.Fatalf("room code = %q", r.PathValue("code"))
		}
		w.WriteHeader(http.StatusSwitchingProtocols)
	})
	handler := NewRouterWithWebSocket(fixedService(), testVerifier{}, nil, nil, socket)
	request := httptest.NewRequest(http.MethodGet, "/api/rooms/ABC234/socket", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !called || response.Code != http.StatusSwitchingProtocols {
		t.Fatalf("socket called=%v status=%d", called, response.Code)
	}
}

func TestCookieSessionRESTRejectsBearerAndChecksCSRFBeforeEffects(t *testing.T) {
	sessionStore := newRouterSessionStore()
	sessions := auth.NewSessionService(sessionStore, bytes.NewReader(append(bytes.Repeat([]byte{0x41}, 32), bytes.Repeat([]byte{0x42}, 32)...)), func() time.Time {
		return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	})
	credentials, err := sessions.Create(context.Background(), "host-user")
	if err != nil {
		t.Fatal(err)
	}
	security, err := auth.NewRequestSecurity("https://game.example", sessions)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewSessionRouter(fixedService(), testVerifier{}, sessions, security, nil, nil)

	bearerOnly := httptest.NewRequest(http.MethodPost, "/api/rooms", nil)
	bearerOnly.Header.Set("Authorization", "Bearer host-user")
	bearerOnly.Header.Set("Origin", "https://game.example")
	bearerOnly.Header.Set(auth.CSRFHeaderName, credentials.CSRFToken)
	bearerOnly.Header.Set("Idempotency-Key", "bearer-must-not-authenticate")
	bearerResponse := httptest.NewRecorder()
	handler.ServeHTTP(bearerResponse, bearerOnly)
	assertStatus(t, bearerResponse, http.StatusUnauthorized)

	for _, test := range []struct {
		name   string
		origin string
		csrf   string
	}{
		{name: "missing origin", csrf: credentials.CSRFToken},
		{name: "foreign origin", origin: "https://attacker.example", csrf: credentials.CSRFToken},
		{name: "missing csrf", origin: "https://game.example"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/rooms", nil)
			request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token})
			request.Header.Set("Origin", test.origin)
			request.Header.Set(auth.CSRFHeaderName, test.csrf)
			request.Header.Set("Idempotency-Key", "csrf-"+strings.ReplaceAll(test.name, " ", "-"))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertStatus(t, response, http.StatusForbidden)
		})
	}

	valid := httptest.NewRequest(http.MethodPost, "/api/rooms", nil)
	valid.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token})
	valid.Header.Set("Origin", "https://game.example")
	valid.Header.Set(auth.CSRFHeaderName, credentials.CSRFToken)
	valid.Header.Set("Idempotency-Key", "cookie-session-create")
	validResponse := httptest.NewRecorder()
	handler.ServeHTTP(validResponse, valid)
	assertStatus(t, validResponse, http.StatusCreated)
	if strings.Contains(validResponse.Body.String(), "player_token") {
		t.Fatalf("cookie response exposed legacy room token: %s", validResponse.Body.String())
	}
}

func TestLegacyRealtimeRoutesAreRemoved(t *testing.T) {
	handler := NewRouter(fixedService(), testVerifier{}, nil, nil)
	for _, path := range []string{"/api/rooms/ABC234/events", "/api/rooms/ABC234/presence", "/api/rooms/ABC234/validate-session"} {
		response := request(t, handler, http.MethodGet, path, "", "")
		assertStatus(t, response, http.StatusNotFound)
	}
}

func TestPublicStateProjectsSingleForfeitMoveByPlayerIdentity(t *testing.T) {
	state := domain.State{
		Code:     "ABC234",
		Resolved: true,
		Forfeit:  true,
		Players:  []domain.Player{{ID: "host-id"}, {ID: "guest-id"}},
		Moves:    []domain.PlayerMove{{PlayerID: "guest-id", Move: domain.Paper}},
	}

	response := publicRoomState(state.Code, state)
	if len(response.Moves) != 1 || response.Moves[0].Role != "guest" || response.Moves[0].Move != domain.Paper {
		t.Fatalf("projected moves = %#v", response.Moves)
	}
}

func fixedService() *application.Service {
	repository, events := memory.New()
	tokens := []string{"host-secret-token", "host-id", "guest-secret-token", "guest-id"}
	var mu sync.Mutex
	next := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		v := tokens[0]
		tokens = tokens[1:]
		return v, nil
	}
	service := application.NewServiceWithGenerators(repository, events, func() (string, error) { return "ABC234", nil }, next, next)
	for index, owner := range []string{"host-user", "guest-user"} {
		if _, err := service.Recharge(context.Background(), owner, 1_000, fmt.Sprintf("http-test-funding-%d", index)); err != nil {
			panic(err)
		}
	}
	return service
}
func request(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	account := "host-user"
	if strings.Contains(path, "/join") || token == "guest-secret-token" {
		account = "guest-user"
	}
	return requestAs(t, handler, method, path, account, token, body)
}

func requestAs(t *testing.T, handler http.Handler, method, path, account, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if account != "" {
		req.Header.Set("Authorization", "Bearer "+account)
	}
	if token != "" {
		req.Header.Set("X-Room-Token", token)
	}
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("test-%d", time.Now().UnixNano()))
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func commandRequest(t *testing.T, handler http.Handler, method, path, account, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+account)
	req.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func walletRechargeRequest(t *testing.T, handler http.Handler, account, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/wallet/recharges", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+account)
	req.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

type testVerifier struct{}

type routerSessionStore struct {
	mu      sync.Mutex
	records map[auth.Digest]auth.Session
}

func newRouterSessionStore() *routerSessionStore {
	return &routerSessionStore{records: make(map[auth.Digest]auth.Session)}
}

func (s *routerSessionStore) CreateSession(_ context.Context, session auth.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[session.Digest] = session
	return nil
}

func (s *routerSessionStore) UseSession(_ context.Context, digest auth.Digest, now, idleExpiresAt time.Time) (auth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.records[digest]
	if !ok || session.RevokedAt != nil || !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
		return auth.Session{}, auth.ErrInvalidSession
	}
	session.IdleExpiresAt = idleExpiresAt
	s.records[digest] = session
	return session, nil
}

func (s *routerSessionStore) RevokeSession(_ context.Context, digest auth.Digest, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.records[digest]
	if !ok {
		return auth.ErrInvalidSession
	}
	session.RevokedAt = &at
	s.records[digest] = session
	return nil
}

func (s *routerSessionStore) DeleteExpiredSessions(context.Context, time.Time) (int64, error) {
	return 0, nil
}

type submitOnlyRepository struct {
	application.Repository
	application.CommandRepository
	calls int
}

func (r *submitOnlyRepository) SubmitMoveCommand(context.Context, string, string, domain.Move, application.Command) (application.CommandResult, error) {
	r.calls++
	return application.CommandResult{}, nil
}

func (r *submitOnlyRepository) SubmitMoveAndSettle(context.Context, string, application.CredentialDigest, string, domain.Move) (application.Snapshot, error) {
	r.calls++
	return application.Snapshot{}, nil
}

func (testVerifier) Verify(_ context.Context, token string) (auth.Principal, error) {
	if token != "host-user" && token != "guest-user" && token != "other-user" {
		return auth.Principal{}, errors.New("invalid token")
	}
	return auth.Principal{Subject: token}, nil
}
func assertStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, want, response.Body.String())
	}
}
func credentials(t *testing.T, response *httptest.ResponseRecorder) roomCredentials {
	t.Helper()
	var value roomCredentials
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
func assertPrivate(t *testing.T, body string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(body, secret) {
			t.Fatalf("state leaked token: %s", body)
		}
	}
	if strings.Contains(body, "player_id") || strings.Contains(body, "credential") {
		t.Fatalf("state leaked identity: %s", body)
	}
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, code, message string, hasRaw bool) {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != float64(response.Code) || body["code"] != code || body["message"] != message {
		t.Fatalf("error body = %#v", body)
	}
	_, rawPresent := body["rawError"]
	if rawPresent != hasRaw {
		t.Fatalf("rawError presence = %v, want %v; body=%#v", rawPresent, hasRaw, body)
	}
	meta, ok := body["meta"].(map[string]any)
	if !ok || meta["requestId"] == "" || meta["requestId"] != response.Header().Get("X-Request-ID") {
		t.Fatalf("error metadata = %#v; header=%q", meta, response.Header().Get("X-Request-ID"))
	}
	if _, err := time.Parse(time.RFC3339Nano, meta["time"].(string)); err != nil {
		t.Fatalf("error time = %#v: %v", meta["time"], err)
	}
}
