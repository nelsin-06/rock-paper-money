package roomhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"example.com/rock-paper-money/internal/auth"
	"example.com/rock-paper-money/internal/latency"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

const (
	roomCodeLength   = 6
	roomCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

type ReadinessStatus struct {
	Status     string `json:"status"`
	PostgreSQL string `json:"postgresql"`
	Realtime   string `json:"realtime"`
}

type router struct {
	service  *application.Service
	verifier auth.Verifier
	sessions *auth.SessionService
	security *auth.RequestSecurity
	ready    func(context.Context) error
	status   func(context.Context) (ReadinessStatus, int)
	metrics  http.Handler
	logger   *slog.Logger
	latency  *latency.Observer
}

func NewRouter(service *application.Service, verifier auth.Verifier, ready func(context.Context) error, logger *slog.Logger) http.Handler {
	return NewRouterWithWebSocket(service, verifier, ready, logger, nil)
}

func NewRouterWithWebSocket(service *application.Service, verifier auth.Verifier, ready func(context.Context) error, logger *slog.Logger, socket http.Handler) http.Handler {
	return newRouter(service, verifier, nil, nil, ready, nil, logger, socket, nil)
}

func NewSessionRouter(service *application.Service, verifier auth.Verifier, sessions *auth.SessionService, security *auth.RequestSecurity, ready func(context.Context) error, logger *slog.Logger) http.Handler {
	return NewSessionRouterWithWebSocket(service, verifier, sessions, security, ready, logger, nil)
}

func NewSessionRouterWithWebSocket(service *application.Service, verifier auth.Verifier, sessions *auth.SessionService, security *auth.RequestSecurity, ready func(context.Context) error, logger *slog.Logger, socket http.Handler) http.Handler {
	return newRouter(service, verifier, sessions, security, ready, nil, logger, socket, nil)
}

func NewOperationalRouter(service *application.Service, verifier auth.Verifier, sessions *auth.SessionService, security *auth.RequestSecurity, status func(context.Context) (ReadinessStatus, int), logger *slog.Logger, socket, metrics http.Handler, observer ...*latency.Observer) http.Handler {
	return newRouter(service, verifier, sessions, security, nil, status, logger, socket, metrics, observer...)
}

func newRouter(service *application.Service, verifier auth.Verifier, sessions *auth.SessionService, security *auth.RequestSecurity, ready func(context.Context) error, status func(context.Context) (ReadinessStatus, int), logger *slog.Logger, socket, metrics http.Handler, observer ...*latency.Observer) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	handler := &router{service: service, verifier: verifier, sessions: sessions, security: security, ready: ready, status: status, logger: logger, metrics: metrics}
	if len(observer) > 0 {
		handler.latency = observer[0]
	}

	mux := http.NewServeMux()
	registerRoute(mux, http.MethodGet, "/api/health", health)
	registerRoute(mux, http.MethodGet, "/api/ready", handler.observed("readiness", handler.readiness))
	if metrics != nil {
		mux.Handle("GET /metrics", metrics)
	}
	if sessions != nil && security != nil {
		mux.Handle("/api/session", auth.NewSessionHTTPHandler(verifier, sessions, security))
	}
	registerRoute(mux, http.MethodPost, "/api/rooms", handler.protected("create_room", handler.createRoom))
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/join", handler.protected("join_room", handler.joinRoom))
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/moves", handler.protected("submit_move", handler.submitMove))
	registerRoute(mux, http.MethodGet, "/api/rooms/{code}/state", handler.protected("room_state", handler.roomState))
	if socket != nil {
		registerRoute(mux, http.MethodGet, "/api/rooms/{code}/socket", socket.ServeHTTP)
	}
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/next-round", handler.protected("next_round", handler.startNextRound))
	registerRoute(mux, http.MethodPost, "/api/rooms/{code}/leave", handler.protected("leave_room", handler.leaveRoom))
	registerRoute(mux, http.MethodGet, "/api/wallet", handler.protected("wallet_balance", handler.walletBalance))
	registerRoute(mux, http.MethodPost, "/api/wallet/recharges", handler.protected("recharge_wallet", handler.rechargeWallet))
	registerRoute(mux, http.MethodGet, "/api/analytics/rounds", handler.protected("list_round_analytics", handler.analytics))
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
	if rt.status != nil {
		status, code := rt.status(r.Context())
		writeJSON(w, code, status)
		return
	}
	if rt.ready != nil && rt.ready(r.Context()) != nil {
		writeAPIError(w, r, errorNotReady)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func (rt *router) protected(operationName string, next func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := &statusWriter{ResponseWriter: w}
		if rt.latency != nil {
			ctx, finish := rt.latency.StartOperation(r.Context(), requestIDFromContext(r.Context()), operationName)
			r = r.WithContext(ctx)
			defer finishObservedOperation(response, finish)
		}
		defer func() {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				return
			}
			observer, ok := rt.metrics.(interface{ Receipt(string) })
			if !ok {
				return
			}
			if response.status == http.StatusConflict {
				observer.Receipt("conflict")
			} else if response.status > 0 && response.status < http.StatusInternalServerError {
				observer.Receipt("accepted")
			}
		}()
		w = response
		if rt.sessions != nil {
			finishAuthentication := rt.startPhase(r.Context(), latency.PhaseAuthentication)
			cookie, err := r.Cookie(auth.SessionCookieName)
			if err != nil || cookie.Value == "" {
				finishAuthentication()
				writeAPIError(w, r, errorUnauthorized)
				return
			}
			session, err := rt.sessions.Authenticate(r.Context(), cookie.Value)
			if err != nil || session.AccountID == "" {
				finishAuthentication()
				writeAPIError(w, r, errorUnauthorized)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if rt.security == nil || rt.security.ValidateMutation(r, session) != nil {
					finishAuthentication()
					writeAPIError(w, r, errorForbidden)
					return
				}
			}
			finishAuthentication()
			next(w, r, auth.Principal{Subject: session.AccountID})
			return
		}
		token, ok := bearerToken(r)
		if !ok || rt.verifier == nil {
			writeAPIError(w, r, errorUnauthorized)
			return
		}
		finishAuthentication := rt.startPhase(r.Context(), latency.PhaseAuthentication)
		principal, err := rt.verifier.Verify(r.Context(), token)
		finishAuthentication()
		if err != nil || principal.Subject == "" {
			writeAPIError(w, r, errorUnauthorized)
			return
		}
		next(w, r, principal)
	}
}

