package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	roomhttp "example.com/rock-paper-money/internal/room/adapter/http"
	"example.com/rock-paper-money/internal/room/adapter/postgres"
	"example.com/rock-paper-money/internal/room/application"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	root, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(root, databaseURL)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	defer pool.Close()
	startup, cancelStartup := context.WithTimeout(root, 15*time.Second)
	defer cancelStartup()
	if err := pool.Ping(startup); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	if err := postgres.Migrate(startup, pool); err != nil {
		return fmt.Errorf("apply database migrations: %w", err)
	}
	cancelStartup()
	repository := postgres.NewRepository(pool)
	events := postgres.NewEvents(pool, slog.Default())
	if err := events.Start(root); err != nil {
		return fmt.Errorf("start database notification listener: %w", err)
	}
	rooms := application.NewService(repository, events)
	ready := func(ctx context.Context) error {
		if err := repository.Ping(ctx); err != nil {
			return err
		}
		return events.Ready()
	}
	server := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           roomhttp.NewRouter(rooms, ready),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("server listening on %s", server.Addr)
	return serveHTTP(root, server, server.ListenAndServe)
}

func serveHTTP(ctx context.Context, server *http.Server, listen func() error) error {
	return serveHTTPWithShutdown(ctx, listen, server.Shutdown)
}

func serveHTTPWithShutdown(ctx context.Context, listen func() error, shutdown func(context.Context) error) error {
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- listen() }()
	select {
	case <-ctx.Done():
	case err := <-serveErrors:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	select {
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP during shutdown: %w", err)
		}
		return nil
	case <-shutdownContext.Done():
		return fmt.Errorf("wait for HTTP server shutdown: %w", shutdownContext.Err())
	}
}
