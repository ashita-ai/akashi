// Package quality provides decision trace completeness scoring.
// Completeness scores (0.0-1.0) measure trace completeness at write time —
// whether the agent provided reasoning, alternatives, evidence, etc.
// They do NOT measure whether the decision was correct or adopted.
//
// Scoring is UNIFORM across all decision types. A score of 0.55 means the
// same thing regardless of whether the decision is an investigation or a
// security call. This preserves cross-type comparability and avoids stored
// score discontinuities when profiles change.
//
// Per-type differentiation happens in two places:
//   - Tips (computeMissingFields in mcp/tools.go) are profile-aware: agents
//     only get tips relevant to their decision type.
//   - Health thresholds (TypeExpectation) define per-type acceptable levels.
//     The stats breakdown shows which types fall below their threshold.
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

// CompletenessProfile defines per-decision-type expectations for tip filtering.
// Profiles control which completeness tips are surfaced to agents — they do NOT
// affect the completeness score itself (scoring is uniform).
type CompletenessProfile struct {
	// MinEvidence is the minimum number of evidence items expected.
	// When 0, evidence tips are suppressed for this type.
	MinEvidence int `json:"min_evidence"`

	// AlternativesExpected controls whether alternatives tips are surfaced.
	// When false, agents for this type don't get "add alternatives" tips.
	AlternativesExpected bool `json:"alternatives_expected"`

	// MaxConfidenceNoEvidence is the confidence threshold above which a
	// tip warns that confidence seems high for a decision without evidence.
	// Set to 1.0 to disable the warning (default behavior).
	MaxConfidenceNoEvidence float32 `json:"max_confidence_no_evidence"`
}

// DefaultProfile is the fallback for decision types not in DefaultProfiles.
var DefaultProfile = CompletenessProfile{
	MinEvidence:             1,
	AlternativesExpected:    true,
	MaxConfidenceNoEvidence: 1.0,
}

// DefaultProfiles defines per-type expectations based on the nature of each
// decision type. These drive tip filtering, not scoring.
var DefaultProfiles = map[string]CompletenessProfile{
	"investigation": {MinEvidence: 0, AlternativesExpected: false, MaxConfidenceNoEvidence: 0.9},
	"planning":      {MinEvidence: 0, AlternativesExpected: false, MaxConfidenceNoEvidence: 0.85},
	"code_review":   {MinEvidence: 1, AlternativesExpected: true, MaxConfidenceNoEvidence: 0.85},
	"architecture":  {MinEvidence: 2, AlternativesExpected: true, MaxConfidenceNoEvidence: 0.80},
	"security":      {MinEvidence: 2, AlternativesExpected: true, MaxConfidenceNoEvidence: 0.75},
}

// ProfileFor returns the completeness profile for a decision type. It checks
// custom overrides first, then built-in defaults, then falls back to
// DefaultProfile. The overrides parameter may be nil.
func ProfileFor(decisionType string, overrides map[string]CompletenessProfile) CompletenessProfile {
	if overrides != nil {
		if p, ok := overrides[decisionType]; ok {
			return p
		}
	}
	if p, ok := DefaultProfiles[decisionType]; ok {
		return p
	}
	return DefaultProfile
}

// TypeExpectation defines the acceptable completeness level for a decision type.
// Used by the health stats enrichment to flag types that fall below expectations.
type TypeExpectation struct {
	// ExpectedMin is the minimum acceptable average completeness for this type.
	// When a type's actual average falls below this, its status is "needs_attention".
	ExpectedMin float64
}

// DefaultExpectations defines per-type health thresholds. These are applied to
// the completeness_by_type stats to surface which types are underperforming
// relative to what matters for that type.
//
// The thresholds reflect realistic expectations:
//   - investigation/planning: reasoning + type + outcome = 0.40 when all present;
//     0.30 acknowledges most traces won't have full reasoning.
//   - code_review: should include reasoning + some alts or evidence; 0.45.
//   - architecture/trade_off: should include reasoning + alts + evidence; 0.55.
//   - security: highest bar — should have all documentation; 0.60.
var DefaultExpectations = map[string]TypeExpectation{
	"investigation": {ExpectedMin: 0.30},
	"planning":      {ExpectedMin: 0.30},
	"assessment":    {ExpectedMin: 0.30},
	"code_review":   {ExpectedMin: 0.45},
	"architecture":  {ExpectedMin: 0.55},
	"trade_off":     {ExpectedMin: 0.55},
	"security":      {ExpectedMin: 0.60},
}

