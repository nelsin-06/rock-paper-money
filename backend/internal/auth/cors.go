package auth

import (
	"net/http"
	"strings"
)

// CORS admits only the configured browser origin. It does not authenticate
// requests: session and mutation guards still run in the wrapped handler.
func (s *RequestSecurity) CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
		}
		// Origin-less reads retain same-origin/non-browser compatibility.
		if len(r.Header.Values("Origin")) == 0 && r.Method != http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if s.ValidateOrigin(r) != nil {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodOptions {
			methods := r.Header.Values("Access-Control-Request-Method")
			if len(methods) != 1 || !corsMethod(methods[0]) || !corsHeaders(r.Header.Values("Access-Control-Request-Headers")) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-CSRF-Token, Idempotency-Key")
		}
		w.Header().Set("Access-Control-Allow-Origin", s.allowedOrigin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "X-CSRF-Token, X-Request-ID")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func corsMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodPost || method == http.MethodDelete
}

func corsHeaders(values []string) bool {
	for _, value := range values {
		for _, header := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(header)) {
			case "authorization", "content-type", "x-csrf-token", "idempotency-key":
			default:
				return false
			}
		}
	}
	return true
}
