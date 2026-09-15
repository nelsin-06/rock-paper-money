package roomhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestLoggingCorrelatesServerGeneratedIDAndOmitsSecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := observeRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"player_token":"response-secret"}`))
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/rooms?secret=query-secret", strings.NewReader("body-secret"))
	request.Header.Set("Authorization", "Bearer header-secret")
	request.Header.Set("X-Room-Token", "room-secret")
	request.Header.Set("X-Request-ID", "untrusted-id")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	requestID := response.Header().Get("X-Request-ID")
	if requestID == "" || requestID == "untrusted-id" {
		t.Fatalf("server request ID = %q", requestID)
	}
	for _, secret := range []string{"query-secret", "body-secret", "header-secret", "room-secret", "response-secret", "untrusted-id"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs exposed %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `"request_id":"`+requestID+`"`) || !strings.Contains(logs.String(), `"status":201`) {
		t.Fatalf("correlation fields missing: %s", logs.String())
	}
}

func TestEveryRequestReceivesUniqueRequestID(t *testing.T) {
	handler := observeRequests(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	first := httptest.NewRecorder()
	second := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/first", nil))
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/second", nil))
	if first.Header().Get("X-Request-ID") == "" || first.Header().Get("X-Request-ID") == second.Header().Get("X-Request-ID") {
		t.Fatalf("request IDs = %q and %q", first.Header().Get("X-Request-ID"), second.Header().Get("X-Request-ID"))
	}
}

func TestPreCommitPanicReturnsOneSafeInternalErrorLog(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := observeRequests(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(errors.New("storage unavailable"))
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.Bytes()
	var problem apiErrorResponse
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "internal_error" || problem.Message != "An internal error occurred." || problem.RawError != "" || problem.Meta.RequestID != response.Header().Get("X-Request-ID") {
		t.Fatalf("problem = %#v", problem)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["rawError"]; exists {
		t.Fatalf("internal response serialized rawError: %s", body)
	}
	if strings.Contains(string(body), "storage unavailable") {
		t.Fatalf("panic detail leaked in response: %s", body)
	}
	errorEntries := errorLogEntries(t, logs.String())
	if len(errorEntries) != 1 || len(logEntries(t, logs.String())) != 1 {
		t.Fatalf("log counts = total %d, errors %d, want 1 each; logs=%s", len(logEntries(t, logs.String())), len(errorEntries), logs.String())
	}
	entry := errorEntries[0]
	if entry["status"] != float64(500) || entry["request_id"] != problem.Meta.RequestID || entry["error"] != "storage unavailable" || entry["kind"] != "panic" || entry["phase"] != "before_commit" {
		t.Fatalf("panic error log = %#v", entry)
	}
}

func TestPostCommitPanicLogsOnceAndRepanics(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "partial success", status: http.StatusOK},
		{name: "500 already committed", status: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			handler := observeRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("partial response"))
				panic(errors.New("stream encoder failed"))
			}))
			response := httptest.NewRecorder()
			panicValue := serveAndRecover(handler, response, httptest.NewRequest(http.MethodGet, "/events", nil))

			if panicValue != http.ErrAbortHandler {
				t.Fatalf("post-commit panic = %v, want http.ErrAbortHandler", panicValue)
			}
			if response.Code != tt.status || response.Body.String() != "partial response" {
				t.Fatalf("committed response = status %d body %q", response.Code, response.Body.String())
			}
			errorEntries := errorLogEntries(t, logs.String())
			if len(errorEntries) != 1 || len(logEntries(t, logs.String())) != 1 {
				t.Fatalf("log counts = total %d, errors %d, want 1 each; logs=%s", len(logEntries(t, logs.String())), len(errorEntries), logs.String())
			}
			entry := errorEntries[0]
			if entry["error"] != "stream encoder failed" || entry["kind"] != "panic" || entry["phase"] != "after_commit" || entry["request_id"] != response.Header().Get("X-Request-ID") {
				t.Fatalf("post-commit panic log = %#v", entry)
			}
		})
	}
}

func TestSSEFailureLogPreservesCauseAndCorrelation(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	rt := &router{logger: logger}
	request := httptest.NewRequest(http.MethodGet, "/api/rooms/ABC234/events", nil)
	request = request.WithContext(context.WithValue(request.Context(), requestIDContextKey{}, "request-123"))

	rt.logSSEFailure(request, "room_event", errors.New("connection reset by peer"))

	entries := errorLogEntries(t, logs.String())
	if len(entries) != 1 || entries[0]["request_id"] != "request-123" || entries[0]["error"] != "connection reset by peer" || entries[0]["kind"] != "sse_error" || entries[0]["phase"] != "room_event" {
		t.Fatalf("SSE error logs = %#v", entries)
	}
}

func serveAndRecover(handler http.Handler, response http.ResponseWriter, request *http.Request) (panicValue any) {
	defer func() { panicValue = recover() }()
	handler.ServeHTTP(response, request)
	return nil
}

func errorLogEntries(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, entry := range logEntries(t, logs) {
		if entry["level"] == "ERROR" {
			entries = append(entries, entry)
		}
	}
	return entries
}

func logEntries(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log entry: %v; line=%s", err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}
