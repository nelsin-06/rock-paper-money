package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

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
