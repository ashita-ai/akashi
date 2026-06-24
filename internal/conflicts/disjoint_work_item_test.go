//go:build !lite

package conflicts

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// clientCtx builds an agent_context with a client-namespaced pr_number, matching
// the structured layout nestedContextString reads first.
func clientCtx(prNumber string) map[string]any {
	return map[string]any{"client": map[string]any{"pr_number": prNumber}}
}

func TestCanonicalPRRef(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"1539", "PR-1539"},
		{"#1539", "PR-1539"},
		{"PR-1539", "PR-1539"},
		{"pr #1543", "PR-1543"},
		{"", ""},
		{"none", ""},
		{"v2", ""}, // single digit run below the 2-digit floor
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, canonicalPRRef(tt.in))
		})
	}
}

// TestExtractWorkItemRefs verifies the union extractor surfaces both ticket
// references and PR/issue references, prefers the structured pr_number, and does
// not over-match the dense numeric vocabulary of cutover decision outcomes
// (migration timestamps, test counts, LSNs).
func TestExtractWorkItemRefs(t *testing.T) {
	tests := []struct {
		name string
		dec  model.Decision
		want []string
	}{
		{
			name: "bare PR ref in outcome",
			dec:  model.Decision{Outcome: "Recommended not merging PR #1539 until evidence is fresh"},
			want: []string{"PR-1539"},
		},
		{
			name: "ticket and PR together",
			dec:  model.Decision{Outcome: "Reviewed ARD-1660 cutover evidence in PR #1539"},
			want: []string{"ARD-1660", "PR-1539"},
		},
		{
			name: "multiple PRs, deduped",
			dec:  model.Decision{Outcome: "closed #1543 in favor of #1545 and #1545 again"},
			want: []string{"PR-1543", "PR-1545"},
		},
		{
			name: "structured pr_number with no # in outcome",
			dec: model.Decision{
				Outcome:      "Fixed the Greptile cutover findings on the engine-flip read",
				AgentContext: clientCtx("1531"),
			},
			want: []string{"PR-1531"},
		},
		{
			name: "structured pr_number plus a different PR in text",
			dec: model.Decision{
				Outcome:      "Rebased onto #1542 after fixing findings",
				AgentContext: clientCtx("1543"),
			},
			want: []string{"PR-1543", "PR-1542"},
		},
		{
			name: "dense numeric prose does not over-match",
			dec:  model.Decision{Outcome: "573 unit tests pass; migration 20260623000001; target_checkpoint_lsn advanced; 8 findings"},
			want: nil,
		},
		{
			name: "single-digit and hex-like tokens are ignored",
			dec:  model.Decision{Outcome: "finding #3 in palette #1a2b3c remains open"},
			want: nil,
		},
		{
			name: "nothing extractable",
			dec:  model.Decision{Outcome: "kept the freeze fail-closed and added a bounded retry"},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want, extractWorkItemRefs(tt.dec))
		})
	}
}

