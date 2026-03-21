// Package tracehealth computes aggregate health metrics for an organization's
// decision trace data. It answers the question: "How well is this org using
// Akashi?" by measuring decision quality, evidence coverage, and conflict
// resolution rates.
package tracehealth

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/storage"
)

// Metrics is the top-level trace health response.
type Metrics struct {
	Status                   string                          `json:"status"` // healthy, needs_attention, insufficient_data
	Completeness             *CompletenessMetrics            `json:"completeness"`
	Evidence                 *EvidenceMetrics                `json:"evidence"`
	Conflicts                *ConflictMetrics                `json:"conflicts,omitempty"`
	OutcomeSignals           *storage.OutcomeSignalsSummary  `json:"outcome_signals,omitempty"`
	ConfidenceDistribution   *storage.ConfidenceDistribution `json:"confidence_distribution,omitempty"`
	DecisionTypeDistribution []storage.DecisionTypeCount     `json:"decision_type_distribution,omitempty"`
	Gaps                     []string                        `json:"gaps"`
}

// CompletenessMetrics tracks decision quality and reasoning coverage.
type CompletenessMetrics struct {
	TotalDecisions   int     `json:"total_decisions"`
	AvgCompleteness  float64 `json:"avg_completeness"`
	BelowHalf        int     `json:"below_half"`  // completeness_score < 0.5
	BelowThird       int     `json:"below_third"` // completeness_score < 0.33
	WithReasoning    int     `json:"with_reasoning"`
	ReasoningPct     float64 `json:"reasoning_pct"`
	WithAlternatives int     `json:"with_alternatives"`
	AlternativesPct  float64 `json:"alternatives_pct"`
}

// EvidenceMetrics tracks evidence coverage across decisions.
type EvidenceMetrics struct {
	TotalDecisions  int     `json:"total_decisions"`
	TotalRecords    int     `json:"total_records"`
	AvgPerDecision  float64 `json:"avg_per_decision"`
	WithEvidence    int     `json:"with_evidence"`
	WithoutEvidence int     `json:"without_evidence"`
	CoveragePct     float64 `json:"coverage_pct"`
}

// ConflictMetrics tracks conflict detection and resolution rates.
type ConflictMetrics struct {
	Total        int     `json:"total"`
	Open         int     `json:"open"`
	Acknowledged int     `json:"acknowledged"`
	Resolved     int     `json:"resolved"`
	WontFix      int     `json:"wont_fix"`
	ResolvedPct  float64 `json:"resolved_pct"`
}

// Service computes trace health metrics.
type Service struct {
	db storage.Store
}

// New creates a trace health service.
func New(db storage.Store) *Service {
	return &Service{db: db}
}

// Compute calculates all trace health metrics for an organization.
// When from/to are non-nil they scope the decision and conflict windows.
// Pass nil, nil to get all-time metrics (equivalent to the legacy behavior).
func (s *Service) Compute(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (*Metrics, error) {
	qs, err := s.db.GetDecisionQualityStats(ctx, orgID, from, to)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: quality stats: %w", err)
	}

	if qs.Total == 0 {
		return &Metrics{
			Status:       "insufficient_data",
			Completeness: &CompletenessMetrics{},
			Evidence:     &EvidenceMetrics{},
			Gaps:         []string{"No decisions recorded yet. Start tracing to see health metrics."},
		}, nil
	}

	es, err := s.db.GetEvidenceCoverageStats(ctx, orgID, from, to)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: evidence stats: %w", err)
	}

	cc, err := s.db.GetConflictStatusCounts(ctx, orgID, from, to)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: conflict status counts: %w", err)
	}

	var resolvedPct float64
	if cc.Total > 0 {
		resolvedPct = float64(cc.Resolved) / float64(cc.Total) * 100
	}

	var reasoningPct float64
	if qs.Total > 0 {
		reasoningPct = float64(qs.WithReasoning) / float64(qs.Total) * 100
	}

	var alternativesPct float64
	if qs.Total > 0 {
		alternativesPct = float64(qs.WithAlternatives) / float64(qs.Total) * 100
	}

	m := &Metrics{
		Completeness: &CompletenessMetrics{
			TotalDecisions:   qs.Total,
			AvgCompleteness:  qs.AvgCompleteness,
			BelowHalf:        qs.BelowHalf,
			BelowThird:       qs.BelowThird,
			WithReasoning:    qs.WithReasoning,
			ReasoningPct:     reasoningPct,
			WithAlternatives: qs.WithAlternatives,
			AlternativesPct:  alternativesPct,
		},
		Evidence: &EvidenceMetrics{
			TotalDecisions:  es.TotalDecisions,
			TotalRecords:    es.TotalRecords,
			AvgPerDecision:  es.AvgPerDecision,
			WithEvidence:    es.WithEvidence,
			WithoutEvidence: es.WithoutEvidenceCount,
			CoveragePct:     es.CoveragePercent,
		},
		Gaps: []string{},
	}

	if cc.Total > 0 {
		m.Conflicts = &ConflictMetrics{
			Total:        cc.Total,
			Open:         cc.Open,
			Acknowledged: cc.Acknowledged,
			Resolved:     cc.Resolved,
			WontFix:      cc.WontFix,
			ResolvedPct:  resolvedPct,
		}
	}

	// Outcome signals: temporal, graph, and fate aggregate counts.
	os, err := s.db.GetOutcomeSignalsSummary(ctx, orgID, from, to)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: outcome signals: %w", err)
	}
	if qs.Total > 0 {
		m.OutcomeSignals = &os
	}

	// Confidence distribution: histogram + per-agent breakdown.
	cd, err := s.db.GetConfidenceDistribution(ctx, orgID, from, to)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: confidence distribution: %w", err)
	}
	if qs.Total > 0 {
		m.ConfidenceDistribution = &cd
	}

	// High-confidence outcome signals: behavioral calibration check.
	hcos, err := s.db.GetHighConfOutcomeSignals(ctx, orgID, from, to)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: high-conf outcome signals: %w", err)
	}

	// Decision type distribution.
	dtd, err := s.db.GetDecisionTypeDistribution(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: decision type distribution: %w", err)
	}
	if len(dtd) > 0 {
		m.DecisionTypeDistribution = dtd
	}

	// Gap detection: rule-based, max 3 gaps, ordered by severity.
	m.Gaps = computeGaps(qs, cc.Total, cc.Open, os, cd, hcos)

	// Overall status.
	m.Status = computeStatus(qs, cc.Open)

	return m, nil
}

