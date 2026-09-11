package roomhttp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/room/adapter/memory"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

func TestHTTPContractAndPrivacy(t *testing.T) {
	service := fixedService()
	handler := NewRouter(service, func(context.Context) error { return nil })
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
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", guest.PlayerToken, `{"move":"scissors"}`), http.StatusNoContent)
	resolved := request(t, handler, http.MethodGet, "/api/rooms/ABC234/state", "", "")
	if !strings.Contains(resolved.Body.String(), "player_one_wins") || !strings.Contains(resolved.Body.String(), "scissors") {
		t.Fatalf("resolved state = %s", resolved.Body.String())
	}
	assertPrivate(t, resolved.Body.String(), host.PlayerToken, guest.PlayerToken)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", host.PlayerToken, `{"move":"paper"}`), http.StatusConflict)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/validate-session", host.PlayerToken, `{"role":"host"}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/next-round", guest.PlayerToken, `{"round":1}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/next-round", host.PlayerToken, `{"round":1}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", host.PlayerToken, `{"move":"paper"}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", guest.PlayerToken, `{"move":"rock"}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/leave", guest.PlayerToken, ""), http.StatusNoContent)
}

func TestAuthenticationAndStableErrors(t *testing.T) {
	service := fixedService()
	handler := NewRouter(service, nil)
	created := request(t, handler, http.MethodPost, "/api/rooms", "", "")
	host := credentials(t, created)
	_ = host
	tests := []struct {
		name, path, token, body string
		status                  int
		message                 string
	}{
		{"unknown room", "/api/rooms/NONE23/moves", "bad", `{"move":"rock"}`, 404, "room not found"},
		{"missing token", "/api/rooms/ABC234/moves", "", `{"move":"rock"}`, 401, "unauthorized"},
		{"bad token", "/api/rooms/ABC234/moves", "bad", `{"move":"rock"}`, 401, "unauthorized"},
		{"invalid move", "/api/rooms/ABC234/moves", host.PlayerToken, `{"move":"lizard"}`, 400, "invalid move"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := request(t, handler, http.MethodPost, tt.path, tt.token, tt.body)
			assertStatus(t, response, tt.status)
			if got := response.Body.String(); got != "{\"error\":\""+tt.message+"\"}\n" {
				t.Fatalf("body = %q", got)
			}
		})
	}
}

func TestValidateSessionChecksClosedRoomBeforeCredentials(t *testing.T) {
	service := fixedService()
	handler := NewRouter(service, nil)
	hostResponse := request(t, handler, http.MethodPost, "/api/rooms", "", "")
	assertStatus(t, hostResponse, http.StatusCreated)
	host := credentials(t, hostResponse)
	guestResponse := request(t, handler, http.MethodPost, "/api/rooms/ABC234/join", "", "")
	guest := credentials(t, guestResponse)

	for _, authorization := range []string{"", "Basic invalid", "Bearer invalid"} {
		response := validateSessionRequestWithAuthorization(t, handler, authorization)
		assertStatus(t, response, http.StatusUnauthorized)
	}

	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", host.PlayerToken, `{"move":"rock"}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", guest.PlayerToken, `{"move":"rock"}`), http.StatusNoContent)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/leave", guest.PlayerToken, ""), http.StatusNoContent)
	for _, authorization := range []string{"", "Basic invalid", "Bearer invalid"} {
		response := validateSessionRequestWithAuthorization(t, handler, authorization)
		assertStatus(t, response, http.StatusNotFound)
		if response.Body.String() != "{\"error\":\"room not found\"}\n" {
			t.Fatalf("closed-room body = %q", response.Body.String())
		}
	}
}

func TestLeaveRejectedDuringUnfinishedGameAndPresenceIsAuthenticated(t *testing.T) {
	service := fixedService()
	handler := NewRouter(service, nil)
	host := credentials(t, request(t, handler, http.MethodPost, "/api/rooms", "", ""))
	guest := credentials(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/join", "", ""))

	response := request(t, handler, http.MethodPost, "/api/rooms/ABC234/leave", guest.PlayerToken, "")
	assertStatus(t, response, http.StatusConflict)
	if response.Body.String() != "{\"error\":\"game is unfinished\"}\n" {
		t.Fatalf("body = %q", response.Body.String())
	}
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/presence", "bad", ""), http.StatusUnauthorized)
	assertStatus(t, request(t, handler, http.MethodPost, "/api/rooms/ABC234/presence", host.PlayerToken, ""), http.StatusNoContent)
}

func TestHealthAndReadinessDiffer(t *testing.T) {
	handler := NewRouter(fixedService(), func(context.Context) error { return errors.New("database unavailable") })
	assertStatus(t, request(t, handler, http.MethodGet, "/api/health", "", ""), 200)
	response := request(t, handler, http.MethodGet, "/api/ready", "", "")
	assertStatus(t, response, 503)
}

func TestSSEEmitsAuthoritativeRevisions(t *testing.T) {
	service := fixedService()
	host, _ := service.Create(context.Background())
	server := httptest.NewServer(NewRouter(service, nil))
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/api/rooms/ABC234/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if id := readEvent(t, reader); id != "1" {
		t.Fatalf("initial id = %s", id)
	}
	_, _ = service.Join(context.Background(), host.RoomCode)
	if id := readEvent(t, reader); id != "2" {
		t.Fatalf("changed id = %s", id)
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
	return application.NewServiceWithGenerators(repository, events, func() (string, error) { return "ABC234", nil }, next, next)
}
func request(t *testing.T, handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func validateSessionRequestWithAuthorization(t *testing.T, handler http.Handler, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/rooms/ABC234/validate-session", strings.NewReader(`{"role":"host"}`))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
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
		if strings.Contains(body, secret) {
			t.Fatalf("state leaked token: %s", body)
		}
	}
	if strings.Contains(body, "player_id") || strings.Contains(body, "credential") {
		t.Fatalf("state leaked identity: %s", body)
	}
}
func readEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	result := make(chan string, 1)
	go func() {
		id := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				result <- ""
				return
			}
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "id: ") {
				id = strings.TrimPrefix(line, "id: ")
			}
			if line == "" {
				result <- id
				return
			}
		}
	}()
	select {
	case id := <-result:
		return id
	case <-time.After(time.Second):
		t.Fatal("SSE timeout")
		return ""
	}
}
