package latency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	_ pgx.QueryTracer       = (*Observer)(nil)
	_ pgxpool.AcquireTracer = (*Observer)(nil)
	_ pgxpool.ReleaseTracer = (*Observer)(nil)
)

type manualClock struct{ value time.Time }

func (c *manualClock) now() time.Time                 { return c.value }
func (c *manualClock) advance(duration time.Duration) { c.value = c.value.Add(duration) }

func TestObserverSummarizesCorrelatedOperationInMilliseconds(t *testing.T) {
	clock := &manualClock{value: time.Unix(100, 0)}
	var output bytes.Buffer
	observer := newObserver(slog.New(slog.NewJSONHandler(&output, nil)), Config{
		SlowQueryThreshold:     250 * time.Millisecond,
		SlowOperationThreshold: 2 * time.Second,
	}, clock.now)

	ctx, finish := observer.StartOperation(context.Background(), "request-123", "submit_move")
	finishAuth := StartPhase(ctx, PhaseAuthentication)
	clock.advance(20 * time.Millisecond)
	finishAuth()

	fastQuery := observer.TraceQueryStart(WithQueryName(ctx, "room_snapshot"), nil, pgx.TraceQueryStartData{SQL: "private SQL", Args: []any{"token-value"}})
	clock.advance(100 * time.Millisecond)
	observer.TraceQueryEnd(fastQuery, nil, pgx.TraceQueryEndData{})

	acquire := observer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
	clock.advance(40 * time.Millisecond)
	observer.TraceAcquireEnd(acquire, nil, pgxpool.TraceAcquireEndData{})

	finishRoomLock := StartPhase(ctx, PhaseRoomLock)
	lockQuery := observer.TraceQueryStart(WithQueryName(ctx, "room_lock_wait"), nil, pgx.TraceQueryStartData{SQL: "SELECT secret@example.com", Args: []any{"private-room"}})
	clock.advance(300 * time.Millisecond)
	observer.TraceQueryEnd(lockQuery, nil, pgx.TraceQueryEndData{})
	finishRoomLock()
	clock.advance(1700 * time.Millisecond)
	finish(204)

	if strings.Contains(output.String(), "private SQL") || strings.Contains(output.String(), "secret@example.com") || strings.Contains(output.String(), "token-value") || strings.Contains(output.String(), "private-room") {
		t.Fatalf("logs leaked SQL or arguments: %s", output.String())
	}
	entries := decodeEntries(t, output.String())
	if len(entries) != 2 {
		t.Fatalf("log entry count = %d, want one slow query and one summary: %s", len(entries), output.String())
	}
	query := entries[0]
	if query["msg"] != "database query" || query["query"] != "room_lock_wait" || query["duration_ms"] != float64(300) || query["request_id"] != "request-123" {
		t.Fatalf("slow query entry = %#v", query)
	}
	summary := entries[1]
	if summary["msg"] != "database operation" || summary["operation"] != "submit_move" || summary["status"] != float64(204) {
		t.Fatalf("summary identity = %#v", summary)
	}
	if summary["duration_ms"] != float64(2160) || summary["query_count"] != float64(2) || summary["query_duration_ms"] != float64(400) {
		t.Fatalf("summary durations/count = %#v", summary)
	}
	if summary["pool_acquire_count"] != float64(1) || summary["pool_wait_ms"] != float64(40) || summary["lock_query_duration_ms"] != float64(300) || summary["slow"] != true {
		t.Fatalf("summary database totals = %#v", summary)
	}
	phases, ok := summary["phases_ms"].(map[string]any)
	if !ok || phases[PhaseAuthentication] != float64(20) || phases[PhaseRoomLock] != float64(300) {
		t.Fatalf("summary phases = %#v", summary["phases_ms"])
	}
}

