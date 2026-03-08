package search

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterReScoreMetrics verifies that metric registration succeeds and returns
// a usable ReScoreMetrics instance with the default (noop) OTel provider.
func TestRegisterReScoreMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	m := RegisterReScoreMetrics(logger)

	require.NotNil(t, m)
	assert.NotNil(t, m.signalContribution, "signalContribution histogram should be initialized")
}

// TestRegisterReScoreMetrics_NilLogger verifies that RegisterReScoreMetrics does not panic
// when given a nil-output logger (slog routes to a discard handler).
func TestRegisterReScoreMetrics_NilLogger(t *testing.T) {
	// slog.New with a nil-writer TextHandler produces a valid logger that discards output.
	logger := slog.New(slog.NewTextHandler(nil, nil))
	m := RegisterReScoreMetrics(logger)

	require.NotNil(t, m)
	assert.NotNil(t, m.signalContribution)
}

// TestReScoreMetrics_Record verifies that Record does not panic when called with
// the noop OTel provider (no real exporter configured). This exercises the full
// signal recording path with various input values.
func TestReScoreMetrics_Record(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	m := RegisterReScoreMetrics(logger)
	require.NotNil(t, m)

	tests := []struct {
		name       string
		assessment float64
		citation   float64
		stability  float64
		agreement  float64
		conflict   float64
	}{
		{
			name:       "all zeros",
			assessment: 0, citation: 0, stability: 0, agreement: 0, conflict: 0,
		},
		{
			name:       "all ones",
			assessment: 1.0, citation: 1.0, stability: 1.0, agreement: 1.0, conflict: 1.0,
		},
		{
			name:       "mixed values",
			assessment: 0.4, citation: 0.25, stability: 0.15, agreement: 0.10, conflict: 0.05,
		},
		{
			name:       "negative values",
			assessment: -0.1, citation: -0.5, stability: 0, agreement: 0, conflict: -1.0,
		},
		{
			name:       "large values",
			assessment: 100.0, citation: 50.0, stability: 25.0, agreement: 10.0, conflict: 5.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Record should not panic with the noop provider.
			assert.NotPanics(t, func() {
				m.Record(context.Background(), tc.assessment, tc.citation, tc.stability, tc.agreement, tc.conflict)
			})
		})
	}
}

// TestReScoreMetrics_RecordWithCancelledContext verifies that Record handles a
// cancelled context gracefully (OTel histograms accept any context).
func TestReScoreMetrics_RecordWithCancelledContext(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))
	m := RegisterReScoreMetrics(logger)
	require.NotNil(t, m)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	assert.NotPanics(t, func() {
		m.Record(ctx, 0.5, 0.3, 0.2, 0.1, 0.05)
	}, "Record should not panic with a cancelled context")
}

// TestRegisterReScoreMetrics_CalledTwice verifies that calling RegisterReScoreMetrics
// multiple times does not panic or produce nil instruments.
func TestRegisterReScoreMetrics_CalledTwice(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nil, nil))

	m1 := RegisterReScoreMetrics(logger)
	m2 := RegisterReScoreMetrics(logger)

	require.NotNil(t, m1)
	require.NotNil(t, m2)
	assert.NotNil(t, m1.signalContribution)
	assert.NotNil(t, m2.signalContribution)
}
