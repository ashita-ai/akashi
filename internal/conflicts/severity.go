package conflicts

import "strings"

// SeverityInput holds the signals needed to compute severity independently of
// the significance score. All fields are optional — the function degrades
// gracefully when signals are missing.
type SeverityInput struct {
	DecisionTypeA string  // decision type of side A
	DecisionTypeB string  // decision type of side B
	ConfidenceA   float32 // decision confidence of side A (0-1); 0 means unknown
	ConfidenceB   float32 // decision confidence of side B (0-1); 0 means unknown
	Category      string  // factual, assessment, strategic, temporal; "" means unknown
}

// decisionTypeTier maps standard decision types to impact tiers (1-4).
// Higher tiers represent decisions with wider blast radius.
var decisionTypeTier = map[string]int{
	"security":        4,
	"architecture":    3,
	"deployment":      3,
	"trade_off":       2,
	"model_selection": 2,
	"data_source":     2,
	"error_handling":  2,
	"code_review":     1,
	"feature_scope":   1,
	"investigation":   1,
	"planning":        1,
	"assessment":      1,
}

// DecisionTypeTierOf returns the severity tier (1-4) for a decision type.
// Unknown types return 1 (lowest tier).
func DecisionTypeTierOf(decisionType string) int {
	if tier, ok := decisionTypeTier[strings.ToLower(decisionType)]; ok {
		return tier
	}
	return 1
}

// ComputeSeverity derives severity from metadata signals that are independent
// of the significance score (which measures detection confidence, not impact).
//
// The function never returns "critical" — that level is reserved for precedent
// escalation and explicit LLM judgment.
//
// Returns one of "high", "medium", "low". Returns "" only if both decision
// types are empty strings (no data to compute from).
func ComputeSeverity(input SeverityInput) string {
	if input.DecisionTypeA == "" && input.DecisionTypeB == "" {
		return ""
	}

	tierA := DecisionTypeTierOf(input.DecisionTypeA)
	tierB := DecisionTypeTierOf(input.DecisionTypeB)
	effectiveTier := tierA
	if tierB > effectiveTier {
		effectiveTier = tierB
	}

	// Base severity from decision type tier.
	sev := tierToBaseSeverity(effectiveTier)

	// Promotion (at most one applies — see ADR-015).
	bothHighConf := input.ConfidenceA >= 0.7 && input.ConfidenceB >= 0.7
	if bothHighConf && effectiveTier >= 3 {
		// Both decisions high confidence on a high-tier type.
		sev = promote(sev)
	} else if strings.ToLower(input.Category) == "factual" && effectiveTier >= 2 {
		// Factual conflicts are harder to reconcile (objective truth
		// disagreement vs. subjective assessment). Applies to tier 2+ only —
		// tier 1 factual conflicts (e.g., two investigation conclusions) are
		// common and not inherently severe.
		sev = promote(sev)
	}

	// Demotion: either side has low confidence (exploratory/uncertain).
	// Zero confidence is treated as unknown (not low) because the lite
	// scorer path has no confidence data at all.
	eitherLowConf := (input.ConfidenceA > 0 && input.ConfidenceA <= 0.3) ||
		(input.ConfidenceB > 0 && input.ConfidenceB <= 0.3)
	if eitherLowConf {
		sev = demote(sev)
	}

	return sev
}

func tierToBaseSeverity(tier int) string {
	switch {
	case tier >= 4:
		return "high"
	case tier >= 2:
		return "medium"
	default:
		return "low"
	}
}

func promote(sev string) string {
	switch sev {
	case "low":
		return "medium"
	case "medium":
		return "high"
	default:
		return sev // "high" stays "high" — never promotes to "critical"
	}
}

func demote(sev string) string {
	switch sev {
	case "high":
		return "medium"
	case "medium":
		return "low"
	default:
		return sev // "low" stays "low"
	}
}
