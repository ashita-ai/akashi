package mcp

import (
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

const maxCompactReasoning = 200

// compactDecision returns a minimal representation of a decision for MCP responses.
// Drops internal bookkeeping (content_hash, transaction_time, valid_from/to,
// completeness_score, quality_score, org_id, run_id, metadata, embedding fields) that agents don't act on.
// Includes consensus scoring and outcome signals when populated.
func compactDecision(d model.Decision) map[string]any {
	m := map[string]any{
		"id":              d.ID,
		"agent_id":        d.AgentID,
		"decision_type":   d.DecisionType,
		"outcome":         d.Outcome,
		"confidence":      d.Confidence,
		"created_at":      d.CreatedAt,
		"agreement_count": d.AgreementCount,
		"conflict_count":  d.ConflictCount,
	}
	if d.Reasoning != nil && *d.Reasoning != "" {
		m["reasoning"] = truncate(*d.Reasoning, maxCompactReasoning)
	}
	if d.SessionID != nil {
		m["session_id"] = d.SessionID
	}
	if tool, ok := d.AgentContext["tool"]; ok {
		m["tool"] = tool
	}
	if mdl, ok := d.AgentContext["model"]; ok {
		m["model"] = mdl
	}

	// Consensus weight: [0.5, 1.0]; only include when there's meaningful data.
	total := d.AgreementCount + d.ConflictCount
	if total > 0 {
		cw := 0.5 + 0.5*float64(d.AgreementCount)/float64(max(1, total))
		m["consensus_weight"] = math.Round(cw*1000) / 1000 // 3 decimal places
	}

	// Outcome-based context note (rule-based, not LLM).
	if note := generateContextNote(d); note != "" {
		m["context_note"] = note
	}

	return m
}

// generateContextNote produces a human-readable signal note for a decision.
// Rules are evaluated in priority order; first match wins. Returns "" when no rule fires.
func generateContextNote(d model.Decision) string {
	vel := d.SupersessionVelocityHours

	switch {
	case vel != nil && *vel < 48 && d.PrecedentCitationCount == 0:
		return fmt.Sprintf("Revised within %.0fh and never cited as precedent — treat with caution.", *vel)

	case vel == nil && d.PrecedentCitationCount >= 2:
		return fmt.Sprintf("Never superseded. Cited as precedent %d times.", d.PrecedentCitationCount)

	case vel == nil && d.ConflictFate.Won >= 1:
		return fmt.Sprintf("Never superseded. Won %d conflict resolution(s).", d.ConflictFate.Won)

	case vel != nil && *vel > 720: // > 30 days
		days := int(math.Round(*vel / 24))
		return fmt.Sprintf("Stood for %d days before revision.", days)

	case d.ConflictFate.Lost >= 1 && d.ConflictFate.Won == 0:
		return fmt.Sprintf("Overridden in %d conflict resolution(s).", d.ConflictFate.Lost)
	}
	return ""
}

// compactConflict returns a minimal representation of a conflict for MCP responses.
// Drops scoring internals (topic_similarity, outcome_divergence, significance,
// scoring_method, confidence_weight, temporal_decay) and full outcomes/reasoning.
// consensusNote is the optional asymmetry framing string (may be "").
func compactConflict(c model.DecisionConflict, consensusNote string) map[string]any {
	m := map[string]any{
		"id":          c.ID,
		"agent_a":     c.AgentA,
		"agent_b":     c.AgentB,
		"status":      c.Status,
		"detected_at": c.DetectedAt,
	}
	if c.Category != nil {
		m["category"] = *c.Category
	}
	if c.Severity != nil {
		m["severity"] = *c.Severity
	}
	if c.Explanation != nil && *c.Explanation != "" {
		m["explanation"] = *c.Explanation
	}
	// Include brief outcome summaries so agents understand what the conflict is about.
	m["outcome_a"] = truncate(c.OutcomeA, maxCompactReasoning)
	m["outcome_b"] = truncate(c.OutcomeB, maxCompactReasoning)

	// Winner: which decision prevailed (nil when not set or conflict is unresolved).
	if c.WinningDecisionID != nil {
		m["winning_decision_id"] = c.WinningDecisionID
	}

	// Consensus asymmetry note, when there's a meaningful corroboration imbalance.
	if consensusNote != "" {
		m["consensus_note"] = consensusNote
	}

	return m
}

// compactSearchResult wraps a search result with its similarity score.
func compactSearchResult(r model.SearchResult) map[string]any {
	m := compactDecision(r.Decision)
	m["similarity_score"] = r.SimilarityScore
	return m
}

// buildConsensusNote returns a consensus framing note for a conflict when one side
// has at least 2 more corroborating decisions than the other. Returns "" otherwise.
func buildConsensusNote(c model.DecisionConflict, agreementCounts map[[16]byte]int) string {
	aID := [16]byte(c.DecisionAID)
	bID := [16]byte(c.DecisionBID)
	countA := agreementCounts[aID]
	countB := agreementCounts[bID]
	diff := countA - countB
	if diff < 0 {
		diff = -diff
	}
	if diff < 2 {
		return ""
	}
	// Determine which side has more corroboration.
	outcomeA := truncate(c.OutcomeA, 60)
	outcomeB := truncate(c.OutcomeB, 60)
	if countA > countB {
		return fmt.Sprintf("Decision A (%s) has %d corroborating decision(s). Decision B (%s) has %d.",
			outcomeA, countA, outcomeB, countB)
	}
	return fmt.Sprintf("Decision B (%s) has %d corroborating decision(s). Decision A (%s) has %d.",
		outcomeB, countB, outcomeA, countA)
}

// generateCheckSummary creates a 1-3 sentence human-readable synthesis of check results.
// Template-based, no LLM dependency. Includes consensus framing when material.
func generateCheckSummary(decisions []model.Decision, conflicts []model.DecisionConflict) string {
	var parts []string

	// Decision summary.
	if len(decisions) == 0 {
		parts = append(parts, "No prior decisions found for this type.")
	} else {
		types := map[string]int{}
		for _, d := range decisions {
			types[d.DecisionType]++
		}

		if len(types) == 1 {
			parts = append(parts, fmt.Sprintf("%d prior decision(s) found.", len(decisions)))
		} else {
			parts = append(parts, fmt.Sprintf("%d prior decisions across %d types.", len(decisions), len(types)))
		}

		// Most recent decision with outcome signals appended when material.
		most := decisions[0] // decisions come sorted by valid_from desc
		summaryLine := fmt.Sprintf("Most recent: \"%s\" (%.0f%% confidence",
			truncate(most.Outcome, 100), most.Confidence*100)

		// Append outcome signal context when non-trivial.
		var signals []string
		if most.SupersessionVelocityHours == nil {
			signals = append(signals, "never superseded")
		}
		if most.PrecedentCitationCount >= 2 {
			signals = append(signals, fmt.Sprintf("cited %d times", most.PrecedentCitationCount))
		}
		if len(signals) > 0 {
			summaryLine += ", " + strings.Join(signals, ", ")
		}
		summaryLine += ")."
		parts = append(parts, summaryLine)
	}

	// Conflict summary with winner and consensus framing.
	if len(conflicts) > 0 {
		open := 0
		var maxSeverity string
		severityRank := map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}
		maxRank := 0
		resolved := 0
		resolvedWithWinner := 0

		for _, c := range conflicts {
			switch c.Status {
			case "open", "acknowledged":
				open++
				if c.Severity != nil {
					if r := severityRank[*c.Severity]; r > maxRank {
						maxRank = r
						maxSeverity = *c.Severity
					}
				}
			case "resolved", "wont_fix":
				resolved++
				if c.WinningDecisionID != nil {
					resolvedWithWinner++
				}
			}
		}

		switch {
		case open > 0:
			conflictPart := buildOpenConflictSummary(open, maxSeverity, decisions, conflicts)
			parts = append(parts, conflictPart)
		case resolvedWithWinner > 0:
			parts = append(parts, fmt.Sprintf("%d conflict(s) resolved with winner declared.", resolvedWithWinner))
		case resolved > 0:
			parts = append(parts, fmt.Sprintf("%d conflict(s) resolved.", resolved))
		}
	}

	return strings.Join(parts, " ")
}

