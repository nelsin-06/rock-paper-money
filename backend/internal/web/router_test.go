package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/room"
)

func TestRoomEventsStreamsCurrentAndChangedPublicState(t *testing.T) {
	store := room.NewStore()
	if _, err := store.Create("ABC234", "host-secret-token"); err != nil {
		t.Fatalf("create room: %v", err)
	}

	server := httptest.NewUnstartedServer(NewRouter(store))
	server.Config.WriteTimeout = 25 * time.Millisecond
	server.Start()
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/rooms/ABC234/events", nil)
	if err != nil {
		t.Fatalf("create events request: %v", err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("connect to room events: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want %q", contentType, "text/event-stream")
	}

	reader := bufio.NewReader(response.Body)
	initialID, initialBody := readRoomEvent(t, reader)
	if initialID != "1" {
		t.Errorf("initial event ID = %q, want 1", initialID)
	}
	assertPublicStateDoesNotExposeTokens(t, initialBody, "host-secret-token")
	initial := decodeStateJSON(t, initialBody)
	if initial.Ready || len(initial.Players) != 1 {
		t.Errorf("initial state = %#v, want waiting room", initial)
	}

	// Prove the endpoint clears its own deadline rather than relying on a zero
	// server-wide WriteTimeout.
	time.Sleep(50 * time.Millisecond)
	if err := store.Join("ABC234", "guest-secret-token"); err != nil {
		t.Fatalf("join room: %v", err)
	}
	changedID, changedBody := readRoomEvent(t, reader)
	if changedID != "2" {
		t.Errorf("changed event ID = %q, want 2", changedID)
	}
	assertPublicStateDoesNotExposeTokens(t, changedBody, "host-secret-token", "guest-secret-token")
	changed := decodeStateJSON(t, changedBody)
	if !changed.Ready || len(changed.Players) != 2 {
		t.Errorf("changed state = %#v, want ready room", changed)
	}

	if err := store.SubmitMove("ABC234", "host-secret-token", "rock"); err != nil {
		t.Fatalf("submit host move: %v", err)
	}
	moveID, moveBody := readRoomEvent(t, reader)
	if moveID != "3" {
		t.Errorf("move event ID = %q, want 3", moveID)
	}
	assertPublicStateDoesNotExposeTokens(t, moveBody, "host-secret-token", "guest-secret-token")
	assertRoundOutcomeOmitted(t, moveBody)
	if strings.Contains(moveBody, "rock") {
		t.Errorf("unresolved SSE state exposed submitted move: %s", moveBody)
	}

	if err := store.SubmitMove("ABC234", "guest-secret-token", "scissors"); err != nil {
		t.Fatalf("submit guest move: %v", err)
	}
	resolvedID, resolvedBody := readRoomEvent(t, reader)
	if resolvedID != "4" {
		t.Errorf("resolved event ID = %q, want 4", resolvedID)
	}
	resolved := decodeStateJSON(t, resolvedBody)
	if !resolved.Resolved || len(resolved.Moves) != 2 {
		t.Errorf("resolved SSE state = %#v, want complete resolved round", resolved)
	}
}

func TestRoomEventsRejectsUnsupportedStreamingBeforeStartingStream(t *testing.T) {
	store := room.NewStore()
	if _, err := store.Create("ABC234", "host-token"); err != nil {
		t.Fatalf("create room: %v", err)
	}

	response := performRequest(t, NewRouter(store), http.MethodGet, "/api/rooms/ABC234/events")
	assertJSONError(t, response, http.StatusInternalServerError, "streaming is not supported")
}

func TestHealth(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	response := httptest.NewRecorder()

	NewRouter(room.NewStore()).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", contentType, "application/json")
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status value = %q, want %q", body.Status, "ok")
	}
}

