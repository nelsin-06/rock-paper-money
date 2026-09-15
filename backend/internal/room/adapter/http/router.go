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
	logger   *slog.Logger
}

func NewRouter(service *application.Service, verifier auth.Verifier, ready func(context.Context) error, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	handler := &router{service: service, verifier: verifier, ready: ready, logger: logger}

	mux := http.NewServeMux()
	registerRoute(mux, http.MethodGet, "/api/health", health)
	registerRoute(mux, http.MethodGet, "/api/ready", handler.readiness)
	registerRoute(mux, http.MethodPost, "/api/rooms", handler.protected(handler.createRoom))
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/join", handler.protected(handler.joinRoom))
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/moves", handler.protected(handler.submitMove))
	registerRoute(mux, http.MethodGet, "/api/rooms/{code}/state", handler.roomState)
	registerRoute(mux, http.MethodGet, "/api/rooms/{code}/events", handler.roomEvents)
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/validate-session", handler.protected(handler.validateSession))
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/next-round", handler.protected(handler.startNextRound))
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/leave", handler.protected(handler.leaveRoom))
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/presence", handler.protected(handler.refreshPresence))
	registerRoute(mux, http.MethodGet, "/api/wallet", handler.protected(handler.walletBalance))
	registerRoute(mux, http.MethodPost, "/api/wallet/recharges", handler.protected(handler.rechargeWallet))
	registerRoute(mux, http.MethodGet, "/api/analytics/rounds", handler.protected(handler.analytics))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeAPIError(w, r, errorEndpointNotFound) })
	return observeRequests(logger, mux)
}

func registerRoute(mux *http.ServeMux, method, path string, handler http.HandlerFunc) {
	mux.HandleFunc(method+" "+path, handler)
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		allowed := method
		if method == http.MethodGet {
			allowed += ", " + http.MethodHead
		}
		w.Header().Set("Allow", allowed)
		writeAPIError(w, r, errorMethodNotAllowed)
	})
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func (rt *router) readiness(w http.ResponseWriter, r *http.Request) {
	if rt.ready != nil && rt.ready(r.Context()) != nil {
		writeAPIError(w, r, errorNotReady)
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
			writeAPIError(w, r, errorUnauthorized)
			return
		}
		principal, err := rt.verifier.Verify(r.Context(), token)
		if err != nil || principal.Subject == "" {
			writeAPIError(w, r, errorUnauthorized)
			return
		}
		next(w, r, principal)
	}
}

func (rt *router) createRoom(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	credentials, err := rt.service.Create(r.Context(), principal.Subject)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, roomCredentials{RoomCode: credentials.RoomCode, PlayerToken: credentials.PlayerToken})
	case errors.Is(err, application.ErrInsufficientFunds):
		writeAPIError(w, r, errorInsufficientFunds)
	default:
		writeInternalError(w, r, "create_room", err)
	}
}

func (rt *router) joinRoom(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeAPIError(w, r, errorRoomCodeRequired)
		return
	}

	credentials, err := rt.service.Join(r.Context(), code, principal.Subject)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, roomCredentials{RoomCode: code, PlayerToken: credentials.PlayerToken})
		return
	case errors.Is(err, domain.ErrRoomFull):
		writeAPIError(w, r, errorRoomFull)
		return
	case errors.Is(err, domain.ErrRoomClosed):
		writeAPIError(w, r, errorRoomClosed)
		return
	case errors.Is(err, application.ErrRoomNotFound):
		writeAPIError(w, r, errorRoomNotFound)
		return
	case errors.Is(err, application.ErrAccountSeated):
		writeAPIError(w, r, errorAccountSeated)
		return
	case errors.Is(err, application.ErrInsufficientFunds):
		writeAPIError(w, r, errorInsufficientFunds)
		return
	default:
		writeInternalError(w, r, "join_room", err)
	}
}

func (rt *router) submitMove(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeAPIError(w, r, errorRoomCodeRequired)
		return
	}
	token, ok := roomToken(r)
	if !ok {
		writeAPIError(w, r, errorUnauthorized)
		return
	}
	var request moveRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeAPIError(w, r, errorInvalidBody)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, r, errorInvalidBody)
		return
	}
	if !domain.IsValidMove(request.Move) {
		writeAPIError(w, r, errorInvalidMove)
		return
	}

	switch err := rt.service.SubmitMove(r.Context(), code, token, principal.Subject, request.Move); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, domain.ErrRoomNotReady):
		writeAPIError(w, r, errorRoomNotReady)
	case errors.Is(err, domain.ErrDuplicateMove):
		writeAPIError(w, r, errorDuplicateMove)
	case errors.Is(err, domain.ErrRoundResolved):
		writeAPIError(w, r, errorRoundResolved)
	case errors.Is(err, domain.ErrRoomClosed):
		writeAPIError(w, r, errorRoomClosed)
	case errors.Is(err, application.ErrRoomNotFound):
		writeAPIError(w, r, errorRoomNotFound)
	case errors.Is(err, application.ErrUnauthorized):
		writeAPIError(w, r, errorUnauthorized)
	case errors.Is(err, application.ErrInsufficientFunds):
		writeAPIError(w, r, errorInsufficientFunds)
	default:
		writeInternalError(w, r, "submit_move", err)
	}
}

