package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/rock-paper-money/internal/room"
)

func TestRequestLoggingCapturesFinalStatusOnce(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		writeBody  bool
		writeTwice bool
		wantBody   string
	}{
		{name: "implicit 200", status: http.StatusOK, writeBody: true, wantBody: "response"},
		{name: "explicit 201", status: http.StatusCreated, writeBody: true, writeTwice: true, wantBody: "response"},
		{name: "204", status: http.StatusNoContent},
		{name: "400", status: http.StatusBadRequest, writeBody: true, wantBody: "response"},
		{name: "401", status: http.StatusUnauthorized, writeBody: true, wantBody: "response"},
		{name: "404", status: http.StatusNotFound, writeBody: true, wantBody: "response"},
		{name: "409", status: http.StatusConflict, writeBody: true, wantBody: "response"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.status != http.StatusOK || !test.writeBody {
					w.WriteHeader(test.status)
				}
				if test.writeTwice {
					w.WriteHeader(http.StatusTeapot)
				}
				if test.writeBody {
					_, _ = w.Write([]byte("response"))
				}
			})
			handler := logRequests(logger, next)
			request := httptest.NewRequest(http.MethodPost, "/api/rooms/ABC234/moves?private=query", nil)
			request.RemoteAddr = "[2001:0db8::1]:4321"
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.status {
				t.Fatalf("response status = %d, want %d", response.Code, test.status)
			}
			if response.Body.String() != test.wantBody {
				t.Errorf("response body = %q, want %q", response.Body.String(), test.wantBody)
			}
			entries := decodeLogEntries(t, logs.String())
			if len(entries) != 1 {
				t.Fatalf("log entries = %d, want 1; logs = %s", len(entries), logs.String())
			}
			assertLogField(t, entries[0], "method", http.MethodPost)
			assertLogField(t, entries[0], "path", "/api/rooms/ABC234/moves")
			assertLogField(t, entries[0], "status", float64(test.status))
			assertLogField(t, entries[0], "client_ip", "2001:db8::1")
			if _, ok := entries[0]["duration"]; !ok {
				t.Error("log entry has no duration field")
			}
			if strings.Contains(logs.String(), "private=query") {
				t.Error("log entry exposed the query string")
			}
		})
	}
}

func TestRequestLoggingIncludesServeMuxErrors(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("healthy"))
	})
	handler := logRequests(logger, mux)

	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodPost, path: "/api/health", status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(test.method, test.path, nil)
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, response.Code, test.status)
		}
	}

	entries := decodeLogEntries(t, logs.String())
	if len(entries) != len(tests) {
		t.Fatalf("log entries = %d, want %d; logs = %s", len(entries), len(tests), logs.String())
	}
	for i, test := range tests {
		assertLogField(t, entries[i], "method", test.method)
		assertLogField(t, entries[i], "path", test.path)
		assertLogField(t, entries[i], "status", float64(test.status))
	}
}

func TestRequestLoggingDoesNotExposeSecretsOrPayloads(t *testing.T) {
	const (
		authorization = "Bearer authorization-secret"
		bodySecret    = "body-secret"
		responseToken = "generated-player-token"
		querySecret   = "query-secret"
	)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"player_token":"` + responseToken + `"}`))
	})
	request := httptest.NewRequest(http.MethodPost, "/api/rooms?access="+querySecret, strings.NewReader(bodySecret))
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()

	logRequests(logger, next).ServeHTTP(response, request)

	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), responseToken) {
		t.Fatalf("response changed: status = %d, body = %q", response.Code, response.Body.String())
	}
	for _, secret := range []string{authorization, "authorization-secret", bodySecret, responseToken, querySecret} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs exposed %q: %s", secret, logs.String())
		}
	}
}

func TestRouterLoggingDoesNotExposeGeneratedPlayerToken(t *testing.T) {
	const playerToken = "generated-player-token"
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := newRouterWithLogger(room.NewStore(), fixedGenerator("ABC234"), fixedGenerator(playerToken), logger)
	request := httptest.NewRequest(http.MethodPost, "/api/rooms?invite=private", nil)
	request.Header.Set("Authorization", "Bearer unrelated-secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assertJSONResponse(t, response, http.StatusCreated)
	if token := decodeCredentials(t, response).PlayerToken; token != playerToken {
		t.Fatalf("response player token = %q, want %q", token, playerToken)
	}
	entries := decodeLogEntries(t, logs.String())
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1; logs = %s", len(entries), logs.String())
	}
	assertLogField(t, entries[0], "method", http.MethodPost)
	assertLogField(t, entries[0], "path", "/api/rooms")
	assertLogField(t, entries[0], "status", float64(http.StatusCreated))
	for _, secret := range []string{playerToken, "private", "unrelated-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs exposed %q: %s", secret, logs.String())
		}
	}
}

func TestRequestLoggingPreservesPanic(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := logRequests(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler panic")
	}))

	defer func() {
		panicValue := recover()
		if panicValue != "handler panic" {
			t.Fatalf("panic value = %#v, want %q", panicValue, "handler panic")
		}
		entries := decodeLogEntries(t, logs.String())
		if len(entries) != 1 {
			t.Fatalf("log entries = %d, want 1; logs = %s", len(entries), logs.String())
		}
		assertLogField(t, entries[0], "status", float64(http.StatusInternalServerError))
		assertLogField(t, entries[0], "panicked", true)
	}()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
}

func TestRequestLoggingPreservesFlush(t *testing.T) {
	underlying := &flushResponseWriter{header: make(http.Header)}
	wrapped := &loggingResponseWriter{ResponseWriter: underlying}

	if err := http.NewResponseController(wrapped).Flush(); err != nil {
		t.Fatalf("flush through wrapped response: %v", err)
	}
	if !underlying.flushed {
		t.Error("underlying response writer was not flushed")
	}
}

type flushResponseWriter struct {
	header  http.Header
	flushed bool
}

func (w *flushResponseWriter) Header() http.Header            { return w.header }
func (w *flushResponseWriter) Write(body []byte) (int, error) { return len(body), nil }
func (w *flushResponseWriter) WriteHeader(_ int)              {}
func (w *flushResponseWriter) Flush()                         { w.flushed = true }

func decodeLogEntries(t *testing.T, logs string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	entries := make([]map[string]any, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &entries[i]); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
	}
	return entries
}

func assertLogField(t *testing.T, entry map[string]any, field string, want any) {
	t.Helper()
	if got := entry[field]; got != want {
		t.Errorf("log field %q = %#v, want %#v", field, got, want)
	}
}