func TestHealthRejectsWrongMethod(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/health", nil)
	response := httptest.NewRecorder()

	NewRouter(room.NewStore()).ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestCreateAndJoinRoom(t *testing.T) {
	handler := NewRouter(room.NewStore())

	created := performRequest(t, handler, http.MethodPost, "/api/rooms")
	assertJSONResponse(t, created, http.StatusCreated)
	creator := decodeCredentials(t, created)
	if creator.RoomCode == "" {
		t.Error("created room code is empty")
	}
	if len(creator.RoomCode) != roomCodeLength {
		t.Errorf("room code length = %d, want %d", len(creator.RoomCode), roomCodeLength)
	}
	for _, character := range creator.RoomCode {
		if !strings.ContainsRune(roomCodeAlphabet, character) {
			t.Errorf("room code contains non-shareable character %q", character)
		}
	}
	if creator.PlayerToken == "" {
		t.Error("host player token is empty")
	}

	joined := performRequest(t, handler, http.MethodPost, "/api/rooms/"+creator.RoomCode+"/join")
	assertJSONResponse(t, joined, http.StatusCreated)
	guest := decodeCredentials(t, joined)
	if guest.RoomCode != creator.RoomCode {
		t.Errorf("joined room code = %q, want %q", guest.RoomCode, creator.RoomCode)
	}
	if guest.PlayerToken == "" {
		t.Error("guest player token is empty")
	}
	if guest.PlayerToken == creator.PlayerToken {
		t.Error("guest reused the host player token")
	}
}

func TestJoinNormalizesRoomCode(t *testing.T) {
	store := room.NewStore()
	if _, err := store.Create("ABC234", "host-token"); err != nil {
		t.Fatalf("create room: %v", err)
	}
	handler := newRouter(store, fixedGenerator("unused"), fixedGenerator("guest-token"))

	response := performRequest(t, handler, http.MethodPost, "/api/rooms/%20abc234%20/join")
	assertJSONResponse(t, response, http.StatusCreated)
	joined := decodeCredentials(t, response)
	if joined.RoomCode != "ABC234" {
		t.Errorf("room code = %q, want %q", joined.RoomCode, "ABC234")
	}
}

func TestJoinUnknownRoom(t *testing.T) {
	handler := NewRouter(room.NewStore())
	response := performRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/join")

	assertJSONError(t, response, http.StatusNotFound, "room not found")
}

func TestJoinRejectsMissingRoomCode(t *testing.T) {
	handler := NewRouter(room.NewStore())
	response := performRequest(t, handler, http.MethodPost, "/api/rooms/%20/join")

	assertJSONError(t, response, http.StatusBadRequest, "room code is required")
}

func TestJoinFullRoom(t *testing.T) {
	store := room.NewStore()
	if _, err := store.Create("ABC234", "host-token"); err != nil {
		t.Fatalf("create room: %v", err)
	}
	if err := store.Join("ABC234", "first-guest-token"); err != nil {
		t.Fatalf("join room: %v", err)
	}
	handler := NewRouter(store)

	response := performRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/join")

	assertJSONError(t, response, http.StatusConflict, "room is full")
}

func TestRoomEndpointsRejectWrongMethods(t *testing.T) {
	handler := NewRouter(room.NewStore())
	for _, path := range []string{"/api/rooms", "/api/rooms/ABC234/join"} {
		t.Run(path, func(t *testing.T) {
			response := performRequest(t, handler, http.MethodGet, path)
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestCreateHandlesGeneratorFailure(t *testing.T) {
	generationError := func() (string, error) { return "", errors.New("entropy unavailable") }
	handler := newRouter(room.NewStore(), generationError, fixedGenerator("unused"))

	response := performRequest(t, handler, http.MethodPost, "/api/rooms")

	assertJSONError(t, response, http.StatusInternalServerError, "internal server error")
	if strings.Contains(response.Body.String(), "entropy") {
		t.Error("response exposed the internal generation error")
	}
}

func TestCreateHandlesPlayerTokenFailure(t *testing.T) {
	generationError := func() (string, error) { return "", errors.New("token-secret") }
	handler := newRouter(room.NewStore(), fixedGenerator("ABC234"), generationError)

	response := performRequest(t, handler, http.MethodPost, "/api/rooms")

	assertJSONError(t, response, http.StatusInternalServerError, "internal server error")
	if strings.Contains(response.Body.String(), "token-secret") {
		t.Error("response exposed the internal generation error")
	}
}

func TestJoinHandlesPlayerTokenFailure(t *testing.T) {
	store := room.NewStore()
	if _, err := store.Create("ABC234", "host-token"); err != nil {
		t.Fatalf("create room: %v", err)
	}
	generationError := func() (string, error) { return "", errors.New("token-secret") }
	handler := newRouter(store, fixedGenerator("unused"), generationError)

	response := performRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/join")

	assertJSONError(t, response, http.StatusInternalServerError, "internal server error")
	if strings.Contains(response.Body.String(), "token-secret") {
		t.Error("response exposed the internal generation error")
	}
}

func TestCreateStopsAfterBoundedRoomCodeCollisions(t *testing.T) {
	store := room.NewStore()
	if _, err := store.Create("ABC234", "existing-token"); err != nil {
		t.Fatalf("create room: %v", err)
	}
	calls := 0
	generateCode := func() (string, error) {
		calls++
		return "ABC234", nil
	}
	handler := newRouter(store, generateCode, fixedGenerator("new-token"))

	response := performRequest(t, handler, http.MethodPost, "/api/rooms")

	assertJSONError(t, response, http.StatusInternalServerError, "internal server error")
	if calls != maxGenerationAttempts {
		t.Errorf("code generation calls = %d, want %d", calls, maxGenerationAttempts)
	}
}

func TestJoinRetriesAccidentalPlayerTokenReuse(t *testing.T) {
	store := room.NewStore()
	if _, err := store.Create("ABC234", "same-token"); err != nil {
		t.Fatalf("create room: %v", err)
	}
	tokens := []string{"same-token", "new-token"}
	generateToken := func() (string, error) {
		token := tokens[0]
		tokens = tokens[1:]
		return token, nil
	}
	handler := newRouter(store, fixedGenerator("unused"), generateToken)

	response := performRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/join")

	assertJSONResponse(t, response, http.StatusCreated)
	if token := decodeCredentials(t, response).PlayerToken; token != "new-token" {
		t.Errorf("player token = %q, want %q", token, "new-token")
	}
}

func TestFullMatchAndNextRound(t *testing.T) {
	store := room.NewStore()
	tokens := []string{"host-secret-token", "guest-secret-token"}
	generateToken := func() (string, error) {
		token := tokens[0]
		tokens = tokens[1:]
		return token, nil
	}
	handler := newRouter(store, fixedGenerator("ABC234"), generateToken)

	created := performRequest(t, handler, http.MethodPost, "/api/rooms")
	host := decodeCredentials(t, created)
	joined := performRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/join")
	guest := decodeCredentials(t, joined)

	beforeMoves := performRequest(t, handler, http.MethodGet, "/api/rooms/%20abc234%20/state")
	assertJSONResponse(t, beforeMoves, http.StatusOK)
	assertPublicStateDoesNotExposeTokens(t, beforeMoves.Body.String(), host.PlayerToken, guest.PlayerToken)
	state := decodeState(t, beforeMoves)
	assertStatePlayers(t, state, true, false, 0, 0, false, false)
	if state.Result != "" || len(state.Moves) != 0 {
		t.Errorf("pre-move state exposed round outcome: %#v", state)
	}
	assertRoundOutcomeOmitted(t, beforeMoves.Body.String())

	firstMove := performAuthorizedJSONRequest(t, handler, http.MethodPost, "/api/rooms/%20abc234%20/moves", host.PlayerToken, `{"move":"rock"}`)
	assertNoContent(t, firstMove)

	waiting := performRequest(t, handler, http.MethodGet, "/api/rooms/ABC234/state")
	assertPublicStateDoesNotExposeTokens(t, waiting.Body.String(), host.PlayerToken, guest.PlayerToken)
	state = decodeState(t, waiting)
	assertStatePlayers(t, state, true, false, 0, 0, true, false)
	if state.Result != "" || len(state.Moves) != 0 || strings.Contains(waiting.Body.String(), "rock") {
		t.Errorf("state revealed a move before resolution: %s", waiting.Body.String())
	}
	assertRoundOutcomeOmitted(t, waiting.Body.String())

	secondMove := performAuthorizedJSONRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", guest.PlayerToken, `{"move":"scissors"}`)
	assertNoContent(t, secondMove)

	resolved := performRequest(t, handler, http.MethodGet, "/api/rooms/ABC234/state")
	assertPublicStateDoesNotExposeTokens(t, resolved.Body.String(), host.PlayerToken, guest.PlayerToken)
	state = decodeState(t, resolved)
	assertStatePlayers(t, state, true, true, 1, 0, true, true)
	if state.Result != "player_one_wins" {
		t.Errorf("result = %q, want %q", state.Result, "player_one_wins")
	}
	wantMoves := []stateMove{{Role: "host", Move: "rock"}, {Role: "guest", Move: "scissors"}}
	if len(state.Moves) != len(wantMoves) {
		t.Fatalf("moves = %#v, want %#v", state.Moves, wantMoves)
	}
	for i := range wantMoves {
		if state.Moves[i] != wantMoves[i] {
			t.Errorf("moves[%d] = %#v, want %#v", i, state.Moves[i], wantMoves[i])
		}
	}

	nextRound := performAuthorizedJSONRequest(t, handler, http.MethodPost, "/api/rooms/%20abc234%20/next-round", guest.PlayerToken, "")
	assertNoContent(t, nextRound)

	reset := performRequest(t, handler, http.MethodGet, "/api/rooms/ABC234/state")
	assertPublicStateDoesNotExposeTokens(t, reset.Body.String(), host.PlayerToken, guest.PlayerToken)
	state = decodeState(t, reset)
	assertStatePlayers(t, state, true, false, 1, 0, false, false)
	if state.Result != "" || len(state.Moves) != 0 {
		t.Errorf("next-round state retained round outcome: %#v", state)
	}
	assertRoundOutcomeOmitted(t, reset.Body.String())
}

func TestGameplayAuthentication(t *testing.T) {
	store := readyStoreForWeb(t)
	handler := NewRouter(store)

	tests := []struct {
		name       string
		path       string
		body       string
		authorizer func(*http.Request)
	}{
		{name: "move missing authorization", path: "/api/rooms/ABC234/moves", body: `{"move":"rock"}`},
		{name: "move wrong scheme", path: "/api/rooms/ABC234/moves", body: `{"move":"rock"}`, authorizer: setAuthorization("Basic host-token")},
		{name: "move missing bearer value", path: "/api/rooms/ABC234/moves", body: `{"move":"rock"}`, authorizer: setAuthorization("Bearer ")},
		{name: "move bearer containing whitespace", path: "/api/rooms/ABC234/moves", body: `{"move":"rock"}`, authorizer: setAuthorization("Bearer host-token extra")},
		{name: "move unknown bearer", path: "/api/rooms/ABC234/moves", body: `{"move":"rock"}`, authorizer: setAuthorization("Bearer unknown-secret")},
		{name: "move repeated authorization", path: "/api/rooms/ABC234/moves", body: `{"move":"rock"}`, authorizer: addAuthorizations("Bearer host-token", "Bearer guest-token")},
		{name: "next round missing authorization", path: "/api/rooms/ABC234/next-round"},
		{name: "next round malformed authorization", path: "/api/rooms/ABC234/next-round", authorizer: setAuthorization("Bearer")},
		{name: "next round unknown bearer", path: "/api/rooms/ABC234/next-round", authorizer: setAuthorization("Bearer unknown-secret")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			if test.authorizer != nil {
				test.authorizer(request)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			assertJSONError(t, response, http.StatusUnauthorized, "unauthorized")
			for _, secret := range []string{"host-token", "guest-token", "unknown-secret"} {
				if strings.Contains(response.Body.String(), secret) {
					t.Errorf("response exposed credential %q", secret)
				}
			}
		})
	}
}

func TestSubmitMoveRejectsInvalidJSONAndMoves(t *testing.T) {
	store := readyStoreForWeb(t)
	handler := NewRouter(store)

	tests := []struct {
		name    string
		body    string
		message string
	}{
		{name: "empty body", message: "invalid request body"},
		{name: "malformed JSON", body: `{"move":`, message: "invalid request body"},
		{name: "unknown field", body: `{"move":"rock","extra":true}`, message: "invalid request body"},
		{name: "trailing JSON", body: `{"move":"rock"}{"move":"paper"}`, message: "invalid request body"},
		{name: "missing move", body: `{}`, message: "invalid move"},
		{name: "empty move", body: `{"move":""}`, message: "invalid move"},
		{name: "invalid move", body: `{"move":"lizard"}`, message: "invalid move"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performAuthorizedJSONRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "host-token", test.body)
			assertJSONError(t, response, http.StatusBadRequest, test.message)
		})
	}
}

func TestGameplayConflicts(t *testing.T) {
	t.Run("room not ready", func(t *testing.T) {
		store := room.NewStore()
		if _, err := store.Create("ABC234", "host-token"); err != nil {
			t.Fatalf("create room: %v", err)
		}
		response := performAuthorizedJSONRequest(t, NewRouter(store), http.MethodPost, "/api/rooms/ABC234/moves", "host-token", `{"move":"rock"}`)
		assertJSONError(t, response, http.StatusConflict, "room not ready")
	})

	t.Run("duplicate move", func(t *testing.T) {
		handler := NewRouter(readyStoreForWeb(t))
		first := performAuthorizedJSONRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "host-token", `{"move":"rock"}`)
		assertNoContent(t, first)
		duplicate := performAuthorizedJSONRequest(t, handler, http.MethodPost, "/api/rooms/ABC234/moves", "host-token", `{"move":"paper"}`)
		assertJSONError(t, duplicate, http.StatusConflict, "move already submitted")
	})

	t.Run("unresolved next round", func(t *testing.T) {
		response := performAuthorizedJSONRequest(t, NewRouter(readyStoreForWeb(t)), http.MethodPost, "/api/rooms/ABC234/next-round", "guest-token", "")
		assertJSONError(t, response, http.StatusConflict, "round is not resolved")
	})
}

func TestGameplayEndpointsReturnNotFoundForUnknownRoom(t *testing.T) {
	handler := NewRouter(room.NewStore())
	tests := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPost, path: "/api/rooms/NONE23/moves", body: `{"move":"rock"}`},
		{method: http.MethodGet, path: "/api/rooms/NONE23/state"},
		{method: http.MethodPost, path: "/api/rooms/NONE23/next-round"},
	}

	for _, test := range tests {
		response := performAuthorizedJSONRequest(t, handler, test.method, test.path, "unknown-secret", test.body)
		assertJSONError(t, response, http.StatusNotFound, "room not found")
		if strings.Contains(response.Body.String(), "unknown-secret") {
			t.Error("not-found response exposed credential")
		}
	}
}

