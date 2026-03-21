// Package quality provides decision trace completeness scoring.
// Completeness scores (0.0-1.0) measure trace completeness at write time —
// whether the agent provided reasoning, alternatives, evidence, etc.
// They do NOT measure whether the decision was correct or adopted.
//
// Scoring is profile-aware: different decision types have different
// expectations (e.g., investigations don't need alternatives; security
// decisions require more evidence). When a factor is not expected for a
// decision type, its weight is redistributed to reasoning.
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

// CompletenessProfile defines per-decision-type expectations for completeness
// scoring. When a factor is not expected (e.g., alternatives for investigations),
// its weight is redistributed to reasoning — reflecting that the value of a
// trace depends on the decision type.
type CompletenessProfile struct {
	// MinEvidence is the minimum number of evidence items expected.
	// When 0, evidence is not expected and its weight (0.15) is redistributed
	// to reasoning. When >= 1, the standard evidence tiers apply.
	MinEvidence int `json:"min_evidence"`

	// AlternativesExpected controls whether the alternatives factor (0.20)
	// contributes to the score. When false, the weight is redistributed
	// to reasoning.
	AlternativesExpected bool `json:"alternatives_expected"`

	// MaxConfidenceNoEvidence caps the confidence factor when no evidence is
	// provided. If confidence exceeds this threshold and evidence count is 0,
	// the confidence factor is reduced from mid-range (0.15) to edge (0.10).
	// Set to 1.0 to disable the penalty (default behavior).
	MaxConfidenceNoEvidence float32 `json:"max_confidence_no_evidence"`
}

// DefaultProfile is the fallback for decision types not in DefaultProfiles.
// It preserves the original uniform scoring behavior.
var DefaultProfile = CompletenessProfile{
	MinEvidence:             1,
	AlternativesExpected:    true,
	MaxConfidenceNoEvidence: 1.0,
}

// DefaultProfiles defines per-type expectations based on the nature of each
// decision type. High-stakes decisions (architecture, security) require more
// evidence and alternatives; exploratory decisions (investigation, planning)
// emphasize reasoning instead.
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

// Base factor weights (uncalibrated). These are the maximum contribution of
// each factor before profile-based redistribution.
const (
	weightConfidence  float32 = 0.15
	weightReasoning   float32 = 0.25
	weightAlternative float32 = 0.20
	weightEvidence    float32 = 0.15
	weightType        float32 = 0.10
	weightOutcome     float32 = 0.05
	weightPrecedent   float32 = 0.10
)

// Score computes a completeness score (0.0-1.0) for a trace decision using the
// built-in default profile for the decision's type. Use ScoreWithProfile for
// custom profile overrides.
//
// Scoring factors and their base weights (uncalibrated):
//   - Confidence present and reasonable (0.05-0.95): 0.15
//   - Reasoning substantive (>50 chars): up to 0.25 (+ redistributed weight)
//   - Alternatives with substantive rejection reasons: up to 0.20
//   - Evidence provided: up to 0.15
//   - Standard decision type: 0.10
//   - Outcome substantive (>20 chars): 0.05
//   - Precedent reference set: 0.10
//
// When a profile marks alternatives or evidence as not expected, the weight
// is redistributed to reasoning. This means investigation/planning decisions
// can score high through thorough reasoning alone, while architecture/security
// decisions must also provide alternatives and evidence.
func Score(d model.TraceDecision, hasPrecedentRef bool) float32 {
	return ScoreWithProfile(d, hasPrecedentRef, ProfileFor(d.DecisionType, nil))
}

// ScoreWithProfile computes a completeness score using the provided profile.
func ScoreWithProfile(d model.TraceDecision, hasPrecedentRef bool, profile CompletenessProfile) float32 {
	var score float32

	// Compute effective reasoning weight: base + any redistributed weight
	// from factors that this profile does not expect.
	reasoningMax := weightReasoning
	if !profile.AlternativesExpected {
		reasoningMax += weightAlternative
	}
	if profile.MinEvidence == 0 {
		reasoningMax += weightEvidence
	}

	// Factor 1: Confidence is present and reasonable (base: 0.15).
	// Extreme values (exactly 0 or 1) are often defaults, so we reward mid-range.
	// Strict inequality: exactly 0.05 and 0.95 fall to edge tier (0.10).
	confidenceScore := confidenceFactor(d.Confidence)
	// Profile penalty: high confidence without evidence is penalized for
	// decision types that should have evidence to back up strong convictions.
	if len(d.Evidence) == 0 && d.Confidence > profile.MaxConfidenceNoEvidence {
		confidenceScore = 0.10 // cap at edge tier
	}
	score += confidenceScore

	// Factor 2: Reasoning is substantive (base: 0.25, may be higher with redistribution).
	score += reasoningFactor(d.Reasoning, reasoningMax)

	// Factor 3: Alternatives with substantive rejection reasons (base: 0.20).
	// Skipped when the profile says alternatives are not expected.
	if profile.AlternativesExpected {
		score += alternativesFactor(d.Alternatives)
	}

	// Factor 4: Evidence provided (base: 0.15).
	// Skipped when the profile sets MinEvidence to 0.
	if profile.MinEvidence > 0 {
		score += evidenceFactor(d.Evidence)
	}

	// Factor 5: Decision type is from standard taxonomy (0.10).
	if StandardDecisionTypes[d.DecisionType] {
		score += weightType
	}

	// Factor 6: Outcome is substantive (0.05).
	if len(strings.TrimSpace(d.Outcome)) > 20 {
		score += weightOutcome
	}

	// Factor 7: Precedent reference links this decision to a prior one (0.10).
	if hasPrecedentRef {
		score += weightPrecedent
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

// reasoningFactor returns the reasoning contribution, scaled to reasoningMax.
// The tier ratios (100%, 80%, 40%) match the original 0.25/0.20/0.10 proportions.
func reasoningFactor(reasoning *string, reasoningMax float32) float32 {
	if reasoning == nil {
		return 0
	}
	reasoningLen := len(strings.TrimSpace(*reasoning))
	switch {
	case reasoningLen > 100:
		return reasoningMax
	case reasoningLen > 50:
		return reasoningMax * 0.80
	case reasoningLen > 20:
		return reasoningMax * 0.40
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
