package conflicts

import "time"

// This file holds symbols that are shared between the cloud scorer (scorer.go,
// build tag !lite) and the lite-compiled members of the package — validator.go
// and ticket_extract.go, which carry no build tag and so are compiled in BOTH
// the cloud binary and the SQLite/MCP lite binary (cmd/akashi-local, built with
// -tags lite per ADR-009).
//
// They must live in an untagged file: when scorer.go is excluded from the lite
// build, any symbol it defines disappears, so a definition referenced by the
// always-compiled validator.go/ticket_extract.go would leave the lite build
// undefined. Keeping the cross-boundary vocabulary here is the membership-parity
// fix — every shared consumer and the cloud scorer resolve the same definition
// in every build configuration.

// nestedContextString extracts a string value from the agent_context JSONB map,
// checking namespaced locations in priority order: client.<key>, server.<key>,
// then flat <key>. This mirrors the SQL COALESCE used by generated attribution
// columns (migration 048) and handles both namespaced (MCP/HTTP handler) and
// legacy flat agent_context layouts.
//
// Shared: used by the cloud scorer's structural filters and by
// ticket_extract.go (lite-compiled) to mine ticket references from context.
func nestedContextString(ctx map[string]any, key string) string {
	if ctx == nil {
		return ""
	}
	// Check client namespace first (self-reported values like commit_sha, pr_number).
	if client, ok := ctx["client"].(map[string]any); ok {
		if s, ok := client[key].(string); ok && s != "" {
			return s
		}
	}
	// Check server namespace (server-extracted values).
	if server, ok := ctx["server"].(map[string]any); ok {
		if s, ok := server[key].(string); ok && s != "" {
			return s
		}
	}
	// Fall back to flat key (legacy layout).
	if s, ok := ctx[key].(string); ok {
		return s
	}
	return ""
}

// reviewTypes are decision types that represent analysis/review/investigation work.
//
// Shared: keyed by the cloud scorer's isTemporalReassessment /
// isDirectionalWorkflowPair and by validator.go (lite-compiled) when it gates
// the temporal-reassessment hint.
var reviewTypes = map[string]bool{
	"code_review":   true,
	"assessment":    true,
	"investigation": true,
	"review":        true,
	"analysis":      true,
	"audit":         true,
}

// temporalReassessmentWindow is the minimum time delta between two
// review-type decisions on the same project for the pair to be treated as a
// re-measurement rather than a contradiction. Quantitative observations
// (FP rates, scores, percentages) are time-bound — a snapshot taken weeks
// later does not contradict an earlier snapshot just because the numbers
// moved. 7 days matches the SessionStart "recent" window used elsewhere
// for conflict-relevance filtering.
//
// Shared: applied by the cloud scorer's isTemporalReassessment and referenced
// by validator.go (lite-compiled).
const temporalReassessmentWindow = 7 * 24 * time.Hour
