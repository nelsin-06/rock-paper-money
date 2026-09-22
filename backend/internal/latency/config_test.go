package latency

import (
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name          string
		values        map[string]string
		wantQuery     time.Duration
		wantOperation time.Duration
		wantError     string
	}{
		{
			name:          "defaults",
			values:        map[string]string{},
			wantQuery:     250 * time.Millisecond,
			wantOperation: 2 * time.Second,
		},
		{
			name: "custom durations",
			values: map[string]string{
				SlowQueryThresholdEnv:     "425ms",
				SlowOperationThresholdEnv: "3.5s",
			},
			wantQuery:     425 * time.Millisecond,
			wantOperation: 3500 * time.Millisecond,
		},
		{
			name:      "invalid duration",
			values:    map[string]string{SlowQueryThresholdEnv: "250"},
			wantError: SlowQueryThresholdEnv,
		},
		{
			name:      "non-positive duration",
			values:    map[string]string{SlowOperationThresholdEnv: "0s"},
			wantError: SlowOperationThresholdEnv,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := ConfigFromEnv(func(name string) string { return tt.values[name] })
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("ConfigFromEnv() error = %v, want error containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.SlowQueryThreshold != tt.wantQuery || config.SlowOperationThreshold != tt.wantOperation {
				t.Fatalf("ConfigFromEnv() = %+v, want query=%v operation=%v", config, tt.wantQuery, tt.wantOperation)
			}
		})
	}
}
