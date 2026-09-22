package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenLoggerWritesStructuredEntriesToConsoleAndFile(t *testing.T) {
	var console bytes.Buffer
	path := filepath.Join(t.TempDir(), "server.log")
	logger, file, err := openLogger(path, &console)
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("request failed", "request_id", "request-123", "status", 500)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"console": console.String(), "file": string(contents)} {
		if !strings.Contains(output, `"level":"ERROR"`) || !strings.Contains(output, `"request_id":"request-123"`) || !strings.Contains(output, `"status":500`) {
			t.Fatalf("%s output = %s", name, output)
		}
	}
}

func TestOpenLoggerFailsWhenRequestedPathCannotBeOpened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "server.log")
	if _, _, err := openLogger(path, &bytes.Buffer{}); err == nil {
		t.Fatal("openLogger() error = nil, want path error")
	}
}

func TestServeHTTPReportsUnexpectedRuntimeError(t *testing.T) {
	want := errors.New("bind failed")
	err := serveHTTPWithShutdown(context.Background(), func() error { return want }, func(context.Context) error { return nil })
	if !errors.Is(err, want) {
		t.Fatalf("serveHTTPWithShutdown() error = %v, want %v", err, want)
	}
}

func TestServeHTTPTreatsServerClosedAsSuccess(t *testing.T) {
	err := serveHTTPWithShutdown(context.Background(), func() error { return http.ErrServerClosed }, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("serveHTTPWithShutdown() error = %v, want nil", err)
	}
}

func TestServeHTTPGracefulShutdownIsSuccessful(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serverClosed := make(chan struct{})
	listen := func() error {
		<-serverClosed
		return http.ErrServerClosed
	}
	shutdown := func(context.Context) error {
		close(serverClosed)
		return nil
	}
	cancel()
	if err := serveHTTPWithShutdown(ctx, listen, shutdown); err != nil {
		t.Fatalf("serveHTTPWithShutdown() error = %v, want nil", err)
	}
}

func TestHTTPServerAllowsDatabaseBackedCommandsPastPreviousWriteDeadline(t *testing.T) {
	server := newHTTPServer("9090", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if server.WriteTimeout != 30*time.Second {
		t.Fatalf("WriteTimeout = %v, want 30s", server.WriteTimeout)
	}
	if server.WriteTimeout <= 10*time.Second {
		t.Fatalf("WriteTimeout = %v, must exceed the previous 10s deadline", server.WriteTimeout)
	}
}
