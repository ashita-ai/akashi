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

// DefaultStandardDecisionTypes are the canonical types from prompt templates.
// Using standard types improves discoverability and consistency.
var DefaultStandardDecisionTypes = map[string]bool{
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

// BuildStandardTypes returns a standard types set from a custom list.
// If overrides is nil or empty, returns DefaultStandardDecisionTypes.
// Entries are normalized to lowercase and trimmed.
func BuildStandardTypes(overrides []string) map[string]bool {
	if len(overrides) == 0 {
		return DefaultStandardDecisionTypes
	}
	m := make(map[string]bool, len(overrides))
	for _, t := range overrides {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			m[t] = true
		}
	}
	return m
}

// SuggestStandardType returns the closest standard type if the edit distance
// is <= maxDist, or empty string if no close match exists. Exact matches
// (distance 0) are not returned since they need no suggestion.
func SuggestStandardType(input string, standardTypes map[string]bool, maxDist int) string {
	// Short inputs produce false matches: "bug" (3 chars) is within distance 2
	// of "build" (5 chars). Require the input to be long enough that a match
	// represents a plausible typo, not a coincidence.
	if len(input) < maxDist+3 {
		return ""
	}
	best := ""
	bestDist := maxDist + 1
	for std := range standardTypes {
		d := levenshtein(input, std)
		if d > 0 && d < bestDist {
			bestDist = d
			best = std
		}
	}
	return best
}

// levenshtein computes the Levenshtein edit distance between two strings.
// It operates on runes (Unicode code points) so that multi-byte characters
// such as CJK or accented text are counted as single edits.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}

	// Use single-row DP to minimize allocations.
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := range len(ra) {
		curr := make([]int, len(rb)+1)
		curr[0] = i + 1
		for j := range len(rb) {
			cost := 1
			if ra[i] == rb[j] {
				cost = 0
			}
			curr[j+1] = min(
				curr[j]+1,    // insertion
				prev[j+1]+1,  // deletion
				prev[j]+cost, // substitution
			)
		}
		prev = curr
	}
	return prev[len(rb)]
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
//   - Reasoning substantive (>50 chars): up to 0.30
//   - Alternatives with substantive rejection reasons: up to 0.20
//   - Evidence provided: up to 0.15
//   - Outcome substantive (>20 chars): 0.10
//   - Precedent reference set: 0.10
//
// The former "standard decision type" factor (0.10) was removed; its weight
// was redistributed to reasoning (+0.05) and outcome (+0.05).
func Score(d model.TraceDecision, hasPrecedentRef bool) float32 {
	var score float32

	// Factor 1: Confidence is present and reasonable (0.15).
	// Extreme values (exactly 0 or 1) are often defaults, so we reward mid-range.
	// Strict inequality: exactly 0.05 and 0.95 fall to edge tier (0.10).
	score += confidenceFactor(d.Confidence)

	// Factor 2: Reasoning is substantive (up to 0.30).
	score += reasoningFactor(d.Reasoning)

	// Factor 3: Alternatives with substantive rejection reasons (up to 0.20).
	score += alternativesFactor(d.Alternatives)

	// Factor 4: Evidence provided (up to 0.15).
	score += evidenceFactor(d.Evidence)

	// Factor 5: Outcome is substantive but concise (up to 0.10).
	// Concise decision statements (20-300 chars) get full credit.
	// Longer outcomes are penalized — they tend to be change logs, not decisions.
	score += outcomeFactor(d.Outcome)

	// Factor 6: Precedent reference links this decision to a prior one (0.10).
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

// reasoningFactor returns the reasoning contribution (0, 0.12, 0.24, or 0.30).
// Tiers are 40%, 80%, and 100% of the 0.30 weight.
func reasoningFactor(reasoning *string) float32 {
	if reasoning == nil {
		return 0
	}
	reasoningLen := len(strings.TrimSpace(*reasoning))
	switch {
	case reasoningLen > 100:
		return 0.30
	case reasoningLen > 50:
		return 0.24
	case reasoningLen > 20:
		return 0.12
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

// outcomeFactor returns the outcome contribution (0, 0.04, 0.07, or 0.10).
// Concise decision statements (20-300 chars) get full credit. Longer outcomes
// are penalized because they tend to be commit-log rewrites rather than
// decision statements.
func outcomeFactor(outcome string) float32 {
	n := len(strings.TrimSpace(outcome))
	switch {
	case n <= 20:
		return 0
	case n <= 300:
		return 0.10
	case n <= 500:
		return 0.07
	default:
		return 0.04
	}
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

// ConfidenceAdjustment holds the result of AdjustConfidence.
type ConfidenceAdjustment struct {
	Adjusted    float32
	Original    float32
	WasAdjusted bool
	Reasons     []string
}

// AdjustConfidence applies evidence-weighted deflation to self-reported confidence.
// Agents consistently over-report confidence (avg 0.867 across 824 decisions).
// Rather than relying on warnings that agents ignore, this adjusts the stored
// value and preserves the original for auditability.
//
// Rules (applied independently, stacking):
//   - conf >= 0.9 with 0 evidence items → cap at 0.75
//   - conf >= 0.85 with 0 alternatives → reduce by 0.10
//   - conf >= 0.8 with reasoning < 50 chars → reduce by 0.10
//
// The adjusted value is floored at 0.3 to avoid penalizing an otherwise
// reasonable decision into absurdly low confidence.
func AdjustConfidence(confidence float32, evidenceCount, altCount, reasoningLen int) ConfidenceAdjustment {
	original := confidence

	// Only adjust values in valid range. Out-of-range values should be
	// rejected by the HTTP/MCP validation layers; if they reach here,
	// pass them through unchanged so the DB CHECK constraint can catch them.
	if confidence < 0 || confidence > 1 {
		return ConfidenceAdjustment{
			Adjusted:    confidence,
			Original:    original,
			WasAdjusted: false,
		}
	}

	var reasons []string

	if confidence >= 0.9 && evidenceCount == 0 {
		confidence = 0.75
		reasons = append(reasons, "confidence >= 0.9 with no evidence: capped at 0.75")
	}
	if confidence >= 0.85 && altCount == 0 {
		confidence -= 0.10
		reasons = append(reasons, "confidence >= 0.85 with no alternatives: reduced by 0.10")
	}
	if confidence >= 0.8 && reasoningLen < 50 {
		confidence -= 0.10
		reasons = append(reasons, "confidence >= 0.8 with reasoning < 50 chars: reduced by 0.10")
	}

	if confidence < 0.3 {
		confidence = 0.3
	}

	return ConfidenceAdjustment{
		Adjusted:    confidence,
		Original:    original,
		WasAdjusted: confidence != original,
		Reasons:     reasons,
	}
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
