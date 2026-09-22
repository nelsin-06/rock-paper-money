package operations

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"example.com/rock-paper-money/internal/worker"
)

// Metrics is a dependency-free Prometheus collector for the bounded operational
// signals used by the realtime runtime. Labels are fixed by code so secrets,
// room payloads, moves, cookies, and tokens can never become metric dimensions.
type Metrics struct {
	activeSockets atomic.Int64
	mu            sync.Mutex
	counters      map[string]uint64
	gauges        map[string]float64
}

func NewMetrics() *Metrics {
	return &Metrics{counters: make(map[string]uint64), gauges: make(map[string]float64)}
}

func (m *Metrics) SocketOpened() { m.activeSockets.Add(1); m.increment("rpm_socket_open_total") }
func (m *Metrics) SocketClosed(reason string) {
	m.activeSockets.Add(-1)
	m.increment("rpm_socket_close_total{reason=\"" + safeLabel(reason) + "\"}")
}
func (m *Metrics) SnapshotDelivered(latency time.Duration) {
	m.increment("rpm_snapshot_delivered_total")
	m.set("rpm_snapshot_latency_seconds", latency.Seconds())
}
func (m *Metrics) RevisionGap()    { m.increment("rpm_revision_gap_total") }
func (m *Metrics) QueueEviction()  { m.increment("rpm_backpressure_eviction_total") }
func (m *Metrics) AuthClose()      { m.increment("rpm_auth_close_total") }
func (m *Metrics) RedisReconnect() { m.increment("rpm_redis_reconnect_total") }
func (m *Metrics) RedisFailure()   { m.increment("rpm_redis_failure_total") }
func (m *Metrics) Receipt(outcome string) {
	m.increment("rpm_receipt_total{outcome=\"" + safeLabel(outcome) + "\"}")
}
func (m *Metrics) Deadline(processed, settled, noops, stale int64, lag time.Duration) {
	m.add("rpm_deadline_processed_total", uint64(max(processed, 0)))
	m.add("rpm_deadline_settled_total", uint64(max(settled, 0)))
	m.add("rpm_deadline_noop_total", uint64(max(noops, 0)))
	m.add("rpm_deadline_stale_claim_total", uint64(max(stale, 0)))
	m.set("rpm_deadline_lag_seconds", max(lag.Seconds(), 0))
}
func (m *Metrics) Outbox(report worker.OutboxRunReport) {
	m.add("rpm_outbox_claimed_total", uint64(max(report.Claimed, 0)))
	m.add("rpm_outbox_published_total", uint64(max(report.Published, 0)))
	m.add("rpm_outbox_attempt_total", uint64(max(report.Attempts, 0)))
	m.add("rpm_outbox_retry_total", uint64(max(report.Retries, 0)))
	m.add("rpm_outbox_publish_failure_total", uint64(max(report.PublishFailures, 0)))
	m.set("rpm_outbox_backlog_depth", float64(max(report.BacklogDepth, 0)))
	m.set("rpm_outbox_oldest_pending_age_seconds", max(report.OldestPendingAge.Seconds(), 0))
}
func (m *Metrics) Cleanup(kind string, removed int64) {
	m.add("rpm_cleanup_removed_total{kind=\""+safeLabel(kind)+"\"}", uint64(max(removed, 0)))
}

func (m *Metrics) increment(name string) { m.add(name, 1) }
func (m *Metrics) add(name string, value uint64) {
	m.mu.Lock()
	m.counters[name] += value
	m.mu.Unlock()
}
func (m *Metrics) set(name string, value float64) {
	m.mu.Lock()
	m.gauges[name] = value
	m.mu.Unlock()
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	m.mu.Lock()
	values := make(map[string]string, len(m.counters)+len(m.gauges)+1)
	values["rpm_active_sockets"] = fmt.Sprintf("%d", m.activeSockets.Load())
	for name, value := range m.counters {
		values[name] = fmt.Sprintf("%d", value)
	}
	for name, value := range m.gauges {
		values[name] = fmt.Sprintf("%g", value)
	}
	m.mu.Unlock()
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_, _ = fmt.Fprintf(w, "%s %s\n", name, values[name])
	}
}

func safeLabel(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			result.WriteRune(character)
		}
	}
	if result.Len() == 0 {
		return "unknown"
	}
	return result.String()
}
