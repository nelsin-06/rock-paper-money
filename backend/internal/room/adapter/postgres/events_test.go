package postgres

import (
	"errors"
	"log/slog"
	"testing"
)

func TestEventsAreNotReadyBeforeListenerStarts(t *testing.T) {
	events := NewEvents(nil, slog.Default())
	if err := events.Ready(); !errors.Is(err, ErrListenerUnavailable) {
		t.Fatalf("Ready() error = %v, want %v", err, ErrListenerUnavailable)
	}
}
