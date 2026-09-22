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
	"example.com/rock-paper-money/internal/latency"
	"example.com/rock-paper-money/internal/operations"
	"example.com/rock-paper-money/internal/realtime"
	roomhttp "example.com/rock-paper-money/internal/room/adapter/http"
	"example.com/rock-paper-money/internal/room/adapter/postgres"
	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
	redis "github.com/redis/go-redis/v9"
)

const (
	serverReadTimeout  = 10 * time.Second
	serverWriteTimeout = 30 * time.Second
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
	latencyConfig, err := latency.ConfigFromEnv(os.Getenv)
	if err != nil {
		return fmt.Errorf("configure latency observability: %w", err)
	}
	latencyObserver := latency.NewObserver(logger, latencyConfig)

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
	allowedOrigin := os.Getenv("APP_ORIGIN")
	if allowedOrigin == "" {
		return errors.New("APP_ORIGIN is required")
	}
	redisAddress := os.Getenv("REDIS_ADDR")
	if redisAddress == "" {
		return errors.New("REDIS_ADDR is required")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	root, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	poolConfig.ConnConfig.Tracer = latencyObserver
	pool, err := pgxpool.NewWithConfig(root, poolConfig)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	latencyObserver.BindPool(pool)
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
	sessions := auth.NewSessionService(auth.NewPostgresSessionStore(pool), nil, nil)
	security, err := auth.NewRequestSecurity(allowedOrigin, sessions)
	if err != nil {
		return fmt.Errorf("configure request security: %w", err)
	}
	redisClient := redis.NewClient(&redis.Options{Addr: redisAddress})
	defer redisClient.Close()
	transport := realtime.NewRedisTransport(redisClient, time.Second)
	metrics := operations.NewMetrics()
	transport.SetObserver(metrics)
	hub := realtime.NewHub(transport, repository)
	hub.SetObserver(metrics)
	sockets := realtime.NewWebSocketHandler(security, sessions, repository, hub, realtime.WebSocketConfig{Observer: metrics, LifecycleContext: root})
	rooms := application.NewService(repository, nil)
	ready := func(ctx context.Context) (roomhttp.ReadinessStatus, int) {
		if err := repository.Ping(ctx); err != nil {
			return roomhttp.ReadinessStatus{Status: "unavailable", PostgreSQL: "unavailable", Realtime: "unknown"}, http.StatusServiceUnavailable
		}
		if err := transport.CheckReady(ctx); err != nil {
			return roomhttp.ReadinessStatus{Status: "degraded", PostgreSQL: "authoritative", Realtime: "redis_unavailable"}, http.StatusServiceUnavailable
		}
		return roomhttp.ReadinessStatus{Status: "ok", PostgreSQL: "authoritative", Realtime: "ready"}, http.StatusOK
	}
	runtimeContext, stopRuntime := context.WithCancel(context.Background())
	runtime := operations.NewRuntime(
		worker.NewDeadlineWorker(repository, time.Now),
		worker.NewOutboxWorker(repository, transport, worker.OutboxConfig{}),
		worker.NewCleanupWorker(sessions, repository, time.Now),
		metrics,
		logger,
		operations.Intervals{},
	)
	runtime.SetReconciliation(worker.NewReconciliationWorker(repository, postgres.NewRoomStateListener(pool), worker.ReconciliationConfig{}))
	runtime.Start(runtimeContext)
	defer func() {
		stopRuntime()
		runtime.Wait()
	}()
	server := newHTTPServer(port, roomhttp.NewOperationalRouter(rooms, verifier, sessions, security, ready, logger, sockets, metrics, latencyObserver))

	logger.Info("server listening", "address", server.Addr)
	serveErr := serveHTTP(root, server, server.ListenAndServe)
	socketShutdownContext, cancelSocketShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSocketShutdown()
	if err := sockets.Shutdown(socketShutdownContext); err != nil {
		return errors.Join(serveErr, fmt.Errorf("shut down WebSockets: %w", err))
	}
	return serveErr
}

func newHTTPServer(port string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       60 * time.Second,
	}
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
