package search

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ashita-ai/akashi/internal/telemetry"
)

// ReScoreMetrics holds pre-created OpenTelemetry instruments for per-signal
// contribution tracking in ReScore (issue #264).
type ReScoreMetrics struct {
	signalContribution metric.Float64Histogram
}

// RegisterReScoreMetrics creates OTel instruments for the search re-scoring pipeline.
func RegisterReScoreMetrics(logger *slog.Logger) *ReScoreMetrics {
	meter := telemetry.Meter("akashi/search")
	m := &ReScoreMetrics{}

	var err error
	m.signalContribution, err = meter.Float64Histogram("akashi.search.rescore_signal_contribution",
		metric.WithDescription("Absolute contribution of each signal to the outcome weight in ReScore"),
	)
	if err != nil {
		logger.Warn("search: failed to create akashi.search.rescore_signal_contribution metric", "error", err)
		m.signalContribution, _ = meter.Float64Histogram("akashi.search.rescore_signal_contribution.fallback")
	}

	return m
}

// Record emits per-signal contribution values to the histogram.
func (m *ReScoreMetrics) Record(ctx context.Context, assessment, citation, stability, agreement, conflict float64) {
	m.signalContribution.Record(ctx, assessment, metric.WithAttributes(attribute.String("signal", "assessment")))
	m.signalContribution.Record(ctx, citation, metric.WithAttributes(attribute.String("signal", "citation")))
	m.signalContribution.Record(ctx, stability, metric.WithAttributes(attribute.String("signal", "stability")))
	m.signalContribution.Record(ctx, agreement, metric.WithAttributes(attribute.String("signal", "agreement")))
	m.signalContribution.Record(ctx, conflict, metric.WithAttributes(attribute.String("signal", "conflict")))
}
