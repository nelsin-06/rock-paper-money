package application

import (
	"context"
	"time"
)

const (
	DisconnectGracePeriod    = 20 * time.Second
	FundedInactivityDeadline = 30 * time.Second
)

type DeadlineKind string

const (
	DeadlineDisconnect DeadlineKind = "disconnect"
	DeadlineInactivity DeadlineKind = "inactivity"
)

type ConnectionLease struct {
	ID             string
	RoomCode       string
	AccountID      string
	SessionDigest  []byte
	ConnectedAt    time.Time
	LeaseExpiresAt time.Time
}

type PresenceTransition struct {
	RoomCode   string
	Role       string
	Generation uint64
	LastSocket bool
	GraceDueAt time.Time
}

type DeadlineResult struct {
	Processed bool
	Settled   bool
	Stale     bool
	RoomCode  string
	Round     uint64
	Kind      DeadlineKind
	DueAt     time.Time
}

type DeadlineRepository interface {
	ReapExpiredConnections(context.Context, time.Time) (int64, error)
	ProcessNextDeadline(context.Context, time.Time) (DeadlineResult, error)
	CleanupDeadlines(context.Context, time.Time) (int64, error)
}
