package mcp

// This file delegates all compaction functions to the shared internal/compact package.
// The MCP layer calls these wrappers; the HTTP API can import internal/compact directly.

import (
	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/compact"
	"github.com/ashita-ai/akashi/internal/model"
)

const maxCompactReasoning = compact.MaxCompactReasoning

func compactDecision(d model.Decision) map[string]any {
	return compact.Decision(d)
}

func compactConflict(c model.DecisionConflict, consensusNote string) map[string]any {
	return compact.Conflict(c, consensusNote)
}

func compactConflictGroup(g model.ConflictGroup) map[string]any {
	return compact.ConflictGroup(g)
}

func compactSearchResult(r model.SearchResult) map[string]any {
	return compact.SearchResult(r)
}

func buildConsensusNote(c model.DecisionConflict, agreementCounts map[[16]byte]int) string {
	return compact.BuildConsensusNote(c, agreementCounts)
}

func generateCheckSummary(decisions []model.Decision, conflicts []model.DecisionConflict) string {
	return compact.GenerateCheckSummary(decisions, conflicts)
}

func actionNeeded(conflicts []model.DecisionConflict) bool {
	return compact.ActionNeeded(conflicts)
}

func compactResolution(r model.ConflictResolution) map[string]any {
	return compact.Resolution(r)
}

func generateContextNote(d model.Decision) string {
	return compact.GenerateContextNote(d)
}

func buildOpenConflictSummary(open int, maxSeverity string, decisions []model.Decision, conflicts []model.DecisionConflict) string {
	return compact.BuildOpenConflictSummary(open, maxSeverity, decisions, conflicts)
}

func decisionAgreementCount(decisions []model.Decision, id uuid.UUID) int {
	return compact.DecisionAgreementCount(decisions, id)
}

func truncate(s string, maxLen int) string {
	return compact.Truncate(s, maxLen)
}
