// Package quality provides decision trace completeness scoring.
// Completeness scores (0.0-1.0) measure trace completeness at write time —
// whether the agent provided reasoning, alternatives, evidence, etc.
// They do NOT measure whether the decision was correct or adopted.
package quality

import (
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

// StandardDecisionTypes are the canonical types from prompt templates.
// Using standard types improves discoverability and consistency.
var StandardDecisionTypes = map[string]bool{
	"model_selection": true,
	"architecture":    true,
	"data_source":     true,
	"error_handling":  true,
	"feature_scope":   true,
	"trade_off":       true,
	"deployment":      true,
	"security":        true,
	"code_review":     true,
	"investigation":   true,
	"planning":        true,
	"assessment":      true,
}

// Score computes a completeness score (0.0-1.0) for a trace decision.
// Higher scores indicate more complete traces (more fields populated).
//
// All weights below are uncalibrated — chosen by hand without empirical basis.
// See issue #252 (Tier 2) for the plan to fit weights against assessed data.
//
// Scoring factors:
//   - Confidence present and reasonable (0.05-0.95): 0.15 (uncalibrated)
//   - Reasoning substantive (>50 chars): up to 0.25 (uncalibrated)
//   - Alternatives with substantive rejection reasons: up to 0.20 (uncalibrated)
//   - Evidence provided: up to 0.15 (uncalibrated)
//   - Standard decision type: 0.10 (uncalibrated)
//   - Outcome substantive (>20 chars): 0.05 (uncalibrated)
//   - Precedent reference set: 0.10 (uncalibrated)
func Score(d model.TraceDecision, hasPrecedentRef bool) float32 {
	var score float32

	// Factor 1: Confidence is present and reasonable (uncalibrated: 0.15).
	// Extreme values (exactly 0 or 1) are often defaults, so we reward mid-range.
	// Strict inequality: exactly 0.05 and 0.95 fall to edge tier (0.10).
	if d.Confidence > 0.05 && d.Confidence < 0.95 {
		score += 0.15
	} else if d.Confidence > 0 && d.Confidence < 1 {
		score += 0.10
	}

	// Factor 2: Reasoning is substantive (uncalibrated: up to 0.25).
	if d.Reasoning != nil {
		reasoningLen := len(strings.TrimSpace(*d.Reasoning))
		switch {
		case reasoningLen > 100:
			score += 0.25
		case reasoningLen > 50:
			score += 0.20
		case reasoningLen > 20:
			score += 0.10
		}
	}

	// Factor 3: Alternatives with substantive rejection reasons (uncalibrated: up to 0.20).
	// Only non-selected alternatives with rejection reasons > 20 chars count.
	// This prevents gaming by providing empty alternatives with no explanation.
	substantiveAlts := countSubstantiveRejections(d.Alternatives)
	switch {
	case substantiveAlts >= 3:
		score += 0.20
	case substantiveAlts >= 2:
		score += 0.15
	case substantiveAlts >= 1:
		score += 0.10
	}

	// Factor 4: Evidence provided (uncalibrated: up to 0.15).
	if len(d.Evidence) >= 2 {
		score += 0.15
	} else if len(d.Evidence) >= 1 {
		score += 0.10
	}

	// Factor 5: Decision type is from standard taxonomy (uncalibrated: 0.10).
	if StandardDecisionTypes[d.DecisionType] {
		score += 0.10
	}

	// Factor 6: Outcome is substantive (uncalibrated: 0.05).
	if len(strings.TrimSpace(d.Outcome)) > 20 {
		score += 0.05
	}

	// Factor 7: Precedent reference links this decision to a prior one (uncalibrated: 0.10).
	// This wires the attribution graph so the audit trail shows how decisions evolved.
	if hasPrecedentRef {
		score += 0.10
	}

	return score
}

// countSubstantiveRejections counts non-selected alternatives that have a
// rejection reason longer than 20 characters (after trimming). Selected
// alternatives are excluded — they don't need rejection reasons.
func countSubstantiveRejections(alts []model.TraceAlternative) int {
	count := 0
	for _, alt := range alts {
		if alt.Selected {
			continue
		}
		if alt.RejectionReason != nil && len(strings.TrimSpace(*alt.RejectionReason)) > 20 {
			count++
		}
	}
	return count
}

// ComputeOutcomeScore computes an outcome score (0.0-1.0) from assessment counts.
// Formula: (correct + 0.5 * partially_correct) / total.
// Returns nil when total == 0 (no assessments).
func ComputeOutcomeScore(summary model.AssessmentSummary) *float32 {
	if summary.Total == 0 {
		return nil
	}
	score := float32(float64(summary.Correct)+0.5*float64(summary.PartiallyCorrect)) / float32(summary.Total)
	return &score
}