func TestGameplayEndpointsRejectWrongMethods(t *testing.T) {
	handler := NewRouter(room.NewStore())
	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/rooms/ABC234/moves"},
		{method: http.MethodPost, path: "/api/rooms/ABC234/state"},
		{method: http.MethodGet, path: "/api/rooms/ABC234/next-round"},
	}

	for _, test := range tests {
		response := performRequest(t, handler, test.method, test.path)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, response.Code, http.StatusMethodNotAllowed)
		}
	}
}

func performRequest(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func performAuthorizedJSONRequest(t *testing.T, handler http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertJSONResponse(t *testing.T, response *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, wantStatus, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", contentType, "application/json")
	}
}

func assertJSONError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantMessage string) {
	t.Helper()
	assertJSONResponse(t, response, wantStatus)
	bodyJSON := response.Body.String()
	wantBody := "{\"error\":\"" + wantMessage + "\"}\n"
	if bodyJSON != wantBody {
		t.Errorf("body = %q, want stable JSON %q", bodyJSON, wantBody)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(strings.NewReader(bodyJSON)).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error != wantMessage {
		t.Errorf("error = %q, want %q", body.Error, wantMessage)
	}
}

func assertNoContent(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	if response.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", response.Body.String())
	}
}

func decodeCredentials(t *testing.T, response *httptest.ResponseRecorder) roomCredentials {
	t.Helper()
	var credentials roomCredentials
	if err := json.NewDecoder(response.Body).Decode(&credentials); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return credentials
}

