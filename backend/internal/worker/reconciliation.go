package worker

import (
	"context"
	"time"
)

type ReconciliationSource string

const (
	ReconciliationStartup      ReconciliationSource = "startup"
	ReconciliationReconnect    ReconciliationSource = "reconnect"
	ReconciliationNotification ReconciliationSource = "notification"
	ReconciliationPeriodic     ReconciliationSource = "periodic"
)

type RoomHint struct {
	RoomCode string
	Round    uint64
}

type ReconciliationReport struct {
	ClaimAttempts     int64
	ClaimWins         int64
	ClaimNoops        int64
	SettledRounds     int64
	AdvancedRounds    int64
	Failures          int64
	EligibleBacklog   int64
	OldestEligibleAge time.Duration
	Duration          time.Duration
}

func (r *ReconciliationReport) Add(other ReconciliationReport) {
	r.ClaimAttempts += other.ClaimAttempts
	r.ClaimWins += other.ClaimWins
	r.ClaimNoops += other.ClaimNoops
	r.SettledRounds += other.SettledRounds
	r.AdvancedRounds += other.AdvancedRounds
	r.Failures += other.Failures
	if other.EligibleBacklog > r.EligibleBacklog {
		r.EligibleBacklog = other.EligibleBacklog
	}
	if other.OldestEligibleAge > r.OldestEligibleAge {
		r.OldestEligibleAge = other.OldestEligibleAge
	}
}

type ReconciliationStore interface {
	ReconcileRoom(context.Context, RoomHint) (ReconciliationReport, error)
	ReconcileBatch(context.Context, int) (ReconciliationReport, error)
}

// RoomStateListener owns one PostgreSQL connection for LISTEN. Listen returns
// when that connection fails or the context is cancelled; callers reconnect.
type RoomStateListener interface {
	Listen(context.Context, func(), func(RoomHint)) error
}

type ReconciliationConfig struct {
	BatchSize        int
	MaxDrainBatches  int
	PeriodicInterval time.Duration
	IdleInterval     time.Duration
	ReconnectDelay   time.Duration
}

type ReconciliationWorker struct {
	store    ReconciliationStore
	listener RoomStateListener
	config   ReconciliationConfig
}

func NewReconciliationWorker(store ReconciliationStore, listener RoomStateListener, config ReconciliationConfig) *ReconciliationWorker {
	if config.BatchSize <= 0 {
		config.BatchSize = 32
	}
	if config.MaxDrainBatches <= 0 {
		config.MaxDrainBatches = 8
	}
	if config.PeriodicInterval <= 0 {
		config.PeriodicInterval = time.Second
	}
	if config.IdleInterval < config.PeriodicInterval {
		config.IdleInterval = 5 * time.Second
	}
	if config.ReconnectDelay <= 0 {
		config.ReconnectDelay = time.Second
	}
	return &ReconciliationWorker{store: store, listener: listener, config: config}
}

// Run combines lossy notification wakeups with bounded durable scans. The
// callback receives only fixed sources and aggregate operational values.
func (w *ReconciliationWorker) Run(ctx context.Context, observe func(ReconciliationSource, ReconciliationReport, error)) {
	notifications := make(chan RoomHint, w.config.BatchSize)
	connections := make(chan ReconciliationSource, 1)
	if w.listener != nil {
		go w.listen(ctx, connections, notifications, observe)
	} else {
		connections <- ReconciliationStartup
	}
	timer := time.NewTimer(w.config.PeriodicInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case source := <-connections:
			started := time.Now()
			report, err := w.drain(ctx)
			report.Duration = time.Since(started)
			observe(source, report, err)
			resetTimer(timer, w.nextInterval(report))
		case hint := <-notifications:
			started := time.Now()
			report, err := w.store.ReconcileRoom(ctx, hint)
			report.Duration = time.Since(started)
			observe(ReconciliationNotification, report, err)
			resetTimer(timer, w.nextInterval(report))
		case <-timer.C:
			started := time.Now()
			report, err := w.store.ReconcileBatch(ctx, w.config.BatchSize)
			report.Duration = time.Since(started)
			observe(ReconciliationPeriodic, report, err)
			resetTimer(timer, w.nextInterval(report))
		}
	}
}

func (w *ReconciliationWorker) listen(ctx context.Context, connections chan<- ReconciliationSource, notifications chan<- RoomHint, observe func(ReconciliationSource, ReconciliationReport, error)) {
	connected := false
	for ctx.Err() == nil {
		source := ReconciliationStartup
		if connected {
			source = ReconciliationReconnect
		}
		err := w.listener.Listen(ctx, func() {
			connected = true
			select {
			case connections <- source:
			default:
			}
		}, func(hint RoomHint) {
			select {
			case notifications <- hint:
			default:
			}
		})
		if ctx.Err() != nil {
			return
		}
		observe(ReconciliationReconnect, ReconciliationReport{Failures: 1}, err)
		timer := time.NewTimer(w.config.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *ReconciliationWorker) drain(ctx context.Context) (ReconciliationReport, error) {
	var total ReconciliationReport
	for range w.config.MaxDrainBatches {
		report, err := w.store.ReconcileBatch(ctx, w.config.BatchSize)
		total.Add(report)
		if err != nil || report.ClaimAttempts < int64(w.config.BatchSize) {
			return total, err
		}
	}
	return total, nil
}

func (w *ReconciliationWorker) nextInterval(report ReconciliationReport) time.Duration {
	if report.EligibleBacklog == 0 && report.ClaimWins == 0 {
		return w.config.IdleInterval
	}
	return w.config.PeriodicInterval
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}
