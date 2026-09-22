package latency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	PhaseAuthentication              = "authentication"
	PhasePoolBeginTransaction        = "pool_begin_transaction"
	PhaseIdempotencyLockReceipt      = "idempotency_lock_receipt"
	PhaseRoomLock                    = "room_lock"
	PhaseAggregateLoad               = "aggregate_load"
	PhaseDomainMutation              = "domain_mutation"
	PhasePersistenceWalletSettlement = "persistence_wallet_settlement"
	PhaseOutboxReceipt               = "outbox_receipt"
	PhaseCommit                      = "commit"
)

var commandPhases = []string{
	PhaseAuthentication,
	PhasePoolBeginTransaction,
	PhaseIdempotencyLockReceipt,
	PhaseRoomLock,
	PhaseAggregateLoad,
	PhaseDomainMutation,
	PhasePersistenceWalletSettlement,
	PhaseOutboxReceipt,
	PhaseCommit,
}

type clockFunc func() time.Time

type Observer struct {
	logger *slog.Logger
	config Config
	now    clockFunc
	mu     sync.RWMutex
	pool   *pgxpool.Pool
}

type operationContextKey struct{}
type queryNameContextKey struct{}
type queryTraceContextKey struct{}
type acquireTraceContextKey struct{}

type operation struct {
	mu            sync.Mutex
	requestID     string
	name          string
	started       time.Time
	now           clockFunc
	queryCount    int
	queryDuration time.Duration
	queryErrors   int
	acquireCount  int
	acquireWait   time.Duration
	acquireErrors int
	lockDuration  time.Duration
	phases        map[string]time.Duration
}

type queryTrace struct {
	started   time.Time
	name      string
	operation *operation
}

type acquireTrace struct {
	started   time.Time
	operation *operation
}

func NewObserver(logger *slog.Logger, config Config) *Observer {
	return newObserver(logger, config, time.Now)
}

func newObserver(logger *slog.Logger, config Config, now clockFunc) *Observer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Observer{logger: logger, config: config, now: now}
}

func (o *Observer) BindPool(pool *pgxpool.Pool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pool = pool
}

func (o *Observer) StartOperation(ctx context.Context, requestID, name string) (context.Context, func(int)) {
	op := &operation{requestID: requestID, name: name, started: o.now(), now: o.now, phases: make(map[string]time.Duration, len(commandPhases))}
	for _, phase := range commandPhases {
		op.phases[phase] = 0
	}
	ctx = context.WithValue(ctx, operationContextKey{}, op)
	return ctx, func(status int) { o.finishOperation(op, status) }
}

func (o *Observer) StartPhase(ctx context.Context, name string) func() {
	return StartPhase(ctx, name)
}

func StartPhase(ctx context.Context, name string) func() {
	op := operationFromContext(ctx)
	if op == nil {
		return func() {}
	}
	started := op.now()
	var once sync.Once
	return func() {
		once.Do(func() {
			op.mu.Lock()
			op.phases[name] += op.now().Sub(started)
			op.mu.Unlock()
		})
	}
}

func WithQueryName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, queryNameContextKey{}, name)
}

func (o *Observer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	name, _ := ctx.Value(queryNameContextKey{}).(string)
	if name == "" {
		name = classifyQuery(data.SQL)
	}
	return context.WithValue(ctx, queryTraceContextKey{}, queryTrace{started: o.now(), name: name, operation: operationFromContext(ctx)})
}

func (o *Observer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	trace, ok := ctx.Value(queryTraceContextKey{}).(queryTrace)
	if !ok {
		return
	}
	duration := o.now().Sub(trace.started)
	failed := data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows)
	requestID, operationName := "", ""
	if trace.operation != nil {
		trace.operation.mu.Lock()
		trace.operation.queryCount++
		trace.operation.queryDuration += duration
		if failed {
			trace.operation.queryErrors++
		}
		if isLockQuery(trace.name) {
			trace.operation.lockDuration += duration
		}
		requestID, operationName = trace.operation.requestID, trace.operation.name
		trace.operation.mu.Unlock()
	}
	if duration < o.config.SlowQueryThreshold && !failed {
		return
	}
	attrs := []any{
		"request_id", requestID,
		"operation", operationName,
		"query", trace.name,
		"duration_ms", milliseconds(duration),
		"slow", duration >= o.config.SlowQueryThreshold,
		"error", failed,
	}
	if failed {
		attrs = append(attrs, safeErrorAttrs(data.Err)...)
	}
	o.logger.Warn("database query", attrs...)
}

