package conflicts

import (
	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// Tier classifies a conflict for auto-resolution routing.
type Tier int

const (
	// TierP0 — reopens a prior resolution. Never auto-resolve.
	TierP0 Tier = iota
	// TierP1 — both sides high confidence + critical/high severity. Requires human.
	TierP1
	// TierP2 — standard LLM-validated contradiction. Auto-resolve after N days.
	TierP2
	// TierP3 — low significance or supersession. Auto-resolve after min(policy, 7) days.
	TierP3
)

// ClassifyTier determines the auto-resolution tier for a conflict.
func ClassifyTier(c model.DecisionConflict) Tier {
	// P0: conflict that has been previously resolved (reopened).
	if c.ResolvedBy != nil {
		return TierP0
	}

	severity := ""
	if c.Severity != nil {
		severity = *c.Severity
	}

	// P1: both sides confidence > 0.8 AND severity is critical or high.
	if c.ConfidenceA > 0.8 && c.ConfidenceB > 0.8 &&
		(severity == "critical" || severity == "high") {
		return TierP1
	}

	// P3: low significance or supersession relationship.
	if c.Significance != nil && *c.Significance < 0.30 {
		return TierP3
	}
	if c.Relationship != nil && *c.Relationship == "supersession" {
		return TierP3
	}

	// P2: everything else.
	return TierP2
}

// ShouldAutoResolve checks whether a conflict should be auto-resolved given the policy.
func ShouldAutoResolve(c model.DecisionConflict, policy model.ConflictResolutionPolicy) bool {
	tier := ClassifyTier(c)

	switch tier {
	case TierP0:
		return false // Never auto-resolve reopened conflicts.
	case TierP1:
		return false // Requires human resolution.
	case TierP2, TierP3:
		// Check severity against policy.
		if c.Severity != nil {
			sev := *c.Severity
			// Severity must be at or below max.
			if model.SeverityRank(sev) > model.SeverityRank(policy.AutoResolveMaxSeverity) {
				return false
			}
			// Check never-resolve list.
			for _, never := range policy.NeverAutoResolveSeverities {
				if sev == never {
					return false
				}
			}
		}
		return true
	}
	return false
}

// DetermineWinner selects the winning decision based on the policy strategy
// and the recommendation signals.
func DetermineWinner(c model.DecisionConflict, policy model.ConflictResolutionPolicy, rec *model.Recommendation) *uuid.UUID {
	switch policy.AutoResolveWinner {
	case model.WinnerRecency:
		if c.DecidedAtB.After(c.DecidedAtA) {
			return &c.DecisionBID
		}
		return &c.DecisionAID

	case model.WinnerConfidence:
		if c.ConfidenceB > c.ConfidenceA {
			return &c.DecisionBID
		}
		if c.ConfidenceA > c.ConfidenceB {
			return &c.DecisionAID
		}
		// Tie: fall through to recommendation.
		if rec != nil {
			w := rec.SuggestedWinner
			return &w
		}
		return nil

	case model.WinnerConsensus:
		if rec != nil {
			w := rec.SuggestedWinner
			return &w
		}
		// No recommendation available: fall back to recency.
		if c.DecidedAtB.After(c.DecidedAtA) {
			return &c.DecisionBID
		}
		return &c.DecisionAID
	}

	// Unknown strategy: use recommendation if available.
	if rec != nil {
		w := rec.SuggestedWinner
		return &w
	}
	return nil
}

// String returns the human-readable tier name.
func (t Tier) String() string {
	switch t {
	case TierP0:
		return "P0"
	case TierP1:
		return "P1"
	case TierP2:
		return "P2"
	case TierP3:
		return "P3"
	default:
		return "unknown"
	}
}
