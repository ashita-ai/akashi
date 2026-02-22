// Package tracehealth computes aggregate health metrics for an organization's
// decision trace data. It answers the question: "How well is this org using
// Akashi?" by measuring decision quality, evidence coverage, and conflict
// resolution rates.
package tracehealth

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/storage"
)

// Metrics is the top-level trace health response.
type Metrics struct {
	Status         string                         `json:"status"` // healthy, needs_attention, insufficient_data
	Completeness   *CompletenessMetrics           `json:"completeness"`
	Evidence       *EvidenceMetrics               `json:"evidence"`
	Conflicts      *ConflictMetrics               `json:"conflicts,omitempty"`
	OutcomeSignals *storage.OutcomeSignalsSummary `json:"outcome_signals,omitempty"`
	Gaps           []string                       `json:"gaps"`
}

// CompletenessMetrics tracks decision quality and reasoning coverage.
type CompletenessMetrics struct {
	TotalDecisions   int     `json:"total_decisions"`
	AvgQuality       float64 `json:"avg_quality"`
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
	db *storage.DB
}

// New creates a trace health service.
func New(db *storage.DB) *Service {
	return &Service{db: db}
}

// Compute calculates all trace health metrics for an organization.
func (s *Service) Compute(ctx context.Context, orgID uuid.UUID) (*Metrics, error) {
	qs, err := s.db.GetDecisionQualityStats(ctx, orgID)
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

	es, err := s.db.GetEvidenceCoverageStats(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: evidence stats: %w", err)
	}

	cc, err := s.db.GetConflictStatusCounts(ctx, orgID)
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
			AvgQuality:       qs.AvgQuality,
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
	os, err := s.db.GetOutcomeSignalsSummary(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("tracehealth: outcome signals: %w", err)
	}
	if qs.Total > 0 {
		m.OutcomeSignals = &os
	}

	// Gap detection: rule-based, max 3 gaps, ordered by severity.
	m.Gaps = computeGaps(qs, es, cc.Total, cc.Open, os)

	// Overall status.
	m.Status = computeStatus(qs, es, cc.Open)

	return m, nil
}

// computeGaps identifies the most important areas for improvement.
// Returns at most 3 gaps, ordered by severity.
func computeGaps(qs storage.DecisionQualityStats, es storage.EvidenceCoverageStats, totalConflicts, openConflicts int, os storage.OutcomeSignalsSummary) []string {
	var gaps []string

	// Most severe first.
	if qs.AvgQuality < 0.3 {
		gaps = append(gaps, fmt.Sprintf(
			"Average decision quality is %.2f. Most decisions lack substantive reasoning.", qs.AvgQuality))
	}

	if es.CoveragePercent < 50 {
		gaps = append(gaps, "Less than half of decisions have supporting evidence.")
	}

	if openConflicts > 0 && totalConflicts > 0 {
		gaps = append(gaps, fmt.Sprintf(
			"%d of %d conflicts are unresolved.", openConflicts, totalConflicts))
	}

	if len(gaps) < 3 && es.WithoutEvidenceCount > 0 {
		pct := 0.0
		if es.TotalDecisions > 0 {
			pct = float64(es.WithoutEvidenceCount) / float64(es.TotalDecisions) * 100
		}
		gaps = append(gaps, fmt.Sprintf(
			"%d decisions (%.0f%%) lack evidence records.", es.WithoutEvidenceCount, pct))
	}

	if len(gaps) < 3 && qs.BelowHalf > 0 {
		gaps = append(gaps, fmt.Sprintf(
			"%d decisions have quality scores below 0.5.", qs.BelowHalf))
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

// computeStatus determines the overall health status.
func computeStatus(qs storage.DecisionQualityStats, es storage.EvidenceCoverageStats, openConflicts int) string {
	problems := 0
	if qs.AvgQuality < 0.3 {
		problems++
	}
	if es.CoveragePercent < 50 {
		problems++
	}
	if openConflicts > 3 {
		problems++
	}

	if problems >= 2 {
		return "needs_attention"
	}
	return "healthy"
}