func (rt *router) observed(operationName string, next http.HandlerFunc) http.HandlerFunc {
	if rt.latency == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		response := &statusWriter{ResponseWriter: w}
		ctx, finish := rt.latency.StartOperation(r.Context(), requestIDFromContext(r.Context()), operationName)
		defer finishObservedOperation(response, finish)
		next(response, r.WithContext(ctx))
	}
}

func finishObservedOperation(response *statusWriter, finish func(int)) {
	if panicValue := recover(); panicValue != nil {
		status := response.status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		finish(status)
		panic(panicValue)
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	finish(status)
}

func (rt *router) startPhase(ctx context.Context, name string) func() {
	if rt.latency == nil {
		return func() {}
	}
	return rt.latency.StartPhase(ctx, name)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) recordInternalFailure(failure internalFailure) {
	if recorder, ok := w.ResponseWriter.(interface{ recordInternalFailure(internalFailure) }); ok {
		recorder.recordInternalFailure(failure)
	}
}

func (rt *router) createRoom(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeAPIError(w, r, errorIdempotencyRequired)
		return
	}
	credentials, err := rt.service.CreateCommand(r.Context(), principal.Subject, key)
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

	key, ok := idempotencyKey(r)
	if !ok {
		writeAPIError(w, r, errorIdempotencyRequired)
		return
	}
	credentials, err := rt.service.JoinCommand(r.Context(), code, principal.Subject, key)
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
	key, ok := idempotencyKey(r)
	if !ok {
		writeAPIError(w, r, errorIdempotencyRequired)
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

	switch err := rt.service.SubmitMoveCommand(r.Context(), code, principal.Subject, request.Move, key); {
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
		writeAPIError(w, r, errorRoomNotFound)
	case errors.Is(err, application.ErrIdempotencyConflict):
		writeAPIError(w, r, errorIdempotencyConflict)
	case errors.Is(err, application.ErrInsufficientFunds):
		writeAPIError(w, r, errorInsufficientFunds)
	default:
		writeInternalError(w, r, "submit_move", err)
	}
}

func (rt *router) roomState(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code, state, ok := rt.findRoomForAccount(r, w, r.PathValue("code"), principal.Subject)
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, publicRoomState(code, state))
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
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeAPIError(w, r, errorRoomCodeRequired)
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		writeAPIError(w, r, errorIdempotencyRequired)
		return
	}
	var request nextRoundRequest
	if !decodeJSONBody(r, &request) || request.Round == 0 {
		writeAPIError(w, r, errorInvalidBody)
		return
	}

	switch err := rt.service.RequestNextRoundCommand(r.Context(), code, principal.Subject, request.Round, key); {
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
		writeAPIError(w, r, errorRoomNotFound)
	case errors.Is(err, application.ErrIdempotencyConflict):
		writeAPIError(w, r, errorIdempotencyConflict)
	case errors.Is(err, application.ErrInsufficientFunds):
		writeAPIError(w, r, errorInsufficientFunds)
	default:
		writeInternalError(w, r, "request_next_round", err)
	}
}