func TestObserverLogsErrorsWithoutErrorTextAndSuppressesFastSuccesses(t *testing.T) {
	clock := &manualClock{value: time.Unix(200, 0)}
	var output bytes.Buffer
	observer := newObserver(slog.New(slog.NewJSONHandler(&output, nil)), Config{
		SlowQueryThreshold:     time.Second,
		SlowOperationThreshold: time.Hour,
	}, clock.now)
	ctx, finish := observer.StartOperation(context.Background(), "request-456", "join_room")

	fast := observer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT email FROM users", Args: []any{"person@example.com"}})
	clock.advance(10 * time.Millisecond)
	observer.TraceQueryEnd(fast, nil, pgx.TraceQueryEndData{})

	failed := observer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "UPDATE sessions", Args: []any{"session-secret"}})
	clock.advance(15 * time.Millisecond)
	observer.TraceQueryEnd(failed, nil, pgx.TraceQueryEndData{Err: errors.New("failed for person@example.com with session-secret")})
	finish(500)

	if strings.Contains(output.String(), "person@example.com") || strings.Contains(output.String(), "session-secret") || strings.Contains(output.String(), "SELECT email") || strings.Contains(output.String(), "UPDATE sessions") {
		t.Fatalf("logs leaked private error, SQL, or arguments: %s", output.String())
	}
	entries := decodeEntries(t, output.String())
	if len(entries) != 2 {
		t.Fatalf("log entry count = %d, want one failed query and one summary", len(entries))
	}
	if entries[0]["error"] != true || entries[0]["slow"] != false || entries[0]["error_type"] != "*errors.errorString" {
		t.Fatalf("failed query entry = %#v", entries[0])
	}
	queryName, _ := entries[0]["query"].(string)
	if !strings.HasPrefix(queryName, "query_") || len(queryName) != len("query_")+12 {
		t.Fatalf("safe query fingerprint = %q", queryName)
	}
	if entries[1]["query_count"] != float64(2) || entries[1]["query_error_count"] != float64(1) {
		t.Fatalf("summary query counts = %#v", entries[1])
	}
}

func TestObserverTreatsNoRowsAsACompletedQueryNotADatabaseFailure(t *testing.T) {
	clock := &manualClock{value: time.Unix(250, 0)}
	var output bytes.Buffer
	observer := newObserver(slog.New(slog.NewJSONHandler(&output, nil)), Config{
		SlowQueryThreshold:     time.Second,
		SlowOperationThreshold: time.Hour,
	}, clock.now)
	ctx, finish := observer.StartOperation(context.Background(), "request-no-rows", "join_room")

	query := observer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT receipt"})
	clock.advance(10 * time.Millisecond)
	observer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{Err: pgx.ErrNoRows})
	finish(404)

	entries := decodeEntries(t, output.String())
	if len(entries) != 1 || entries[0]["msg"] != "database operation" {
		t.Fatalf("entries = %#v, want only operation summary", entries)
	}
	if entries[0]["query_count"] != float64(1) || entries[0]["query_error_count"] != float64(0) {
		t.Fatalf("summary = %#v", entries[0])
	}
}

func TestObserverRecordsSlowAcquireWithOperationContext(t *testing.T) {
	clock := &manualClock{value: time.Unix(300, 0)}
	var output bytes.Buffer
	observer := newObserver(slog.New(slog.NewJSONHandler(&output, nil)), Config{
		SlowQueryThreshold:     50 * time.Millisecond,
		SlowOperationThreshold: time.Second,
	}, clock.now)
	ctx, finish := observer.StartOperation(context.Background(), "request-789", "create_room")

	acquire := observer.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
	clock.advance(75 * time.Millisecond)
	observer.TraceAcquireEnd(acquire, nil, pgxpool.TraceAcquireEndData{})
	finish(201)

	entries := decodeEntries(t, output.String())
	if len(entries) != 2 || entries[0]["msg"] != "database pool acquire" {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0]["duration_ms"] != float64(75) || entries[0]["operation"] != "create_room" || entries[0]["request_id"] != "request-789" {
		t.Fatalf("acquire entry = %#v", entries[0])
	}
}

func TestClassifyQueryUsesSafeStableLockNames(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{name: "advisory lock", sql: "select pg_advisory_xact_lock($1)", want: "advisory_lock_wait"},
		{name: "table lock", sql: "LOCK TABLE room_rooms IN ACCESS EXCLUSIVE MODE", want: "table_lock_wait"},
		{name: "row lock", sql: "SELECT revision FROM room_rooms FOR UPDATE", want: "row_lock_statement"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyQuery(tt.sql); got != tt.want {
				t.Fatalf("classifyQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func decodeEntries(t *testing.T, output string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log entry %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}