func (o *Observer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, acquireTraceContextKey{}, acquireTrace{started: o.now(), operation: operationFromContext(ctx)})
}

func (o *Observer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	trace, ok := ctx.Value(acquireTraceContextKey{}).(acquireTrace)
	if !ok {
		return
	}
	duration := o.now().Sub(trace.started)
	requestID, operationName := "", ""
	if trace.operation != nil {
		trace.operation.mu.Lock()
		trace.operation.acquireCount++
		trace.operation.acquireWait += duration
		if data.Err != nil {
			trace.operation.acquireErrors++
		}
		requestID, operationName = trace.operation.requestID, trace.operation.name
		trace.operation.mu.Unlock()
	}
	if duration < o.config.SlowQueryThreshold && data.Err == nil {
		return
	}
	attrs := []any{
		"request_id", requestID,
		"operation", operationName,
		"duration_ms", milliseconds(duration),
		"slow", duration >= o.config.SlowQueryThreshold,
		"error", data.Err != nil,
	}
	attrs = append(attrs, safeErrorAttrs(data.Err)...)
	o.logger.Warn("database pool acquire", attrs...)
}

func (o *Observer) TraceRelease(_ *pgxpool.Pool, _ pgxpool.TraceReleaseData) {}

func (o *Observer) finishOperation(op *operation, status int) {
	duration := o.now().Sub(op.started)
	op.mu.Lock()
	phases := make(map[string]float64, len(op.phases))
	for name, value := range op.phases {
		phases[name] = milliseconds(value)
	}
	requestID := op.requestID
	name := op.name
	queryCount := op.queryCount
	queryDuration := op.queryDuration
	queryErrors := op.queryErrors
	acquireCount := op.acquireCount
	acquireWait := op.acquireWait
	acquireErrors := op.acquireErrors
	lockDuration := op.lockDuration
	op.mu.Unlock()

	attrs := []any{
		"request_id", requestID,
		"operation", name,
		"status", status,
		"duration_ms", milliseconds(duration),
		"slow", duration >= o.config.SlowOperationThreshold,
		"query_count", queryCount,
		"query_duration_ms", milliseconds(queryDuration),
		"query_error_count", queryErrors,
		"pool_acquire_count", acquireCount,
		"pool_wait_ms", milliseconds(acquireWait),
		"pool_acquire_error_count", acquireErrors,
		"lock_query_duration_ms", milliseconds(lockDuration),
		"phases_ms", phases,
	}
	if stat := o.poolStat(); stat != nil {
		attrs = append(attrs,
			"pool_acquired_conns", stat.AcquiredConns(),
			"pool_idle_conns", stat.IdleConns(),
			"pool_total_conns", stat.TotalConns(),
			"pool_max_conns", stat.MaxConns(),
			"pool_empty_acquire_count", stat.EmptyAcquireCount(),
			"pool_canceled_acquire_count", stat.CanceledAcquireCount(),
		)
	}
	o.logger.Info("database operation", attrs...)
}

func (o *Observer) poolStat() *pgxpool.Stat {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.pool == nil {
		return nil
	}
	return o.pool.Stat()
}

func operationFromContext(ctx context.Context) *operation {
	op, _ := ctx.Value(operationContextKey{}).(*operation)
	return op
}

func queryFingerprint(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return "query_" + hex.EncodeToString(sum[:6])
}

func classifyQuery(sql string) string {
	normalized := strings.ToUpper(sql)
	switch {
	case strings.Contains(normalized, "PG_ADVISORY_XACT_LOCK"):
		return "advisory_lock_wait"
	case strings.Contains(normalized, "LOCK TABLE"):
		return "table_lock_wait"
	case strings.Contains(normalized, "FOR UPDATE"):
		return "row_lock_statement"
	default:
		return queryFingerprint(sql)
	}
}

func isLockQuery(name string) bool {
	switch name {
	case "idempotency_lock_wait", "room_lock_wait", "wallet_lock_wait", "advisory_lock_wait", "table_lock_wait", "row_lock_statement":
		return true
	default:
		return false
	}
}

func safeErrorAttrs(err error) []any {
	if err == nil {
		return nil
	}
	attrs := []any{"error_type", fmt.Sprintf("%T", err)}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		attrs = append(attrs, "postgres_code", pgErr.Code)
	}
	return attrs
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
