package roomhttp

import (
	"encoding/json"
	"net/http"
	"time"
)

type apiErrorDefinition struct {
	status   int
	code     string
	message  string
	rawError string
}

type apiErrorResponse struct {
	Status   int          `json:"status"`
	Code     string       `json:"code"`
	Message  string       `json:"message"`
	Meta     apiErrorMeta `json:"meta"`
	RawError string       `json:"rawError,omitempty"`
}

type apiErrorMeta struct {
	Time      string `json:"time"`
	RequestID string `json:"requestId"`
}

var (
	errorNotReady            = publicError(http.StatusServiceUnavailable, "service_unavailable", "Service is unavailable.")
	errorUnauthorized        = publicError(http.StatusUnauthorized, "unauthorized", "Unauthorized.")
	errorRoomCodeRequired    = publicError(http.StatusBadRequest, "room_code_required", "Room code is required.")
	errorInvalidBody         = publicError(http.StatusBadRequest, "invalid_request_body", "Invalid request body.")
	errorInvalidMove         = publicError(http.StatusBadRequest, "invalid_move", "Invalid move.")
	errorEndpointNotFound    = publicError(http.StatusNotFound, "endpoint_not_found", "Endpoint not found.")
	errorMethodNotAllowed    = publicError(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
	errorRoomFull            = businessError(http.StatusConflict, "room_full", "Room is full.", "room already has two players")
	errorRoomClosed          = businessError(http.StatusConflict, "room_closed", "Room is closed.", "room is closed")
	errorRoomNotFound        = businessError(http.StatusNotFound, "room_not_found", "Room not found.", "room code was not found")
	errorAccountSeated       = businessError(http.StatusConflict, "account_already_seated", "Account already occupies a seat.", "account already occupies a seat")
	errorRoomNotReady        = businessError(http.StatusConflict, "room_not_ready", "Room is not ready.", "room needs a second player")
	errorDuplicateMove       = businessError(http.StatusConflict, "move_already_submitted", "Move already submitted.", "player already submitted a move")
	errorRoundResolved       = businessError(http.StatusConflict, "round_already_resolved", "Round is already resolved.", "round is already resolved")
	errorRoundNotResolved    = businessError(http.StatusConflict, "round_not_resolved", "Round is not resolved.", "round is not resolved")
	errorNextRoundRequested  = businessError(http.StatusConflict, "next_round_already_requested", "Next round already requested.", "player already requested another round")
	errorStaleRound          = businessError(http.StatusConflict, "stale_round", "Round request is stale.", "round request is stale")
	errorGameUnfinished      = businessError(http.StatusConflict, "game_unfinished", "Game is unfinished.", "game is unfinished")
	errorInsufficientFunds   = businessError(http.StatusConflict, "insufficient_balance", "Insufficient coin balance.", "insufficient coin balance")
	errorInvalidCoinAmount   = publicError(http.StatusBadRequest, "invalid_coin_amount", "Coin amount must be a positive integer.")
	errorIdempotencyRequired = publicError(http.StatusBadRequest, "idempotency_key_required", "A valid Idempotency-Key header is required.")
	errorIdempotencyConflict = businessError(http.StatusConflict, "idempotency_conflict", "Idempotency key was already used with different data.", "idempotency key was already used with different data")
)

func publicError(status int, code, message string) apiErrorDefinition {
	return apiErrorDefinition{status: status, code: code, message: message}
}

func businessError(status int, code, message, rawError string) apiErrorDefinition {
	return apiErrorDefinition{status: status, code: code, message: message, rawError: rawError}
}

func writeAPIError(w http.ResponseWriter, r *http.Request, definition apiErrorDefinition) {
	writeJSON(w, definition.status, apiErrorResponse{
		Status:   definition.status,
		Code:     definition.code,
		Message:  definition.message,
		RawError: definition.rawError,
		Meta: apiErrorMeta{
			Time:      time.Now().UTC().Format(time.RFC3339Nano),
			RequestID: requestIDFromContext(r.Context()),
		},
	})
}

func writeInternalError(w http.ResponseWriter, r *http.Request, operation string, cause error) {
	if recorder, ok := w.(interface{ recordInternalFailure(internalFailure) }); ok {
		recorder.recordInternalFailure(internalFailure{
			cause:     cause,
			operation: operation,
			kind:      "internal_error",
			phase:     "before_commit",
		})
	}
	writeInternalErrorResponse(w, r)
}

func writeInternalErrorResponse(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, r, publicError(http.StatusInternalServerError, "internal_error", "An internal error occurred."))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
