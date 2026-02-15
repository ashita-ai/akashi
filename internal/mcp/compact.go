package mcp

import (
	"fmt"
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

const maxCompactReasoning = 200

// compactDecision returns a minimal representation of a decision for MCP responses.
// Drops internal bookkeeping (content_hash, transaction_time, valid_from/to,
// quality_score, org_id, run_id, metadata, embedding fields) that agents don't act on.
func compactDecision(d model.Decision) map[string]any {
	m := map[string]any{
		"id":            d.ID,
		"agent_id":      d.AgentID,
		"decision_type": d.DecisionType,
		"outcome":       d.Outcome,
		"confidence":    d.Confidence,
		"created_at":    d.CreatedAt,
	}
	if d.Reasoning != nil && *d.Reasoning != "" {
		r := *d.Reasoning
		if len(r) > maxCompactReasoning {
			r = r[:maxCompactReasoning] + "..."
		}
		m["reasoning"] = r
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
	return m
}

// compactConflict returns a minimal representation of a conflict for MCP responses.
// Drops scoring internals (topic_similarity, outcome_divergence, significance,
// scoring_method, confidence_weight, temporal_decay) and full outcomes/reasoning.
func compactConflict(c model.DecisionConflict) map[string]any {
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
	return m
}

// compactSearchResult wraps a search result with its similarity score.
func compactSearchResult(r model.SearchResult) map[string]any {
	m := compactDecision(r.Decision)
	m["similarity_score"] = r.SimilarityScore
	return m
}

// generateCheckSummary creates a 1-3 sentence human-readable synthesis of check results.
// Template-based, no LLM dependency.
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

		// Most recent decision.
		most := decisions[0] // decisions come sorted by valid_from desc
		parts = append(parts, fmt.Sprintf("Most recent: \"%s\" (%.0f%% confidence).",
			truncate(most.Outcome, 100), most.Confidence*100))
	}

	// Conflict summary.
	if len(conflicts) > 0 {
		open := 0
		var maxSeverity string
		severityRank := map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}
		maxRank := 0
		for _, c := range conflicts {
			if c.Status == "open" || c.Status == "acknowledged" {
				open++
				if c.Severity != nil {
					if r := severityRank[*c.Severity]; r > maxRank {
						maxRank = r
						maxSeverity = *c.Severity
					}
				}
			}
		}
		if open > 0 {
			if maxSeverity != "" {
				parts = append(parts, fmt.Sprintf("%d open conflict(s), highest severity: %s.", open, maxSeverity))
			} else {
				parts = append(parts, fmt.Sprintf("%d open conflict(s).", open))
			}
		}
	}

	return strings.Join(parts, " ")
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
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
