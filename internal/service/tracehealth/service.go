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
	"golang.org/x/sync/errgroup"

	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Metrics is the top-level trace health response.
type Metrics struct {
	Status                   string                             `json:"status"` // healthy, needs_attention, insufficient_data
	Completeness             *CompletenessMetrics               `json:"completeness"`
	Evidence                 *EvidenceMetrics                   `json:"evidence"`
	Conflicts                *ConflictMetrics                   `json:"conflicts,omitempty"`
	OutcomeSignals           *storage.OutcomeSignalsSummary     `json:"outcome_signals,omitempty"`
	ConfidenceDistribution   *storage.ConfidenceDistribution    `json:"confidence_distribution,omitempty"`
	HighConfOutcomeSignals   *storage.HighConfOutcomeSignals    `json:"high_conf_outcome_signals,omitempty"`
	ConfidenceCalibration    *storage.ConfidenceCalibration     `json:"confidence_calibration,omitempty"`
	DecisionTypeDistribution []storage.DecisionTypeCount        `json:"decision_type_distribution,omitempty"`
	CompletenessByType       []storage.DecisionTypeCompleteness `json:"completeness_by_type,omitempty"`
	Gaps                     []string                           `json:"gaps"`
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
	Total         int     `json:"total"`
	Open          int     `json:"open"`
	Resolved      int     `json:"resolved"`
	FalsePositive int     `json:"false_positive"`
	ResolvedPct   float64 `json:"resolved_pct"`
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
	// Quality stats must run first: a zero total triggers an early return.
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

	// The remaining 8 queries are independent — run them concurrently.
	var (
		es   storage.EvidenceCoverageStats
		cc   storage.ConflictStatusCounts
		os   storage.OutcomeSignalsSummary
		cd   storage.ConfidenceDistribution
		hcos storage.HighConfOutcomeSignals
		cal  storage.ConfidenceCalibration
		dtd  []storage.DecisionTypeCount
		cbt  []storage.DecisionTypeCompleteness
	)

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		var err error
		es, err = s.db.GetEvidenceCoverageStats(gctx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("tracehealth: evidence stats: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		cc, err = s.db.GetConflictStatusCounts(gctx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("tracehealth: conflict status counts: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		os, err = s.db.GetOutcomeSignalsSummary(gctx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("tracehealth: outcome signals: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		cd, err = s.db.GetConfidenceDistribution(gctx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("tracehealth: confidence distribution: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		hcos, err = s.db.GetHighConfOutcomeSignals(gctx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("tracehealth: high-conf outcome signals: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		cal, err = s.db.GetConfidenceCalibration(gctx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("tracehealth: confidence calibration: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		dtd, err = s.db.GetDecisionTypeDistribution(gctx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("tracehealth: decision type distribution: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		var err error
		cbt, err = s.db.GetCompletenessByDecisionType(gctx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("tracehealth: completeness by type: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Assemble metrics from the concurrent results.
	var resolvedPct float64
	if cc.Total > 0 {
		resolvedPct = float64(cc.Resolved) / float64(cc.Total) * 100
	}

	reasoningPct := float64(qs.WithReasoning) / float64(qs.Total) * 100
	alternativesPct := float64(qs.WithAlternatives) / float64(qs.Total) * 100

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
			Total:         cc.Total,
			Open:          cc.Open,
			Resolved:      cc.Resolved,
			FalsePositive: cc.FalsePositive,
			ResolvedPct:   resolvedPct,
		}
	}

	if qs.Total > 0 {
		m.OutcomeSignals = &os
	}

	if qs.Total > 0 {
		m.ConfidenceDistribution = &cd
	}

	if hcos.Total > 0 {
		m.HighConfOutcomeSignals = &hcos
	}

	if qs.Total > 0 {
		m.ConfidenceCalibration = &cal
	}

	if len(dtd) > 0 {
		m.DecisionTypeDistribution = dtd
	}

	if len(cbt) > 0 {
		enrichCompletenessWithExpectations(cbt)
		m.CompletenessByType = cbt
	}

	m.Gaps = computeGaps(qs, cc.Total, cc.Open, os, cd, cal)
	m.Status = computeStatus(qs, cc.Open)

	return m, nil
}

// enrichCompletenessWithExpectations annotates each row in completeness_by_type
// with the per-type health threshold and status. This is a server-side
// enrichment — the storage layer returns raw avg completeness, and this
// function adds the type-aware health judgment.
func enrichCompletenessWithExpectations(rows []storage.DecisionTypeCompleteness) {
	for i := range rows {
		exp := quality.ExpectationFor(rows[i].DecisionType)
		rows[i].ExpectedMin = exp.ExpectedMin
		if rows[i].AvgCompleteness >= exp.ExpectedMin {
			rows[i].Status = "healthy"
		} else {
			rows[i].Status = "needs_attention"
		}
	}
}

// computeGaps identifies the most important areas for improvement.
// Returns at most 3 gaps, ordered by severity.
func computeGaps(qs storage.DecisionQualityStats, totalConflicts, openConflicts int, os storage.OutcomeSignalsSummary, cd storage.ConfidenceDistribution, cal storage.ConfidenceCalibration) []string {
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

	// Confidence calibration gap: uses outcome data or revision-rate proxy
	// to flag when declared confidence doesn't predict actual outcomes.
	// Falls back to distribution shape when insufficient behavioral data exists.
	if len(gaps) < 3 {
		if g := confidenceCalibrationGap(cal, cd); g != "" {
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

// confidenceCalibrationGap returns a gap message if confidence is miscalibrated,
// or "" if calibration looks acceptable. Uses three tiers of evidence:
//  1. Assessment outcomes (ground truth) — when available
//  2. Revision rates (temporal proxy) — always available with enough data
//  3. Distribution shape (static fallback) — when behavioral data is insufficient
func confidenceCalibrationGap(cal storage.ConfidenceCalibration, cd storage.ConfidenceDistribution) string {
	// Build tier lookup for readable access.
	tiers := make(map[string]storage.ConfidenceTier, len(cal.Tiers))
	for _, t := range cal.Tiers {
		tiers[t.Tier] = t
	}

	high, hasHigh := tiers["high"]
	mid, hasMid := tiers["mid"]

	// Tier 1: Assessment-based calibration (highest signal).
	if cal.HasOutcomeData && hasHigh && hasMid && high.AvgOutcome != nil && mid.AvgOutcome != nil && high.AssessedCount >= 3 && mid.AssessedCount >= 3 {
		if *high.AvgOutcome < *mid.AvgOutcome {
			return fmt.Sprintf(
				"High-confidence decisions (>= 0.85) have avg outcome score %.2f vs %.2f for mid-range — confidence is not predicting outcomes.",
				*high.AvgOutcome, *mid.AvgOutcome)
		}
		return "" // calibrated by outcome data
	}

	// Tier 2: Revision-rate proxy (temporal signal).
	if hasHigh && hasMid && high.Total >= 5 && mid.Total >= 5 {
		if high.RevisionRate > mid.RevisionRate && high.RevisionRate > 5 {
			return fmt.Sprintf(
				"High-confidence decisions (>= 0.85) are revised within 48h at %.0f%% vs %.0f%% for mid-range — agents may be over-committing.",
				high.RevisionRate, mid.RevisionRate)
		}
		return "" // calibrated by revision rate
	}

	// Tier 3: Distribution shape fallback (static, least signal).
	if cd.TotalDecisions > 0 {
		if cd.AvgConfidence > 0.82 {
			return fmt.Sprintf(
				"Avg confidence is %.2f (%.0f%% of decisions >= 0.85) — above the recommended 0.4–0.8 range. Over-confident scoring reduces signal quality.",
				cd.AvgConfidence, cd.OverconfidentPct)
		}
		if cd.OverconfidentPct > 60 {
			return fmt.Sprintf(
				"%.0f%% of decisions have confidence >= 0.85 (avg %.2f). A heavy tail of over-confident scores reduces signal quality.",
				cd.OverconfidentPct, cd.AvgConfidence)
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
