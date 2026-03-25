package conflicts

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ashita-ai/akashi/internal/telemetry"
)

// Metrics holds pre-created OpenTelemetry instruments for conflict detection.
type Metrics struct {
	detected            metric.Int64Counter
	resolved            metric.Int64Counter
	llmCalls            metric.Int64Counter
	candidatesEvaluated metric.Int64Counter
	claimLevelWins      metric.Int64Counter
	workflowFiltered    metric.Int64Counter
	coordinatedFiltered metric.Int64Counter
	outcomeSimFiltered  metric.Int64Counter

	confidenceFloorFiltered metric.Int64Counter
	noopClaimGateFiltered   metric.Int64Counter
	transitiveGroupFiltered metric.Int64Counter
	fpPatternFiltered       metric.Int64Counter

	scoringDuration    metric.Float64Histogram
	llmCallDuration    metric.Float64Histogram
	significanceDist   metric.Float64Histogram
	candidatesExamined metric.Float64Histogram
}

// ResolutionRecorder records conflict resolution events for OpenTelemetry metrics.
// Implemented by the Scorer; used by HTTP handlers that resolve conflicts
// without going through the scoring pipeline.
type ResolutionRecorder interface {
	RecordResolution(ctx context.Context, status, conflictKind string, count int)
}

// RecordResolution implements ResolutionRecorder, incrementing the resolved counter
// with the given status and conflict_kind labels.
func (s *Scorer) RecordResolution(ctx context.Context, status, conflictKind string, count int) {
	s.metrics.resolved.Add(ctx, int64(count), metric.WithAttributes(
		attribute.String("status", status),
		attribute.String("conflict_kind", conflictKind),
	))
}

// validatorTypeLabel returns a low-cardinality label for the configured validator.
func validatorTypeLabel(v Validator) string {
	switch v.(type) {
	case NoopValidator:
		return "noop"
	case *OllamaValidator:
		return "ollama"
	case *OpenAIValidator:
		return "openai"
	default:
		return "custom"
	}
}

