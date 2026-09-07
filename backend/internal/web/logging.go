package web

import (
	"log/slog"
	"net/http"
	"net/netip"
	"time"
)

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &loggingResponseWriter{ResponseWriter: w}

		defer func() {
			if panicValue := recover(); panicValue != nil {
				logger.Info("http request",
					"method", r.Method,
					"path", r.URL.Path,
					"status", response.statusOr(http.StatusInternalServerError),
					"duration", time.Since(started),
					"client_ip", normalizedClientIP(r.RemoteAddr),
					"panicked", true,
				)
				panic(panicValue)
			}

			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", response.statusOr(http.StatusOK),
				"duration", time.Since(started),
				"client_ip", normalizedClientIP(r.RemoteAddr),
			)
		}()

		next.ServeHTTP(response, r)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *loggingResponseWriter) statusOr(fallback int) int {
	if w.status == 0 {
		return fallback
	}
	return w.status
}

func normalizedClientIP(remoteAddress string) string {
	if addressPort, err := netip.ParseAddrPort(remoteAddress); err == nil {
		return addressPort.Addr().Unmap().WithZone("").String()
	}
	if address, err := netip.ParseAddr(remoteAddress); err == nil {
		return address.Unmap().WithZone("").String()
	}
	return "unknown"
}
