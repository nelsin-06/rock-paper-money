package roomhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
)

type requestIDContextKey struct{}

type internalFailure struct {
	cause     error
	operation string
	kind      string
	phase     string
}

var fallbackRequestID uint64

func observeRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := newRequestID()
		request := r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID))
		response := &loggingResponseWriter{ResponseWriter: w}
		response.Header().Set("X-Request-ID", requestID)

		defer func() {
			if panicValue := recover(); panicValue != nil {
				if response.status == 0 {
					response.failure = internalFailure{
						cause:     panicCause(panicValue),
						operation: "serve_http",
						kind:      "panic",
						phase:     "before_commit",
					}
					writeInternalErrorResponse(response, request)
					logInternalFailure(logger, request, response.failure)
					return
				} else {
					logInternalFailure(logger, request, internalFailure{
						cause:     panicCause(panicValue),
						operation: "serve_http",
						kind:      "panic",
						phase:     "after_commit",
					})
					panic(http.ErrAbortHandler)
				}
			}

			if response.statusOr(http.StatusOK) == http.StatusInternalServerError {
				failure := response.failure
				if failure.cause == nil {
					failure = internalFailure{
						cause:     fmt.Errorf("handler returned status %d without a classified cause", http.StatusInternalServerError),
						operation: "serve_http",
						kind:      "internal_error",
						phase:     "before_commit",
					}
				}
				logInternalFailure(logger, request, failure)
				return
			}
			logRequestCompletion(logger, request, response, started)
		}()

		next.ServeHTTP(response, request)
	})
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	sequence := atomic.AddUint64(&fallbackRequestID, 1)
	return fmt.Sprintf("fallback-%d-%d", time.Now().UTC().UnixNano(), sequence)
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status  int
	failure internalFailure
}

func (w *loggingResponseWriter) recordInternalFailure(failure internalFailure) {
	w.failure = failure
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

func (w *loggingResponseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
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

func logInternalFailure(logger *slog.Logger, r *http.Request, failure internalFailure) {
	logger.Error("http internal failure",
		"request_id", requestIDFromContext(r.Context()),
		"method", r.Method,
		"path", r.URL.Path,
		"status", http.StatusInternalServerError,
		"operation", failure.operation,
		"kind", failure.kind,
		"phase", failure.phase,
		"error_type", fmt.Sprintf("%T", failure.cause),
		"error", boundedError(failure.cause),
	)
}

func logRequestCompletion(logger *slog.Logger, r *http.Request, response *loggingResponseWriter, started time.Time) {
	logger.Info("http request",
		"request_id", requestIDFromContext(r.Context()),
		"method", r.Method,
		"path", r.URL.Path,
		"status", response.statusOr(http.StatusOK),
		"duration", time.Since(started),
		"client_ip", normalizedClientIP(r.RemoteAddr),
	)
}

func panicCause(value any) error {
	if err, ok := value.(error); ok {
		return err
	}
	return fmt.Errorf("%v", value)
}

func boundedError(err error) string {
	if err == nil {
		return "unknown internal error"
	}
	message := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(err.Error())
	const maxRunes = 512
	runes := []rune(message)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return message
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
