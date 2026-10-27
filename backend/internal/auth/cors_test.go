package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSBoundariesAndErrorResponses(t *testing.T) {
	security, err := NewRequestSecurity("https://game.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, path, origin, method, requestedMethod, headers string
		want, calls                                        int
		cors                                               bool
	}{
		{"preflight no auth", "/api/rooms", "https://game.example", "OPTIONS", "POST", "authorization, CONTENT-TYPE, X-CSRF-Token, Idempotency-Key", 204, 0, true},
		{"hostile origin", "/api/session", "https://evil.example", "POST", "", "", 403, 0, false},
		{"duplicate origin", "/api/session", "duplicate", "OPTIONS", "POST", "authorization", 403, 0, false},
		{"missing origin preflight", "/api/session", "", "OPTIONS", "POST", "authorization", 403, 0, false},
		{"missing method", "/api/session", "https://game.example", "OPTIONS", "", "authorization", 403, 0, false},
		{"invalid method", "/api/session", "https://game.example", "OPTIONS", "PATCH", "authorization", 403, 0, false},
		{"invalid header", "/api/rooms", "https://game.example", "OPTIONS", "POST", "content-type, cookie", 403, 0, false},
		{"error retains CORS", "/api/rooms", "https://game.example", "POST", "", "", 401, 1, true},
		{"originless read", "/api/rooms", "", "GET", "", "", 401, 1, false},
		{"outside API", "/metrics", "https://evil.example", "OPTIONS", "POST", "cookie", 401, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			handler := security.CORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			}))
			r := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.origin == "duplicate" {
				r.Header["Origin"] = []string{"https://game.example", "https://game.example"}
			}
			r.Header.Set("Access-Control-Request-Method", tt.requestedMethod)
			if tt.headers != "" {
				r.Header.Set("Access-Control-Request-Headers", tt.headers)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.want || calls != tt.calls || len(w.Result().Cookies()) != 0 {
				t.Fatalf("status=%d calls=%d cookies=%v", w.Code, calls, w.Result().Cookies())
			}
			if tt.cors {
				if w.Header().Get("Access-Control-Allow-Origin") != "https://game.example" || w.Header().Get("Access-Control-Allow-Credentials") != "true" || w.Header().Get("Access-Control-Expose-Headers") != "X-CSRF-Token, X-Request-ID" {
					t.Fatalf("CORS headers: %v", w.Header())
				}
			} else if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("unexpected CORS: %v", w.Header())
			}
			if strings.HasPrefix(tt.path, "/api/") {
				vary := strings.Join(w.Header().Values("Vary"), ", ")
				if !strings.Contains(vary, "Origin") || (tt.method == "OPTIONS" && (!strings.Contains(vary, "Access-Control-Request-Method") || !strings.Contains(vary, "Access-Control-Request-Headers"))) {
					t.Fatalf("Vary=%q", vary)
				}
			}
			wantBody := "Forbidden\n"
			if tt.want == 204 {
				wantBody = ""
			}
			if tt.want == 401 {
				wantBody = "Unauthorized\n"
			}
			if w.Body.String() != wantBody {
				t.Fatalf("body=%q", w.Body.String())
			}
		})
	}
}