func (rt *router) roomState(w http.ResponseWriter, r *http.Request) {
	code, state, ok := rt.findRoom(r, w, r.PathValue("code"))
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, publicRoomState(code, state))
}

func (rt *router) validateSession(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code, state, found := rt.findRoom(r, w, r.PathValue("code"))
	if !found {
		return
	}
	if state.Closed {
		writeAPIError(w, r, errorRoomNotFound)
		return
	}
	token, valid := roomToken(r)
	if !valid {
		writeAPIError(w, r, errorUnauthorized)
		return
	}
	_, role, err := rt.service.Authenticate(r.Context(), code, token, principal.Subject)
	if errors.Is(err, application.ErrRoomNotFound) {
		writeAPIError(w, r, errorRoomNotFound)
		return
	}
	if err != nil {
		writeAPIError(w, r, errorUnauthorized)
		return
	}
	var request validateSessionRequest
	if !decodeJSONBody(r, &request) || (request.Role != "host" && request.Role != "guest") {
		writeAPIError(w, r, errorInvalidBody)
		return
	}
	if request.Role != role {
		writeAPIError(w, r, errorUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (rt *router) roomEvents(w http.ResponseWriter, r *http.Request) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeAPIError(w, r, errorRoomCodeRequired)
		return
	}
	if !supportsStreaming(w) {
		writeInternalError(w, r, "open_room_events", errors.New("response writer does not support streaming"))
		return
	}

	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		writeInternalError(w, r, "configure_room_events", err)
		return
	}

	initial, changes, unsubscribe, err := rt.service.Subscribe(r.Context(), code)
	if errors.Is(err, application.ErrRoomNotFound) {
		writeAPIError(w, r, errorRoomNotFound)
		return
	}
	if err != nil {
		writeInternalError(w, r, "subscribe_room_events", err)
		return
	}
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if err := writeRoomEvent(w, controller, initial); err != nil {
		rt.logSSEFailure(r, "initial_event", err)
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
			if err != nil {
				rt.logSSEFailure(r, "snapshot", err)
				return
			}
			if snapshot.Revision <= lastRevision {
				continue
			}
			if err := writeRoomEvent(w, controller, snapshot); err != nil {
				rt.logSSEFailure(r, "room_event", err)
				return
			}
			lastRevision = snapshot.Revision
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				rt.logSSEFailure(r, "heartbeat_write", err)
				return
			}
			if err := controller.Flush(); err != nil {
				rt.logSSEFailure(r, "heartbeat_flush", err)
				return
			}
		}
	}
}

func (rt *router) logSSEFailure(r *http.Request, phase string, cause error) {
	logInternalFailure(rt.logger, r, internalFailure{
		cause:     cause,
		operation: "stream_room_events",
		kind:      "sse_error",
		phase:     phase,
	})
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
	code, _, ok := rt.findRoom(r, w, r.PathValue("code"))
	if !ok {
		return
	}
	token, ok := roomToken(r)
	if !ok {
		writeAPIError(w, r, errorUnauthorized)
		return
	}
	if _, _, err := rt.service.Authenticate(r.Context(), code, token, principal.Subject); err != nil {
		writeAPIError(w, r, errorUnauthorized)
		return
	}
	var request nextRoundRequest
	if !decodeJSONBody(r, &request) || request.Round == 0 {
		writeAPIError(w, r, errorInvalidBody)
		return
	}

	switch err := rt.service.RequestNextRound(r.Context(), code, token, principal.Subject, request.Round); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, domain.ErrRoundNotResolved):
		writeAPIError(w, r, errorRoundNotResolved)
	case errors.Is(err, domain.ErrNextRoundRequested):
		writeAPIError(w, r, errorNextRoundRequested)
	case errors.Is(err, domain.ErrStaleRound):
		writeAPIError(w, r, errorStaleRound)
	case errors.Is(err, domain.ErrRoomClosed):
		writeAPIError(w, r, errorRoomClosed)
	case errors.Is(err, application.ErrRoomNotFound):
		writeAPIError(w, r, errorRoomNotFound)
	case errors.Is(err, application.ErrUnauthorized):
		writeAPIError(w, r, errorUnauthorized)
	case errors.Is(err, application.ErrInsufficientFunds):
		writeAPIError(w, r, errorInsufficientFunds)
	default:
		writeInternalError(w, r, "request_next_round", err)
	}
}