// buildOpenConflictSummary returns a one-sentence summary for open conflict(s),
// incorporating consensus asymmetry framing when one side has ≥ 2 more corroborating decisions.
func buildOpenConflictSummary(open int, maxSeverity string, decisions []model.Decision, conflicts []model.DecisionConflict) string {
	base := fmt.Sprintf("%d open conflict(s).", open)
	if maxSeverity != "" {
		base = fmt.Sprintf("%d open conflict(s), highest severity: %s.", open, maxSeverity)
	}

	// Check for consensus asymmetry.
	for _, c := range conflicts {
		if c.Status != "open" && c.Status != "acknowledged" {
			continue
		}
		aCount := decisionAgreementCount(decisions, c.DecisionAID)
		bCount := decisionAgreementCount(decisions, c.DecisionBID)
		diff := aCount - bCount
		if diff < 0 {
			diff = -diff
		}
		if diff >= 2 {
			if aCount > bCount {
				return fmt.Sprintf("%d-to-%d in favor of \"%s\".", aCount, bCount, truncate(c.OutcomeA, 60))
			}
			return fmt.Sprintf("%d-to-%d in favor of \"%s\".", bCount, aCount, truncate(c.OutcomeB, 60))
		}
	}
	return base
}

// decisionAgreementCount looks up AgreementCount for a decision ID from a slice.
// Returns 0 when not found (decision not in the check response, or no embedding).
func decisionAgreementCount(decisions []model.Decision, id uuid.UUID) int {
	for _, d := range decisions {
		if d.ID == id {
			return d.AgreementCount
		}
	}
	return 0
}

// actionNeeded returns true if there are open critical/high conflicts.
func actionNeeded(conflicts []model.DecisionConflict) bool {
	for _, c := range conflicts {
		if c.Status != "open" && c.Status != "acknowledged" {
			continue
		}
		if c.Severity != nil && (*c.Severity == "critical" || *c.Severity == "high") {
			return true
		}
	}
	return false
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
