package roomhttp

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestLoggingOmitsSecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := logRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"player_token":"response-secret"}`))
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/rooms?secret=query-secret", strings.NewReader("body-secret"))
	request.Header.Set("Authorization", "Bearer header-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	for _, secret := range []string{"query-secret", "body-secret", "header-secret", "response-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs exposed %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `"status":201`) {
		t.Fatalf("status missing: %s", logs.String())
	}
}