func decodeState(t *testing.T, response *httptest.ResponseRecorder) stateResponse {
	t.Helper()
	var state stateResponse
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatalf("decode state response: %v", err)
	}
	return state
}

func decodeStateJSON(t *testing.T, body string) stateResponse {
	t.Helper()
	var state stateResponse
	if err := json.Unmarshal([]byte(body), &state); err != nil {
		t.Fatalf("decode state JSON: %v", err)
	}
	return state
}

func readRoomEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()
	type result struct {
		id   string
		data string
		err  error
	}
	resultChannel := make(chan result, 1)
	go func() {
		var event result
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				event.err = err
				resultChannel <- event
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				resultChannel <- event
				return
			case strings.HasPrefix(line, "id: "):
				event.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				event.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	select {
	case event := <-resultChannel:
		if event.err != nil {
			t.Fatalf("read SSE event: %v", event.err)
		}
		return event.id, event.data
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE event")
		return "", ""
	}
}

func assertStatePlayers(t *testing.T, state stateResponse, ready, resolved bool, hostWins, guestWins int, hostSubmitted, guestSubmitted bool) {
	t.Helper()
	if state.RoomCode != "ABC234" || state.Ready != ready || state.Resolved != resolved {
		t.Errorf("state identity/status = %#v, want room ABC234 ready=%t resolved=%t", state, ready, resolved)
	}
	want := []statePlayer{
		{Role: "host", Wins: hostWins, Submitted: hostSubmitted},
		{Role: "guest", Wins: guestWins, Submitted: guestSubmitted},
	}
	if len(state.Players) != len(want) {
		t.Fatalf("players = %#v, want %#v", state.Players, want)
	}
	for i := range want {
		if state.Players[i] != want[i] {
			t.Errorf("players[%d] = %#v, want %#v", i, state.Players[i], want[i])
		}
	}
}