// TestIsDisjointWorkItem exercises the structural escalation of the
// disjoint-ticket validator prompt hint. The table covers each guardrail plus
// anchor cases drawn verbatim-in-spirit from the 2026-06-23 open-conflict queue,
// where 9 of 19 open false positives were review/planning pairs about different
// PRs/tickets sharing dense cutover vocabulary, and a single review of "PR #1539"
// fanned out into nine of them.
func TestIsDisjointWorkItem(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()
	mono := "mono"
	other := "other"

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			// Group 8 anchor: the hub review carries only a PR number, not an
			// ARD ticket — a ticket-only rule could never have matched it. Also
			// proves "replaced the boolean" (a code-diff verb) no longer blocks
			// suppression now that the supersession-keyword guard is gone here.
			name: "PR-only review vs PR-only review, disjoint → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "coder", Project: &mono, DecisionType: "code_review",
				Outcome: "Fixed PR #1531 Greptile/Cursor cutover findings: replaced the engine_flip_attempted boolean with a confirmed connector read",
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono, DecisionType: "code_review",
				Outcome: "Recommended not merging PR #1539 until cutover evidence uses a fresh live replica-identity proof",
			},
			expected: true,
		},
		{
			// Group 7c anchor: ticket-scoped review vs PR-scoped review.
			name: "ticket review vs PR review, disjoint → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed ARD-1662 commit 50fbd79b 'Fix Debezium cutover restore audit attribution'. Verdict: APPROVE",
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono, DecisionType: "code_review",
				Outcome: "Recommended not merging PR #1539 until cutover evidence uses a fresh live replica-identity proof",
			},
			expected: true,
		},
		{
			// Group 4 anchor: two plans for different tickets.
			name: "planning vs planning, disjoint tickets → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "codex-mcp-client", Project: &mono, DecisionType: "planning",
				Outcome: "ARD-1606 implementation should build on existing mainline contract/table/gate pieces rather than redesigning cutover",
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code", Project: &mono, DecisionType: "planning",
				Outcome: "Set up the ARD-1662 cutover workflow on its own branch with the durable evidence reader wired in",
			},
			expected: true,
		},
		{
			name: "structured pr_number (no # in outcome) vs PR in text, disjoint → suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome:      "Fixed the Greptile cutover findings on the engine-flip read path",
				AgentContext: clientCtx("1531"),
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed the cutover evidence wiring in PR #1539; gate consumption is fail-closed",
			},
			expected: true,
		},
		{
			name: "decision_type case-insensitive → suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "Code_Review",
				Outcome: "Reviewed PR #1529 retirement contract fixes",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "PLANNING",
				Outcome: "Planned the ARD-1660 evidence collector follow-up",
			},
			expected: true,
		},
		{
			// Group 1 anchor: architecture is direction-setting — a genuine
			// cross-cutting design fork can span tickets and must reach the LLM.
			name: "architecture vs architecture, disjoint → NOT suppressed (design forks preserved)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "architecture",
				Outcome: "Implemented ARD-1693 pgstream Init as a privilege-safe idempotent startup path",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "architecture",
				Outcome: "ARD-1599: cannot wire install_ardent_ddl_capture into the live pgstream setup path — payload-incompatibility blocker",
			},
			expected: false,
		},
		{
			// Group 6a anchor: trade_off is direction-setting.
			name: "trade_off vs code_review, disjoint → NOT suppressed (direction-setting type)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "trade_off",
				Outcome: "Addressed Greptile's ARD-1690 concerns by requiring a fresh proof on the live evidence path",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed PR #1539 cutover-proof reader wiring",
			},
			expected: false,
		},
		{
			// Group 7i anchor: the merge-order plan names #1539, so the sets
			// overlap and the genuinely-about-the-same-PR pair reaches the LLM.
			name: "shared PR ref (overlap) → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome: "Recommended not merging PR #1539 until evidence is fresh",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "planning",
				Outcome: "Recommended merge order: land #1542 first, then merge #1539 before #1543",
			},
			expected: false,
		},
		{
			name: "shared ticket ref (overlap) → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed ARD-1660 evidence readers; gate is fail-closed",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "planning",
				Outcome: "Planned the remaining ARD-1660 live-collection producer work",
			},
			expected: false,
		},
		{
			// Group 7a anchor: the review text names DATALOSS/quarantine. Even
			// across tickets, a data-safety pair must reach the validator — this
			// is the evidenced cross-ticket data-safety contradiction class.
			name: "data-loss vocabulary on one side → NOT suppressed (data-safety guard)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed ARD-1661 wiring: no persistence of apply/quarantine/DATALOSS findings to debezium_migration_events",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "code_review",
				Outcome: "Recommended not merging PR #1539 until evidence is fresh",
			},
			expected: false,
		},
		{
			// Data-safety guard parity: the work-item ref (PR #1539) and the
			// DATALOSS keyword both live in the task field. extractWorkItemRefs
			// mines the task, so the guard must too — otherwise this disjoint pair
			// (PR #1539 vs PR #1543) is suppressed despite a data-safety signal.
			// (taskCtx is shared from disjoint_resource_test.go.)
			name: "data-loss keyword only in task → NOT suppressed (guard scans task)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome:      "Review complete; recommendations posted inline",
				AgentContext: taskCtx("DATALOSS audit of PR #1539 evidence wiring"),
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "code_review",
				Outcome: "Recommended not merging PR #1543 until evidence is fresh",
			},
			expected: false,
		},
		{
			name: "one side has no extractable work item → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed the cutover freeze path; kept it fail-closed with a bounded retry",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "code_review",
				Outcome: "Recommended not merging PR #1539 until evidence is fresh",
			},
			expected: false,
		},
		{
			name: "different project → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed PR #1531 cutover findings",
			},
			cand: model.Decision{
				ID: idB, Project: &other, DecisionType: "code_review",
				Outcome: "Reviewed PR #1539 evidence wiring",
			},
			expected: false,
		},
		{
			name: "both projects untagged (nil) → NOT suppressed",
			d: model.Decision{
				ID: idA, DecisionType: "code_review",
				Outcome: "Reviewed PR #1531 cutover findings",
			},
			cand: model.Decision{
				ID: idB, DecisionType: "code_review",
				Outcome: "Reviewed PR #1539 evidence wiring",
			},
			expected: false,
		},
		{
			name: "precedent-linked → NOT suppressed (explicit lineage → let LLM decide)",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "code_review",
				Outcome:      "Reviewed PR #1531 cutover findings",
				PrecedentRef: &idB,
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed PR #1539 evidence wiring",
			},
			expected: false,
		},
		{
			// A declared cross-work-item supersession names the other item, so
			// the sets overlap and it reaches the LLM (then the supersession→
			// suggestion path) — no keyword guard needed in this filter.
			name: "cross-reference by name (overlap) → NOT suppressed",
			d: model.Decision{
				ID: idA, Project: &mono, DecisionType: "planning",
				Outcome: "closed PR #1543 in favor of #1545 which already carries the spine",
			},
			cand: model.Decision{
				ID: idB, Project: &mono, DecisionType: "code_review",
				Outcome: "Reviewed #1545; the shadow-apply spine landed cleanly",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isDisjointWorkItem(tt.d, tt.cand),
				"isDisjointWorkItem(%q vs %q)", tt.d.DecisionType, tt.cand.DecisionType)
			// Symmetric: argument order must not change the verdict.
			assert.Equal(t, tt.expected, isDisjointWorkItem(tt.cand, tt.d),
				"isDisjointWorkItem should be symmetric for %q", tt.name)
		})
	}
}