// ExpectationFor returns the type expectation for a decision type.
// Returns a default expectation of 0.40 for unknown types.
func ExpectationFor(decisionType string) TypeExpectation {
	if e, ok := DefaultExpectations[decisionType]; ok {
		return e
	}
	return TypeExpectation{ExpectedMin: 0.40}
}

// Score computes a completeness score (0.0-1.0) for a trace decision.
// Higher scores indicate more complete traces (more fields populated).
//
// Scoring is UNIFORM — every decision type uses the same weights. Per-type
// differentiation happens via tips (profile-aware) and health thresholds
// (TypeExpectation), not via the score itself.
//
// All weights below are uncalibrated — chosen by hand without empirical basis.
// See issue #252 (Tier 2) for the plan to fit weights against assessed data.
//
// Scoring factors:
//   - Confidence present and reasonable (0.05-0.95): 0.15
//   - Reasoning substantive (>50 chars): up to 0.25
//   - Alternatives with substantive rejection reasons: up to 0.20
//   - Evidence provided: up to 0.15
//   - Standard decision type: 0.10
//   - Outcome substantive (>20 chars): 0.05
//   - Precedent reference set: 0.10
func Score(d model.TraceDecision, hasPrecedentRef bool) float32 {
	var score float32

	// Factor 1: Confidence is present and reasonable (0.15).
	// Extreme values (exactly 0 or 1) are often defaults, so we reward mid-range.
	// Strict inequality: exactly 0.05 and 0.95 fall to edge tier (0.10).
	score += confidenceFactor(d.Confidence)

	// Factor 2: Reasoning is substantive (up to 0.25).
	score += reasoningFactor(d.Reasoning)

	// Factor 3: Alternatives with substantive rejection reasons (up to 0.20).
	score += alternativesFactor(d.Alternatives)

	// Factor 4: Evidence provided (up to 0.15).
	score += evidenceFactor(d.Evidence)

	// Factor 5: Decision type is from standard taxonomy (0.10).
	if StandardDecisionTypes[d.DecisionType] {
		score += 0.10
	}

	// Factor 6: Outcome is substantive (0.05).
	if len(strings.TrimSpace(d.Outcome)) > 20 {
		score += 0.05
	}

	// Factor 7: Precedent reference links this decision to a prior one (0.10).
	if hasPrecedentRef {
		score += 0.10
	}

	return score
}

// confidenceFactor returns the confidence contribution (0, 0.10, or 0.15).
func confidenceFactor(confidence float32) float32 {
	if confidence > 0.05 && confidence < 0.95 {
		return 0.15
	}
	if confidence > 0 && confidence < 1 {
		return 0.10
	}
	return 0
}

// reasoningFactor returns the reasoning contribution (0, 0.10, 0.20, or 0.25).
func reasoningFactor(reasoning *string) float32 {
	if reasoning == nil {
		return 0
	}
	reasoningLen := len(strings.TrimSpace(*reasoning))
	switch {
	case reasoningLen > 100:
		return 0.25
	case reasoningLen > 50:
		return 0.20
	case reasoningLen > 20:
		return 0.10
	}
	return 0
}

// alternativesFactor returns the alternatives contribution (0, 0.10, 0.15, or 0.20).
func alternativesFactor(alts []model.TraceAlternative) float32 {
	substantiveAlts := countSubstantiveRejections(alts)
	switch {
	case substantiveAlts >= 3:
		return 0.20
	case substantiveAlts >= 2:
		return 0.15
	case substantiveAlts >= 1:
		return 0.10
	}
	return 0
}

// evidenceFactor returns the evidence contribution (0, 0.10, or 0.15).
func evidenceFactor(evidence []model.TraceEvidence) float32 {
	if len(evidence) >= 2 {
		return 0.15
	}
	if len(evidence) >= 1 {
		return 0.10
	}
	return 0
}

// countSubstantiveRejections counts alternatives that have a
// rejection reason longer than 20 characters (after trimming).
func countSubstantiveRejections(alts []model.TraceAlternative) int {
	count := 0
	for _, alt := range alts {
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