// registerMetrics creates all OTel instruments for the conflict pipeline.
// Called once from NewScorer.
func (s *Scorer) registerMetrics() {
	meter := telemetry.Meter("akashi/conflicts")

	var err error

	// --- Counters ---

	s.metrics.detected, err = meter.Int64Counter("akashi.conflicts.detected",
		metric.WithDescription("Total conflicts detected by the scoring pipeline"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.detected metric", "error", err)
		s.metrics.detected, _ = meter.Int64Counter("akashi.conflicts.detected.fallback")
	}

	s.metrics.resolved, err = meter.Int64Counter("akashi.conflicts.resolved",
		metric.WithDescription("Total conflicts resolved (resolved or false_positive)"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.resolved metric", "error", err)
		s.metrics.resolved, _ = meter.Int64Counter("akashi.conflicts.resolved.fallback")
	}

	s.metrics.llmCalls, err = meter.Int64Counter("akashi.conflicts.llm_calls",
		metric.WithDescription("Total LLM validation calls for conflict classification"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.llm_calls metric", "error", err)
		s.metrics.llmCalls, _ = meter.Int64Counter("akashi.conflicts.llm_calls.fallback")
	}

	s.metrics.candidatesEvaluated, err = meter.Int64Counter("akashi.conflicts.candidates_evaluated",
		metric.WithDescription("Total candidate pairs evaluated for conflict scoring"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.candidates_evaluated metric", "error", err)
		s.metrics.candidatesEvaluated, _ = meter.Int64Counter("akashi.conflicts.candidates_evaluated.fallback")
	}

	s.metrics.claimLevelWins, err = meter.Int64Counter("akashi.conflicts.claim_level_wins",
		metric.WithDescription("Times claim-level scoring produced higher significance than full-outcome scoring"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.claim_level_wins metric", "error", err)
		s.metrics.claimLevelWins, _ = meter.Int64Counter("akashi.conflicts.claim_level_wins.fallback")
	}

	s.metrics.workflowFiltered, err = meter.Int64Counter("akashi.conflicts.workflow_filtered",
		metric.WithDescription("Candidate pairs filtered by complementary workflow heuristic (review→fix, same-agent refinement, precedent chain)"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.workflow_filtered metric", "error", err)
		s.metrics.workflowFiltered, _ = meter.Int64Counter("akashi.conflicts.workflow_filtered.fallback")
	}

	s.metrics.coordinatedFiltered, err = meter.Int64Counter("akashi.conflicts.coordinated_filtered",
		metric.WithDescription("Candidate pairs filtered by coordinated change detection (same commit/PR/branch)"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.coordinated_filtered metric", "error", err)
		s.metrics.coordinatedFiltered, _ = meter.Int64Counter("akashi.conflicts.coordinated_filtered.fallback")
	}

	s.metrics.outcomeSimFiltered, err = meter.Int64Counter("akashi.conflicts.outcome_sim_filtered",
		metric.WithDescription("Candidate pairs filtered by outcome similarity floor (outcomes effectively agree)"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.outcome_sim_filtered metric", "error", err)
		s.metrics.outcomeSimFiltered, _ = meter.Int64Counter("akashi.conflicts.outcome_sim_filtered.fallback")
	}

	s.metrics.confidenceFloorFiltered, err = meter.Int64Counter("akashi.conflicts.confidence_floor_filtered",
		metric.WithDescription("Candidate pairs filtered by confidence floor (both decisions too exploratory)"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.confidence_floor_filtered metric", "error", err)
		s.metrics.confidenceFloorFiltered, _ = meter.Int64Counter("akashi.conflicts.confidence_floor_filtered.fallback")
	}

	s.metrics.noopClaimGateFiltered, err = meter.Int64Counter("akashi.conflicts.noop_claim_gate_filtered",
		metric.WithDescription("Candidate pairs filtered by noop claim gate (no claim-level confirmation without LLM)"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.noop_claim_gate_filtered metric", "error", err)
		s.metrics.noopClaimGateFiltered, _ = meter.Int64Counter("akashi.conflicts.noop_claim_gate_filtered.fallback")
	}

	s.metrics.transitiveGroupFiltered, err = meter.Int64Counter("akashi.conflicts.transitive_group_filtered",
		metric.WithDescription("Candidate pairs filtered by transitive group dedup (both decisions already in same group)"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.transitive_group_filtered metric", "error", err)
		s.metrics.transitiveGroupFiltered, _ = meter.Int64Counter("akashi.conflicts.transitive_group_filtered.fallback")
	}

	s.metrics.fpPatternFiltered, err = meter.Int64Counter("akashi.conflicts.fp_pattern_filtered",
		metric.WithDescription("Candidate pairs where significance threshold was doubled due to high historical FP rate for type pair"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.fp_pattern_filtered metric", "error", err)
		s.metrics.fpPatternFiltered, _ = meter.Int64Counter("akashi.conflicts.fp_pattern_filtered.fallback")
	}

	// --- Histograms ---

	s.metrics.scoringDuration, err = meter.Float64Histogram("akashi.conflicts.scoring_duration_ms",
		metric.WithDescription("Per-decision conflict scoring latency"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.scoring_duration_ms metric", "error", err)
		s.metrics.scoringDuration, _ = meter.Float64Histogram("akashi.conflicts.scoring_duration_ms.fallback")
	}

	s.metrics.llmCallDuration, err = meter.Float64Histogram("akashi.conflicts.llm_call_duration_ms",
		metric.WithDescription("Per-call LLM validation latency for conflict classification"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.llm_call_duration_ms metric", "error", err)
		s.metrics.llmCallDuration, _ = meter.Float64Histogram("akashi.conflicts.llm_call_duration_ms.fallback")
	}

	s.metrics.significanceDist, err = meter.Float64Histogram("akashi.conflicts.significance_distribution",
		metric.WithDescription("Significance scores of detected conflicts"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.significance_distribution metric", "error", err)
		s.metrics.significanceDist, _ = meter.Float64Histogram("akashi.conflicts.significance_distribution.fallback")
	}

	s.metrics.candidatesExamined, err = meter.Float64Histogram("akashi.conflicts.candidates_examined",
		metric.WithDescription("Number of candidates examined (past early-exit pruning) per decision scoring run"),
	)
	if err != nil {
		s.logger.Warn("conflicts: failed to create akashi.conflicts.candidates_examined metric", "error", err)
		s.metrics.candidatesExamined, _ = meter.Float64Histogram("akashi.conflicts.candidates_examined.fallback")
	}

	// --- Observable gauges ---

	registerObservableGauges(meter, s.db, s.logger)
}

// registerObservableGauges registers callback-driven gauges that query the database.
func registerObservableGauges(meter metric.Meter, db gaugeQuerier, logger *slog.Logger) {
	_, err := meter.Int64ObservableGauge("akashi.conflicts.open_total",
		metric.WithDescription("Current total of open conflicts"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			count, err := db.GetGlobalOpenConflictCount(ctx)
			if err != nil {
				logger.Debug("conflicts: open_total gauge query failed", "error", err)
				return nil // non-fatal: skip this observation
			}
			o.Observe(count)
			return nil
		}),
	)
	if err != nil {
		logger.Warn("conflicts: failed to create akashi.conflicts.open_total gauge", "error", err)
	}

	_, err = meter.Int64ObservableGauge("akashi.conflicts.backfill_remaining",
		metric.WithDescription("Decisions with embeddings not yet conflict-scored"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			count, err := db.CountUnscoredDecisions(ctx)
			if err != nil {
				logger.Debug("conflicts: backfill_remaining gauge query failed", "error", err)
				return nil // non-fatal: skip this observation
			}
			o.Observe(count)
			return nil
		}),
	)
	if err != nil {
		logger.Warn("conflicts: failed to create akashi.conflicts.backfill_remaining gauge", "error", err)
	}

	_, err = meter.Float64ObservableGauge("akashi.conflicts.false_positive_rate",
		metric.WithDescription("Rolling 30-day false-positive rate: false_positive / (resolved + false_positive). Signals LLM validator drift when elevated."),
		metric.WithFloat64Callback(func(ctx context.Context, o metric.Float64Observer) error {
			rate, err := db.GetGlobalFalsePositiveRate(ctx)
			if err != nil {
				logger.Debug("conflicts: false_positive_rate gauge query failed", "error", err)
				return nil // non-fatal: skip this observation
			}
			o.Observe(rate)
			return nil
		}),
	)
	if err != nil {
		logger.Warn("conflicts: failed to create akashi.conflicts.false_positive_rate gauge", "error", err)
	}
}

// gaugeQuerier is the subset of storage.DB needed by observable gauge callbacks.
type gaugeQuerier interface {
	GetGlobalOpenConflictCount(ctx context.Context) (int64, error)
	CountUnscoredDecisions(ctx context.Context) (int64, error)
	GetGlobalFalsePositiveRate(ctx context.Context) (float64, error)
}
