package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"example.com/rock-paper-money/internal/game"
	"example.com/rock-paper-money/internal/room"
)

const (
	roomCodeLength        = 6
	playerTokenBytes      = 32
	maxGenerationAttempts = 8
	roomCodeAlphabet      = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	heartbeatInterval     = 15 * time.Second
)

type generator func() (string, error)

type router struct {
	store         *room.Store
	generateCode  generator
	generateToken generator
}

func NewRouter(store *room.Store) http.Handler {
	return newRouterWithLogger(store, randomRoomCode, randomPlayerToken, slog.Default())
}

func newRouter(store *room.Store, generateCode, generateToken generator) http.Handler {
	return newRouterWithLogger(store, generateCode, generateToken, slog.Default())
}

func newRouterWithLogger(store *room.Store, generateCode, generateToken generator, logger *slog.Logger) http.Handler {
	handler := &router{
		store:         store,
		generateCode:  generateCode,
		generateToken: generateToken,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", health)
	mux.HandleFunc("POST /api/rooms", handler.createRoom)
	mux.HandleFunc("POST /api/rooms/{code}/join", handler.joinRoom)
	mux.HandleFunc("POST /api/rooms/{code}/moves", handler.submitMove)
	mux.HandleFunc("GET /api/rooms/{code}/state", handler.roomState)
	mux.HandleFunc("GET /api/rooms/{code}/events", handler.roomEvents)
	mux.HandleFunc("POST /api/rooms/{code}/next-round", handler.startNextRound)
	return logRequests(logger, mux)
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func (rt *router) createRoom(w http.ResponseWriter, _ *http.Request) {
	for range maxGenerationAttempts {
		code, err := rt.generateCode()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		token, err := rt.generateToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		if _, err := rt.store.Create(code, token); err != nil {
			if errors.Is(err, room.ErrDuplicateRoom) {
				continue
			}
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		writeJSON(w, http.StatusCreated, roomCredentials{RoomCode: code, PlayerToken: token})
		return
	}

	writeError(w, http.StatusInternalServerError, "internal server error")
}

func (rt *router) joinRoom(w http.ResponseWriter, r *http.Request) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeError(w, http.StatusBadRequest, "room code is required")
		return
	}

	state, err := rt.store.State(code)
	if err != nil {
		if errors.Is(err, room.ErrRoomNotFound) {
			writeError(w, http.StatusNotFound, "room not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if state.Ready {
		writeError(w, http.StatusConflict, "room is full")
		return
	}

	for range maxGenerationAttempts {
		token, err := rt.generateToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}

		err = rt.store.Join(code, token)
		switch {
		case err == nil:
			writeJSON(w, http.StatusCreated, roomCredentials{RoomCode: code, PlayerToken: token})
			return
		case errors.Is(err, room.ErrDuplicatePlayer):
			continue
		case errors.Is(err, room.ErrRoomFull):
			writeError(w, http.StatusConflict, "room is full")
			return
		case errors.Is(err, room.ErrRoomNotFound):
			writeError(w, http.StatusNotFound, "room not found")
			return
		default:
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
	}

	writeError(w, http.StatusInternalServerError, "internal server error")
}

func (rt *router) submitMove(w http.ResponseWriter, r *http.Request) {
	code, state, ok := rt.findRoom(w, r.PathValue("code"))
	if !ok {
		return
	}

	playerID, ok := authenticatedPlayer(r, state)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var request moveRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !game.IsValidMove(request.Move) {
		writeError(w, http.StatusBadRequest, "invalid move")
		return
	}

	switch err := rt.store.SubmitMove(code, playerID, request.Move); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, room.ErrRoomNotReady):
		writeError(w, http.StatusConflict, "room not ready")
	case errors.Is(err, room.ErrDuplicateMove):
		writeError(w, http.StatusConflict, "move already submitted")
	case errors.Is(err, room.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room not found")
	case errors.Is(err, room.ErrUnknownPlayer):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (rt *router) roomState(w http.ResponseWriter, r *http.Request) {
	code, state, ok := rt.findRoom(w, r.PathValue("code"))
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, publicRoomState(code, state))
}

func (rt *router) roomEvents(w http.ResponseWriter, r *http.Request) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeError(w, http.StatusBadRequest, "room code is required")
		return
	}
	if !supportsStreaming(w) {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	initial, changes, unsubscribe, err := rt.store.Subscribe(code)
	if errors.Is(err, room.ErrRoomNotFound) {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if err := writeRoomEvent(w, controller, initial); err != nil {
		return
	}

	lastRevision := initial.Revision
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-changes:
			snapshot, err := rt.store.Snapshot(code)
			if err != nil || snapshot.Revision <= lastRevision {
				continue
			}
			if err := writeRoomEvent(w, controller, snapshot); err != nil {
				return
			}
			lastRevision = snapshot.Revision
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		}
	}
}

func publicRoomState(code string, state room.State) stateResponse {
	response := stateResponse{
		RoomCode: code,
		Ready:    state.Ready,
		Resolved: state.Resolved,
		Players:  make([]statePlayer, len(state.Players)),
	}
	for i, player := range state.Players {
		response.Players[i] = statePlayer{
			Role:      playerRole(i),
			Wins:      player.Wins,
			Submitted: player.Submitted,
		}
	}
	if state.Resolved {
		response.Result = state.Result
		response.Moves = make([]stateMove, len(state.Moves))
		for i, move := range state.Moves {
			response.Moves[i] = stateMove{Role: playerRole(i), Move: move.Move}
		}
	}
	return response
}

func (rt *router) startNextRound(w http.ResponseWriter, r *http.Request) {
	code, state, ok := rt.findRoom(w, r.PathValue("code"))
	if !ok {
		return
	}
	if _, ok := authenticatedPlayer(r, state); !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	switch err := rt.store.StartNextRound(code); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, room.ErrRoundNotResolved):
		writeError(w, http.StatusConflict, "round is not resolved")
	case errors.Is(err, room.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room not found")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (rt *router) findRoom(w http.ResponseWriter, rawCode string) (string, room.State, bool) {
	code := normalizeRoomCode(rawCode)
	if code == "" {
		writeError(w, http.StatusBadRequest, "room code is required")
		return "", room.State{}, false
	}

	state, err := rt.store.State(code)
	if errors.Is(err, room.ErrRoomNotFound) {
		writeError(w, http.StatusNotFound, "room not found")
		return "", room.State{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return "", room.State{}, false
	}
	return code, state, true
}

type roomCredentials struct {
	RoomCode    string `json:"room_code"`
	PlayerToken string `json:"player_token"`
}

type moveRequest struct {
	Move game.Move `json:"move"`
}

type stateResponse struct {
	RoomCode string        `json:"room_code"`
	Ready    bool          `json:"ready"`
	Resolved bool          `json:"resolved"`
	Players  []statePlayer `json:"players"`
	Result   game.Result   `json:"result,omitempty"`
	Moves    []stateMove   `json:"moves,omitempty"`
}

type statePlayer struct {
	Role      string `json:"role"`
	Wins      int    `json:"wins"`
	Submitted bool   `json:"submitted"`
}

type stateMove struct {
	Role string    `json:"role"`
	Move game.Move `json:"move"`
}

func normalizeRoomCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func authenticatedPlayer(r *http.Request, state room.State) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}

	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	for _, player := range state.Players {
		if player.ID == token {
			return player.ID, true
		}
	}
	return "", false
}

func playerRole(index int) string {
	if index == 0 {
		return "host"
	}
	return "guest"
}

func supportsStreaming(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(interface{ FlushError() error }); ok {
			return true
		}
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = unwrapper.Unwrap()
	}
}

func writeRoomEvent(w io.Writer, controller *http.ResponseController, snapshot room.Snapshot) error {
	data, err := json.Marshal(publicRoomState(snapshot.State.Code, snapshot.State))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", snapshot.Revision, data); err != nil {
		return err
	}
	return controller.Flush()
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func randomRoomCode() (string, error) {
	random := make([]byte, roomCodeLength)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", fmt.Errorf("generate room code: %w", err)
	}

	code := make([]byte, roomCodeLength)
	for i, value := range random {
		code[i] = roomCodeAlphabet[int(value)%len(roomCodeAlphabet)]
	}
	return string(code), nil
}

func randomPlayerToken() (string, error) {
	random := make([]byte, playerTokenBytes)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", fmt.Errorf("generate player token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
