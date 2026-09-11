package roomhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"example.com/rock-paper-money/internal/auth"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

const (
	roomCodeLength    = 6
	roomCodeAlphabet  = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	heartbeatInterval = 15 * time.Second
)

type router struct {
	service  *application.Service
	verifier auth.Verifier
	ready    func(context.Context) error
}

func NewRouter(service *application.Service, verifier auth.Verifier, ready func(context.Context) error) http.Handler {
	handler := &router{service: service, verifier: verifier, ready: ready}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", health)
	mux.HandleFunc("GET /api/ready", handler.readiness)
	mux.HandleFunc("POST /api/rooms", handler.protected(handler.createRoom))
	mux.HandleFunc("POST /api/rooms/{code}/join", handler.protected(handler.joinRoom))
	mux.HandleFunc("POST /api/rooms/{code}/moves", handler.protected(handler.submitMove))
	mux.HandleFunc("GET /api/rooms/{code}/state", handler.roomState)
	mux.HandleFunc("GET /api/rooms/{code}/events", handler.roomEvents)
	mux.HandleFunc("POST /api/rooms/{code}/validate-session", handler.protected(handler.validateSession))
	mux.HandleFunc("POST /api/rooms/{code}/next-round", handler.protected(handler.startNextRound))
	mux.HandleFunc("POST /api/rooms/{code}/leave", handler.protected(handler.leaveRoom))
	mux.HandleFunc("POST /api/rooms/{code}/presence", handler.protected(handler.refreshPresence))
	return logRequests(slog.Default(), mux)
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func (rt *router) readiness(w http.ResponseWriter, r *http.Request) {
	if rt.ready != nil && rt.ready(r.Context()) != nil {
		writeError(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func (rt *router) protected(next func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok || rt.verifier == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		principal, err := rt.verifier.Verify(r.Context(), token)
		if err != nil || principal.Subject == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r, principal)
	}
}

func (rt *router) createRoom(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	credentials, err := rt.service.Create(r.Context(), principal.Subject)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusCreated, roomCredentials{RoomCode: credentials.RoomCode, PlayerToken: credentials.PlayerToken})
}

func (rt *router) joinRoom(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeError(w, http.StatusBadRequest, "room code is required")
		return
	}

	credentials, err := rt.service.Join(r.Context(), code, principal.Subject)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, roomCredentials{RoomCode: code, PlayerToken: credentials.PlayerToken})
		return
	case errors.Is(err, domain.ErrRoomFull):
		writeError(w, http.StatusConflict, "room is full")
		return
	case errors.Is(err, domain.ErrRoomClosed):
		writeError(w, http.StatusConflict, "room is closed")
		return
	case errors.Is(err, application.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room not found")
		return
	case errors.Is(err, application.ErrAccountSeated):
		writeError(w, http.StatusConflict, "account already occupies a seat")
		return
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (rt *router) submitMove(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code, _, ok := rt.findRoom(r.Context(), w, r.PathValue("code"))
	if !ok {
		return
	}
	token, ok := roomToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, _, err := rt.service.Authenticate(r.Context(), code, token, principal.Subject); err != nil {
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
	if !domain.IsValidMove(request.Move) {
		writeError(w, http.StatusBadRequest, "invalid move")
		return
	}

	switch err := rt.service.SubmitMove(r.Context(), code, token, principal.Subject, request.Move); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, domain.ErrRoomNotReady):
		writeError(w, http.StatusConflict, "room not ready")
	case errors.Is(err, domain.ErrDuplicateMove):
		writeError(w, http.StatusConflict, "move already submitted")
	case errors.Is(err, domain.ErrRoundResolved):
		writeError(w, http.StatusConflict, "round is already resolved")
	case errors.Is(err, domain.ErrRoomClosed):
		writeError(w, http.StatusConflict, "room is closed")
	case errors.Is(err, application.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room not found")
	case errors.Is(err, application.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (rt *router) roomState(w http.ResponseWriter, r *http.Request) {
	code, state, ok := rt.findRoom(r.Context(), w, r.PathValue("code"))
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, publicRoomState(code, state))
}

func (rt *router) validateSession(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code, state, found := rt.findRoom(r.Context(), w, r.PathValue("code"))
	if !found {
		return
	}
	if state.Closed {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	token, valid := roomToken(r)
	if !valid {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	_, role, err := rt.service.Authenticate(r.Context(), code, token, principal.Subject)
	if errors.Is(err, application.ErrRoomNotFound) {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request validateSessionRequest
	if !decodeJSONBody(r, &request) || (request.Role != "host" && request.Role != "guest") {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if request.Role != role {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

	initial, changes, unsubscribe, err := rt.service.Subscribe(r.Context(), code)
	if errors.Is(err, application.ErrRoomNotFound) {
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
			snapshot, err := rt.service.Snapshot(r.Context(), code)
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

func publicRoomState(code string, state domain.State) stateResponse {
	response := stateResponse{
		RoomCode: code,
		Ready:    state.Ready,
		Resolved: state.Resolved,
		Round:    state.Round,
		Closed:   state.Closed,
		Forfeit:  state.Forfeit,
		Players:  make([]statePlayer, len(state.Players)),
	}
	for i, player := range state.Players {
		response.Players[i] = statePlayer{
			Role:           playerRole(i),
			Wins:           player.Wins,
			Submitted:      player.Submitted,
			WantsNextRound: player.WantsNextRound,
		}
	}
	if state.Resolved {
		response.Result = state.Result
		response.Moves = make([]stateMove, len(state.Moves))
		for i, move := range state.Moves {
			response.Moves[i] = stateMove{Role: roleForPlayer(state.Players, move.PlayerID), Move: move.Move}
		}
	}
	return response
}

func (rt *router) startNextRound(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code, _, ok := rt.findRoom(r.Context(), w, r.PathValue("code"))
	if !ok {
		return
	}
	token, ok := roomToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, _, err := rt.service.Authenticate(r.Context(), code, token, principal.Subject); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request nextRoundRequest
	if !decodeJSONBody(r, &request) || request.Round == 0 {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch err := rt.service.RequestNextRound(r.Context(), code, token, principal.Subject, request.Round); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, domain.ErrRoundNotResolved):
		writeError(w, http.StatusConflict, "round is not resolved")
	case errors.Is(err, domain.ErrNextRoundRequested):
		writeError(w, http.StatusConflict, "next round already requested")
	case errors.Is(err, domain.ErrStaleRound):
		writeError(w, http.StatusConflict, "round request is stale")
	case errors.Is(err, domain.ErrRoomClosed):
		writeError(w, http.StatusConflict, "room is closed")
	case errors.Is(err, application.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room not found")
	case errors.Is(err, application.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (rt *router) leaveRoom(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code, _, ok := rt.findRoom(r.Context(), w, r.PathValue("code"))
	if !ok {
		return
	}
	token, ok := roomToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	switch err := rt.service.Leave(r.Context(), code, token, principal.Subject); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, domain.ErrRoomClosed):
		writeError(w, http.StatusConflict, "room is closed")
	case errors.Is(err, domain.ErrGameUnfinished):
		writeError(w, http.StatusConflict, "game is unfinished")
	case errors.Is(err, application.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room not found")
	case errors.Is(err, application.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (rt *router) refreshPresence(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeError(w, http.StatusBadRequest, "room code is required")
		return
	}
	token, ok := roomToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch err := rt.service.RefreshPresence(r.Context(), code, token, principal.Subject); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, application.ErrRoomNotFound):
		writeError(w, http.StatusNotFound, "room not found")
	case errors.Is(err, application.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (rt *router) findRoom(ctx context.Context, w http.ResponseWriter, rawCode string) (string, domain.State, bool) {
	code := normalizeRoomCode(rawCode)
	if code == "" {
		writeError(w, http.StatusBadRequest, "room code is required")
		return "", domain.State{}, false
	}

	snapshot, err := rt.service.Snapshot(ctx, code)
	if errors.Is(err, application.ErrRoomNotFound) {
		writeError(w, http.StatusNotFound, "room not found")
		return "", domain.State{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return "", domain.State{}, false
	}
	return code, snapshot.State, true
}

type roomCredentials struct {
	RoomCode    string `json:"room_code"`
	PlayerToken string `json:"player_token"`
}

type moveRequest struct {
	Move domain.Move `json:"move"`
}

type nextRoundRequest struct {
	Round uint64 `json:"round"`
}

type validateSessionRequest struct {
	Role string `json:"role"`
}

type stateResponse struct {
	RoomCode string        `json:"room_code"`
	Ready    bool          `json:"ready"`
	Resolved bool          `json:"resolved"`
	Round    uint64        `json:"round"`
	Closed   bool          `json:"closed"`
	Forfeit  bool          `json:"forfeit"`
	Players  []statePlayer `json:"players"`
	Result   domain.Result `json:"result,omitempty"`
	Moves    []stateMove   `json:"moves,omitempty"`
}

type statePlayer struct {
	Role           string `json:"role"`
	Wins           int    `json:"wins"`
	Submitted      bool   `json:"submitted"`
	WantsNextRound bool   `json:"wants_next_round"`
}

type stateMove struct {
	Role string      `json:"role"`
	Move domain.Move `json:"move"`
}

func normalizeRoomCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}

	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func roomToken(r *http.Request) (string, bool) {
	values := r.Header.Values("X-Room-Token")
	if len(values) != 1 || values[0] == "" || strings.ContainsAny(values[0], " \t\r\n") {
		return "", false
	}
	return values[0], true
}

func playerRole(index int) string {
	if index == 0 {
		return "host"
	}
	return "guest"
}

func roleForPlayer(players []domain.Player, playerID string) string {
	for index, player := range players {
		if player.ID == playerID {
			return playerRole(index)
		}
	}
	return ""
}

func decodeJSONBody(r *http.Request, destination any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
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

func writeRoomEvent(w io.Writer, controller *http.ResponseController, snapshot application.Snapshot) error {
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