func (rt *router) leaveRoom(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	code := normalizeRoomCode(r.PathValue("code"))
	if code == "" {
		writeAPIError(w, r, errorRoomCodeRequired)
		return
	}
	key, ok := idempotencyKey(r)
	if !ok {
		writeAPIError(w, r, errorIdempotencyRequired)
		return
	}

	switch err := rt.service.LeaveCommand(r.Context(), code, principal.Subject, key); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, domain.ErrRoomClosed):
		writeAPIError(w, r, errorRoomClosed)
	case errors.Is(err, domain.ErrGameUnfinished):
		writeAPIError(w, r, errorGameUnfinished)
	case errors.Is(err, application.ErrRoomNotFound):
		writeAPIError(w, r, errorRoomNotFound)
	case errors.Is(err, application.ErrUnauthorized):
		writeAPIError(w, r, errorRoomNotFound)
	case errors.Is(err, application.ErrIdempotencyConflict):
		writeAPIError(w, r, errorIdempotencyConflict)
	default:
		writeInternalError(w, r, "leave_room", err)
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
	key, ok := idempotencyKey(r)
	if !ok {
		writeAPIError(w, r, errorIdempotencyRequired)
		return
	}
	var request rechargeRequest
	if !decodeJSONBody(r, &request) || request.Amount <= 0 {
		writeAPIError(w, r, errorInvalidCoinAmount)
		return
	}
	balance, err := rt.service.Recharge(r.Context(), principal.Subject, request.Amount, key)
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

func (rt *router) findRoomForAccount(r *http.Request, w http.ResponseWriter, rawCode, accountID string) (string, domain.State, bool) {
	code := normalizeRoomCode(rawCode)
	if code == "" {
		writeAPIError(w, r, errorRoomCodeRequired)
		return "", domain.State{}, false
	}
	snapshot, err := rt.service.SnapshotForAccount(r.Context(), code, accountID)
	if errors.Is(err, application.ErrRoomNotFound) || errors.Is(err, application.ErrUnauthorized) {
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
	PlayerToken string `json:"player_token,omitempty"`
}

type moveRequest struct {
	Move domain.Move `json:"move"`
}

type nextRoundRequest struct {
	Round uint64 `json:"round"`
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

func idempotencyKey(r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || values[0] == "" || len(values[0]) > 128 || strings.TrimSpace(values[0]) != values[0] || strings.ContainsAny(values[0], " \t\r\n") {
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
