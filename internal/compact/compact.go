// Package compact provides shared compaction functions for reducing decision
// and conflict payloads. Used by both the MCP tool layer and the HTTP API
// when callers request format=concise.
package compact

import (
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// MaxCompactReasoning is the character limit for reasoning/outcome fields in compact format.
const MaxCompactReasoning = 200

// Decision returns a minimal representation of a decision.
// Drops internal bookkeeping (content_hash, transaction_time, valid_from/to,
// completeness_score, org_id, run_id, metadata, embedding fields) that agents don't act on.
// Includes consensus scoring and outcome signals when populated.
func Decision(d model.Decision) map[string]any {
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
		m["reasoning"] = Truncate(*d.Reasoning, MaxCompactReasoning)
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
	if task, ok := d.AgentContext["task"]; ok {
		m["task"] = task
	}

	// Consensus weight: [0.5, 1.0]; only include when there's meaningful data.
	total := d.AgreementCount + d.ConflictCount
	if total > 0 {
		cw := 0.5 + 0.5*float64(d.AgreementCount)/float64(max(1, total))
		m["consensus_weight"] = math.Round(cw*1000) / 1000 // 3 decimal places
	}

	// Assessment summary: explicit correctness feedback from agents.
	if d.AssessmentSummary != nil && d.AssessmentSummary.Total > 0 {
		a := d.AssessmentSummary
		m["assessment_summary"] = map[string]any{
			"total":             a.Total,
			"correct":           a.Correct,
			"incorrect":         a.Incorrect,
			"partially_correct": a.PartiallyCorrect,
		}
	}

	// Outcome-based context note (rule-based, not LLM).
	if note := GenerateContextNote(d); note != "" {
		m["context_note"] = note
	}

	return m
}

// GenerateContextNote produces a human-readable signal note for a decision.
// Rules are evaluated in priority order; first match wins. Returns "" when no rule fires.
// Assessment rules take priority since they are explicit feedback, not indirect signals.
func GenerateContextNote(d model.Decision) string {
	vel := d.SupersessionVelocityHours

	// Assessment rules (explicit feedback — highest priority signal).
	if a := d.AssessmentSummary; a != nil && a.Total >= 2 {
		majorityCorrect := a.Correct*2 > a.Total
		majorityIncorrect := a.Incorrect*2 > a.Total
		switch {
		case majorityCorrect:
			return fmt.Sprintf("Assessed correct by %d of %d agent(s).", a.Correct, a.Total)
		case majorityIncorrect:
			return fmt.Sprintf("Assessed incorrect by %d of %d agent(s) — review carefully.", a.Incorrect, a.Total)
		}
	}

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

// Conflict returns a minimal representation of a conflict.
// Drops scoring internals (topic_similarity, outcome_divergence, significance,
// scoring_method, confidence_weight, temporal_decay) and full outcomes/reasoning.
// consensusNote is the optional asymmetry framing string (may be "").
func Conflict(c model.DecisionConflict, consensusNote string) map[string]any {
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
	m["outcome_a"] = Truncate(c.OutcomeA, MaxCompactReasoning)
	m["outcome_b"] = Truncate(c.OutcomeB, MaxCompactReasoning)

	// Winner: which decision prevailed (nil when not set or conflict is unresolved).
	if c.WinningDecisionID != nil {
		m["winning_decision_id"] = c.WinningDecisionID
	}

	// Consensus asymmetry note, when there's a meaningful corroboration imbalance.
	if consensusNote != "" {
		m["consensus_note"] = consensusNote
	}

	// Precedent-aware escalation: flag when this conflict reopens a prior resolution.
	if c.ReopensResolutionID != nil {
		m["reopens_resolution_id"] = c.ReopensResolutionID
	}

	return m
}

// ConflictGroup renders a ConflictGroup for the concise format.
// Shows the group identity (agents, type, count) and key fields from the
// representative conflict so agents understand what the disagreement is about
// without scanning N×M pairwise rows.
func ConflictGroup(g model.ConflictGroup) map[string]any {
	m := map[string]any{
		"id":             g.ID,
		"agent_a":        g.AgentA,
		"agent_b":        g.AgentB,
		"conflict_kind":  g.ConflictKind,
		"decision_type":  g.DecisionType,
		"conflict_count": g.ConflictCount,
		"open_count":     g.OpenCount,
		"first_detected": g.FirstDetectedAt,
		"last_detected":  g.LastDetectedAt,
	}
	if g.TimesReopened > 0 {
		m["times_reopened"] = g.TimesReopened
	}
	if g.GroupTopic != nil {
		m["group_topic"] = *g.GroupTopic
	}
	if g.Representative != nil {
		r := g.Representative
		if r.Severity != nil {
			m["severity"] = *r.Severity
		}
		if r.Category != nil {
			m["category"] = *r.Category
		}
		if r.Explanation != nil && *r.Explanation != "" {
			m["explanation"] = *r.Explanation
		}
		m["outcome_a"] = Truncate(r.OutcomeA, MaxCompactReasoning)
		m["outcome_b"] = Truncate(r.OutcomeB, MaxCompactReasoning)
		m["status"] = r.Status
	}
	return m
}

// SearchResult wraps a search result with its similarity score.
func SearchResult(r model.SearchResult) map[string]any {
	m := Decision(r.Decision)
	m["similarity_score"] = r.SimilarityScore
	return m
}

// BuildConsensusNote returns a consensus framing note for a conflict when one side
// has at least 2 more corroborating decisions than the other. Returns "" otherwise.
func BuildConsensusNote(c model.DecisionConflict, agreementCounts map[[16]byte]int) string {
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
	outcomeA := Truncate(c.OutcomeA, 60)
	outcomeB := Truncate(c.OutcomeB, 60)
	if countA > countB {
		return fmt.Sprintf("Decision A (%s) has %d corroborating decision(s). Decision B (%s) has %d.",
			outcomeA, countA, outcomeB, countB)
	}
	return fmt.Sprintf("Decision B (%s) has %d corroborating decision(s). Decision A (%s) has %d.",
		outcomeB, countB, outcomeA, countA)
}

// GenerateCheckSummary creates a 1-3 sentence human-readable synthesis of check results.
// Template-based, no LLM dependency. Includes consensus framing when material.
func GenerateCheckSummary(decisions []model.Decision, conflicts []model.DecisionConflict) string {
	var parts []string

	// Decision summary.
	if len(decisions) == 0 {
		parts = append(parts, "No prior decisions found.")
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
			Truncate(most.Outcome, 100), most.Confidence*100)

		// Append outcome signal context when non-trivial.
		var signals []string
		if most.SupersessionVelocityHours == nil {
			signals = append(signals, "never superseded")
		}
		if most.PrecedentCitationCount >= 2 {
			signals = append(signals, fmt.Sprintf("cited %d times", most.PrecedentCitationCount))
		}
		if a := most.AssessmentSummary; a != nil && a.Total > 0 {
			signals = append(signals, fmt.Sprintf("assessed correct %d/%d", a.Correct, a.Total))
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
			conflictPart := BuildOpenConflictSummary(open, maxSeverity, decisions, conflicts)
			parts = append(parts, conflictPart)
		case resolvedWithWinner > 0:
			parts = append(parts, fmt.Sprintf("%d conflict(s) resolved with winner declared.", resolvedWithWinner))
		case resolved > 0:
			parts = append(parts, fmt.Sprintf("%d conflict(s) resolved.", resolved))
		}
	}

	return strings.Join(parts, " ")
}

// BuildOpenConflictSummary returns a one-sentence summary for open conflict(s),
// incorporating consensus asymmetry framing when one side has ≥ 2 more corroborating decisions.
func BuildOpenConflictSummary(open int, maxSeverity string, decisions []model.Decision, conflicts []model.DecisionConflict) string {
	base := fmt.Sprintf("%d open conflict(s).", open)
	if maxSeverity != "" {
		base = fmt.Sprintf("%d open conflict(s), highest severity: %s.", open, maxSeverity)
	}

	// Check for consensus asymmetry.
	for _, c := range conflicts {
		if c.Status != "open" && c.Status != "acknowledged" {
			continue
		}
		aCount := DecisionAgreementCount(decisions, c.DecisionAID)
		bCount := DecisionAgreementCount(decisions, c.DecisionBID)
		diff := aCount - bCount
		if diff < 0 {
			diff = -diff
		}
		if diff >= 2 {
			if aCount > bCount {
				return fmt.Sprintf("%d-to-%d in favor of \"%s\".", aCount, bCount, Truncate(c.OutcomeA, 60))
			}
			return fmt.Sprintf("%d-to-%d in favor of \"%s\".", bCount, aCount, Truncate(c.OutcomeB, 60))
		}
	}
	return base
}

// DecisionAgreementCount looks up AgreementCount for a decision ID from a slice.
// Returns 0 when not found (decision not in the check response, or no embedding).
func DecisionAgreementCount(decisions []model.Decision, id uuid.UUID) int {
	for _, d := range decisions {
		if d.ID == id {
			return d.AgreementCount
		}
	}
	return 0
}

// Resolution returns a minimal representation of a ConflictResolution.
// Truncates long outcomes so agents get a clear signal without being buried in text.
func Resolution(r model.ConflictResolution) map[string]any {
	m := map[string]any{
		"decision_type":       r.DecisionType,
		"winning_decision_id": r.WinningDecisionID,
		"winning_agent":       r.WinningAgent,
		"winning_outcome":     Truncate(r.WinningOutcome, MaxCompactReasoning),
		"losing_agent":        r.LosingAgent,
		"losing_outcome":      Truncate(r.LosingOutcome, MaxCompactReasoning),
		"resolved_at":         r.ResolvedAt,
	}
	if r.Explanation != nil && *r.Explanation != "" {
		m["explanation"] = *r.Explanation
	}
	if r.ResolutionNote != nil && *r.ResolutionNote != "" {
		m["resolution_note"] = *r.ResolutionNote
	}
	return m
}

// ActionNeeded returns true if there are open critical/high conflicts.
func ActionNeeded(conflicts []model.DecisionConflict) bool {
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

// Truncate shortens a string to maxLen runes, appending "..." if truncated.
func Truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// CheckResult builds the concise check response map from full decision and conflict data.
// This is shared between the MCP tool layer and the HTTP API format=concise path.
func CheckResult(resp model.CheckResponse, canSuggestPrecedent bool) map[string]any {
	// Build agreement count lookup for consensus note generation.
	agreementCounts := make(map[[16]byte]int, len(resp.Decisions))
	for _, d := range resp.Decisions {
		agreementCounts[[16]byte(d.ID)] = d.AgreementCount
	}

	compactDecs := make([]map[string]any, len(resp.Decisions))
	for i, d := range resp.Decisions {
		compactDecs[i] = Decision(d)
	}
	compactConfs := make([]map[string]any, len(resp.Conflicts))
	for i, c := range resp.Conflicts {
		note := BuildConsensusNote(c, agreementCounts)
		compactConfs[i] = Conflict(c, note)
	}
	compactResolutions := make([]map[string]any, len(resp.PriorResolutions))
	for i, r := range resp.PriorResolutions {
		compactResolutions[i] = Resolution(r)
	}

	summary := GenerateCheckSummary(resp.Decisions, resp.Conflicts)
	if len(resp.PriorResolutions) > 0 {
		summary += fmt.Sprintf(" %d prior conflict(s) for this decision type were formally resolved; winning approach(es) listed in prior_resolutions.", len(resp.PriorResolutions))
	}

	result := map[string]any{
		"has_precedent":     resp.HasPrecedent,
		"summary":           summary,
		"action_needed":     ActionNeeded(resp.Conflicts),
		"relevant_count":    len(resp.Decisions),
		"decisions":         compactDecs,
		"conflicts":         compactConfs,
		"prior_resolutions": compactResolutions,
	}

	// precedent_ref_hint: the UUID of the best candidate for precedent_ref in the
	// subsequent akashi_trace call. We pick the most recent decision with fewer than
	// 5 citations so that well-established precedents aren't over-attributed.
	if canSuggestPrecedent && len(resp.Decisions) > 0 {
		for _, d := range resp.Decisions {
			if d.PrecedentCitationCount < 5 {
				result["precedent_ref_hint"] = d.ID.String()
				break
			}
		}
	}

	return result
}
