package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/rock-paper-money/internal/auth"
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
	logPath := os.Getenv("LOG_FILE")
	if logPath == "" {
		logPath = "server.log"
	}
	logger, logFile, err := openLogger(logPath, os.Stderr)
	if err != nil {
		return err
	}
	defer logFile.Close()
	slog.SetDefault(logger)

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	supabaseURL := os.Getenv("SUPABASE_URL")
	if supabaseURL == "" {
		return errors.New("SUPABASE_URL is required")
	}
	audience := os.Getenv("SUPABASE_JWT_AUDIENCE")
	verifier, err := auth.NewSupabaseJWTVerifier(supabaseURL, audience, nil)
	if err != nil {
		return fmt.Errorf("configure Supabase authentication: %w", err)
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
	events := postgres.NewEvents(pool, logger)
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
		Handler:           roomhttp.NewRouter(rooms, verifier, ready, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("server listening", "address", server.Addr)
	return serveHTTP(root, server, server.ListenAndServe)
}

func openLogger(path string, console io.Writer) (*slog.Logger, *os.File, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %q: %w", path, err)
	}
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(console, file), nil))
	return logger, file, nil
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