// computeGaps identifies the most important areas for improvement.
// Returns at most 3 gaps, ordered by severity.
func computeGaps(qs storage.DecisionQualityStats, totalConflicts, openConflicts int, os storage.OutcomeSignalsSummary, cd storage.ConfidenceDistribution, hcos storage.HighConfOutcomeSignals) []string {
	var gaps []string

	// Most severe first.
	if qs.AvgCompleteness < 0.3 {
		gaps = append(gaps, fmt.Sprintf(
			"Average completeness score is %.2f. Most decisions lack substantive reasoning.", qs.AvgCompleteness))
	}

	if openConflicts > 0 && totalConflicts > 0 {
		gaps = append(gaps, fmt.Sprintf(
			"%d of %d conflicts are unresolved.", openConflicts, totalConflicts))
	}

	if len(gaps) < 3 && qs.BelowHalf > 0 {
		gaps = append(gaps, fmt.Sprintf(
			"%d decisions have completeness scores below 0.5.", qs.BelowHalf))
	}

	// Confidence calibration gap — tiered by signal quality.
	if len(gaps) < 3 {
		if g := confidenceCalibrationGap(hcos, cd); g != "" {
			gaps = append(gaps, g)
		}
	}

	// Outcome signal gaps (Spec 35).
	if len(gaps) < 3 && os.DecisionsTotal > 0 {
		revisedPct := float64(os.RevisedWithin48h) / float64(os.DecisionsTotal) * 100
		if revisedPct > 10 {
			gaps = append(gaps, fmt.Sprintf(
				"%d decisions (%.0f%%) were revised within 48 hours.", os.RevisedWithin48h, revisedPct))
		}
	}

	if len(gaps) < 3 && os.DecisionsTotal > 0 {
		neverCitedPct := float64(os.NeverCited) / float64(os.DecisionsTotal) * 100
		if neverCitedPct > 70 {
			gaps = append(gaps, fmt.Sprintf(
				"%d decisions (%.0f%%) have never been cited as a precedent. Set precedent_ref when tracing to build the attribution graph.",
				os.NeverCited, neverCitedPct))
		}
	}

	if len(gaps) > 3 {
		gaps = gaps[:3]
	}
	return gaps
}

// confidenceCalibrationGap returns a single gap string describing a confidence
// calibration problem, or "" if none is detected.
// Priority: outcome correctness > revision rate > conflict loss > completeness fallback.
func confidenceCalibrationGap(hcos storage.HighConfOutcomeSignals, cd storage.ConfidenceDistribution) string {
	// Tier 1: outcome score data — most reliable signal.
	if hcos.AssessedCount >= 5 && hcos.AvgOutcomeScore < 0.70 {
		return fmt.Sprintf(
			"High-confidence decisions (>=0.85) average only %.0f%% correctness from assessments. Confidence scores may be miscalibrated.",
			hcos.AvgOutcomeScore*100)
	}

	// Tier 2: behavioral signals — visible actions are harder to fake.
	if hcos.Total > 0 {
		revisionRate := float64(hcos.RevisedWithin48h) / float64(hcos.Total)
		if revisionRate > 0.25 {
			return fmt.Sprintf(
				"%.0f%% of high-confidence decisions were revised within 48 hours, suggesting confidence levels are too high.",
				revisionRate*100)
		}
		conflictLossRate := float64(hcos.ConflictsLost) / float64(hcos.Total)
		if conflictLossRate > 0.15 {
			return fmt.Sprintf(
				"%.0f%% of high-confidence decisions lost conflicts, suggesting confidence levels are too high.",
				conflictLossRate*100)
		}
	}

	// Tier 3: completeness fallback — only when no behavioral data fires.
	if cd.TotalDecisions > 0 && cd.HighConfAvgCompleteness < 0.6 {
		if cd.AvgConfidence > 0.82 {
			return fmt.Sprintf(
				"Avg confidence is %.2f but high-confidence decisions average only %.0f%% completeness. Add reasoning, alternatives, or evidence to support high confidence scores.",
				cd.AvgConfidence, cd.HighConfAvgCompleteness*100)
		}
		if cd.OverconfidentPct > 60 {
			return fmt.Sprintf(
				"%.0f%% of decisions have confidence >= 0.85 but average only %.0f%% completeness. Add reasoning, alternatives, or evidence to support high confidence scores.",
				cd.OverconfidentPct, cd.HighConfAvgCompleteness*100)
		}
	}

	return ""
}

// computeStatus determines the overall health status.
// Evidence coverage is intentionally excluded: most orgs don't provide evidence
// records and that's expected. Missing evidence is surfaced as a coverage tip,
// not a health failure.
func computeStatus(qs storage.DecisionQualityStats, openConflicts int) string {
	problems := 0
	if qs.AvgCompleteness < 0.3 {
		problems++
	}
	if openConflicts > 3 {
		problems++
	}

	if problems >= 1 {
		return "needs_attention"
	}
	return "healthy"
}
