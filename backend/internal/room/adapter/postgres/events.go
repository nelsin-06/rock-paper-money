package postgres

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrListenerUnavailable = errors.New("room notification listener is unavailable")

type Events struct {
	pool        *pgxpool.Pool
	logger      *slog.Logger
	mu          sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
	ready       atomic.Bool
}

func NewEvents(pool *pgxpool.Pool, logger *slog.Logger) *Events {
	return &Events{pool: pool, logger: logger, subscribers: map[string]map[chan struct{}]struct{}{}}
}
func (e *Events) Subscribe(code string) (<-chan struct{}, func()) {
	e.mu.Lock()
	ch := make(chan struct{}, 1)
	if e.subscribers[code] == nil {
		e.subscribers[code] = map[chan struct{}]struct{}{}
	}
	e.subscribers[code][ch] = struct{}{}
	e.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { e.mu.Lock(); delete(e.subscribers[code], ch); e.mu.Unlock() }) }
}

// Start establishes LISTEN before returning, then maintains it in the background.
func (e *Events) Start(ctx context.Context) error {
	conn, err := e.listen(ctx)
	if err != nil {
		return err
	}
	e.ready.Store(true)
	go e.run(ctx, conn)
	return nil
}

func (e *Events) Run(ctx context.Context) {
	conn, err := e.listen(ctx)
	if err != nil {
		return
	}
	e.ready.Store(true)
	e.run(ctx, conn)
}

func (e *Events) run(ctx context.Context, conn *pgxpool.Conn) {
	defer e.ready.Store(false)
	backoff := 100 * time.Millisecond
	for {
		backoff = 100 * time.Millisecond
		for ctx.Err() == nil {
			notification, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				e.ready.Store(false)
				conn.Release()
				e.logger.Warn("room notification listener reconnecting", "error", err)
				break
			}
			code, _, ok := strings.Cut(notification.Payload, ":")
			if ok {
				e.publish(code)
			}
		}
		if ctx.Err() != nil {
			return
		}
		for ctx.Err() == nil {
			var err error
			conn, err = e.listen(ctx)
			if err == nil {
				e.ready.Store(true)
				e.publishAll()
				break
			}
			if !sleep(ctx, backoff) {
				return
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
		}
	}
}

func (e *Events) Ready() error {
	if !e.ready.Load() {
		return ErrListenerUnavailable
	}
	return nil
}

func (e *Events) listen(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = conn.Exec(ctx, "LISTEN room_changes"); err != nil {
		conn.Release()
		return nil, err
	}
	return conn, nil
}
func (e *Events) publish(code string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subscribers[code] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (e *Events) publishAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, subscribers := range e.subscribers {
		for ch := range subscribers {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