func (rt *router) leaveRoom(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code, _, ok := rt.findRoom(r, w, r.PathValue("code"))
	if !ok {
		return
	}
	token, ok := roomToken(r)
	if !ok {
		writeAPIError(w, r, errorUnauthorized)
		return
	}

	switch err := rt.service.Leave(r.Context(), code, token, principal.Subject); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, domain.ErrRoomClosed):
		writeAPIError(w, r, errorRoomClosed)
	case errors.Is(err, domain.ErrGameUnfinished):
		writeAPIError(w, r, errorGameUnfinished)
	case errors.Is(err, application.ErrRoomNotFound):
		writeAPIError(w, r, errorRoomNotFound)
	case errors.Is(err, application.ErrUnauthorized):
		writeAPIError(w, r, errorUnauthorized)
	default:
		writeInternalError(w, r, "leave_room", err)
	}
}

func (rt *router) refreshPresence(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeAPIError(w, r, errorRoomCodeRequired)
		return
	}
	token, ok := roomToken(r)
	if !ok {
		writeAPIError(w, r, errorUnauthorized)
		return
	}
	switch err := rt.service.RefreshPresence(r.Context(), code, token, principal.Subject); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, application.ErrRoomNotFound):
		writeAPIError(w, r, errorRoomNotFound)
	case errors.Is(err, application.ErrUnauthorized):
		writeAPIError(w, r, errorUnauthorized)
	default:
		writeInternalError(w, r, "refresh_presence", err)
	}
}

func (rt *router) walletBalance(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	balance, err := rt.service.Balance(r.Context(), principal.Subject)
	if err != nil {
		writeInternalError(w, r, "wallet_balance", err)
		return
	}
	writeJSON(w, http.StatusOK, walletResponse{Balance: balance})
}

func (rt *router) rechargeWallet(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || strings.TrimSpace(keys[0]) != keys[0] || strings.ContainsAny(keys[0], " \t\r\n") {
		writeAPIError(w, r, errorIdempotencyRequired)
		return
	}
	var request rechargeRequest
	if !decodeJSONBody(r, &request) || request.Amount <= 0 {
		writeAPIError(w, r, errorInvalidCoinAmount)
		return
	}
	balance, err := rt.service.Recharge(r.Context(), principal.Subject, request.Amount, keys[0])
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, walletResponse{Balance: balance})
	case errors.Is(err, application.ErrInvalidCoinAmount):
		writeAPIError(w, r, errorInvalidCoinAmount)
	case errors.Is(err, application.ErrIdempotencyRequired):
		writeAPIError(w, r, errorIdempotencyRequired)
	case errors.Is(err, application.ErrIdempotencyConflict):
		writeAPIError(w, r, errorIdempotencyConflict)
	default:
		writeInternalError(w, r, "recharge_wallet", err)
	}
}

func (rt *router) analytics(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	report, err := rt.service.Analytics(r.Context(), principal.Subject)
	if err != nil {
		writeInternalError(w, r, "list_round_analytics", err)
		return
	}
	response := analyticsResponse{TotalHouseEarnings: report.TotalHouseEarnings, Rounds: make([]playedRoundResponse, len(report.Rounds))}
	for index, round := range report.Rounds {
		response.Rounds[index] = playedRoundResponse{RoomCode: round.RoomCode, Round: round.Round, Result: round.Result, WinnerRole: round.WinnerRole, Forfeit: round.Forfeit, HouseEarnings: round.HouseEarnings, ResolvedAt: round.ResolvedAt}
	}
	writeJSON(w, http.StatusOK, response)
}

func (rt *router) findRoom(r *http.Request, w http.ResponseWriter, rawCode string) (string, domain.State, bool) {
	code := normalizeRoomCode(rawCode)
	if code == "" {
		writeAPIError(w, r, errorRoomCodeRequired)
		return "", domain.State{}, false
	}

	snapshot, err := rt.service.Snapshot(r.Context(), code)
	if errors.Is(err, application.ErrRoomNotFound) {
		writeAPIError(w, r, errorRoomNotFound)
		return "", domain.State{}, false
	}
	if err != nil {
		writeInternalError(w, r, "find_room", err)
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

type rechargeRequest struct {
	Amount int64 `json:"amount"`
}

type walletResponse struct {
	Balance int64 `json:"balance,string"`
}

type analyticsResponse struct {
	TotalHouseEarnings int64                 `json:"total_house_earnings,string"`
	Rounds             []playedRoundResponse `json:"rounds"`
}

type playedRoundResponse struct {
	RoomCode      string        `json:"room_code"`
	Round         uint64        `json:"round"`
	Result        domain.Result `json:"result"`
	WinnerRole    string        `json:"winner_role,omitempty"`
	Forfeit       bool          `json:"forfeit"`
	HouseEarnings int64         `json:"house_earnings,string"`
	ResolvedAt    time.Time     `json:"resolved_at"`
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