func assertPublicStateDoesNotExposeTokens(t *testing.T, body string, tokens ...string) {
	t.Helper()
	if strings.Contains(body, `"ID"`) || strings.Contains(body, `"id"`) || strings.Contains(body, "player_token") {
		t.Errorf("public state exposed an internal identity field: %s", body)
	}
	for _, token := range tokens {
		if strings.Contains(body, token) {
			t.Errorf("public state exposed player token %q", token)
		}
	}
}

func assertRoundOutcomeOmitted(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, `"result"`) || strings.Contains(body, `"moves"`) {
		t.Errorf("unresolved state included round outcome fields: %s", body)
	}
}

func readyStoreForWeb(t *testing.T) *room.Store {
	t.Helper()
	store := room.NewStore()
	if _, err := store.Create("ABC234", "host-token"); err != nil {
		t.Fatalf("create room: %v", err)
	}
	if err := store.Join("ABC234", "guest-token"); err != nil {
		t.Fatalf("join room: %v", err)
	}
	return store
}

func setAuthorization(value string) func(*http.Request) {
	return func(request *http.Request) {
		request.Header.Set("Authorization", value)
	}
}

func addAuthorizations(values ...string) func(*http.Request) {
	return func(request *http.Request) {
		for _, value := range values {
			request.Header.Add("Authorization", value)
		}
	}
}

func fixedGenerator(value string) generator {
	return func() (string, error) { return value, nil }
}
