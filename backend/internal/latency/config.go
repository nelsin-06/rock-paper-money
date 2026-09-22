package latency

import (
	"fmt"
	"time"
)

const (
	SlowQueryThresholdEnv     = "SLOW_QUERY_THRESHOLD"
	SlowOperationThresholdEnv = "SLOW_OPERATION_THRESHOLD"

	DefaultSlowQueryThreshold     = 250 * time.Millisecond
	DefaultSlowOperationThreshold = 2 * time.Second
)

type Config struct {
	SlowQueryThreshold     time.Duration
	SlowOperationThreshold time.Duration
}

func ConfigFromEnv(lookup func(string) string) (Config, error) {
	query, err := positiveDuration(lookup, SlowQueryThresholdEnv, DefaultSlowQueryThreshold)
	if err != nil {
		return Config{}, err
	}
	operation, err := positiveDuration(lookup, SlowOperationThresholdEnv, DefaultSlowOperationThreshold)
	if err != nil {
		return Config{}, err
	}
	return Config{SlowQueryThreshold: query, SlowOperationThreshold: operation}, nil
}

func positiveDuration(lookup func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := lookup(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration, got %q", name, raw)
	}
	return value, nil
}
