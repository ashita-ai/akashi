package conflicts

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// TestIsCrossAgentPrecedentRefinement exercises the cross-agent sibling of
// isSameAgentSameTicketRefinement. The filter is intentionally narrower than
// the same-agent variant: it requires an EXPLICIT precedent_ref between the
// pair (no inference from shared metadata), so the table covers each
// guardrail in isolation.
//
// Anchor cases drawn from the 2026-05-22 open conflict queue:
//   - ARD-1173 (suppressed): codex's audit-trigger refinement cites claude's
//     plan via precedent_ref, same ticket, no supersession keyword.
//   - ARD-1168 (NOT suppressed): no direct precedent_ref between the two
//     agents — preserves the genuine disagreement that resolved with codex
//     winning at 18:39 today.
func TestIsCrossAgentPrecedentRefinement(t *testing.T) {
	now := time.Date(2026, 5, 22, 4, 36, 0, 0, time.UTC)
	idA := uuid.New()
	idB := uuid.New()
	mono := "mono"
	other := "other"

	taskCtx := func(task string) map[string]any {
		return map[string]any{"client": map[string]any{"task": task}}
	}

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			// ARD-1173 anchor: codex 41b52b01 → claude cb9ca435 ("Builds on ...").
			name: "cross-agent same ticket, B precedent_ref → A, no supersession kw → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173: implementation plan"),
				Outcome:      "ARD-1173 plan: add api_keys.last_used_at column + index + debounced RPC",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: taskCtx("fix ARD-1173 review findings"),
				Outcome:      "Fixed ARD-1173 review findings using an api_keys-specific audit trigger",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: true,
		},
		{
			name: "cross-agent same ticket, A precedent_ref → B (reverse direction), no supersession kw → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173 follow-up"),
				Outcome:      "ARD-1173 follow-up: completed the audit-trigger refinement",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idB,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: taskCtx("ARD-1173 initial"),
				Outcome:      "ARD-1173 initial: api_keys.last_used_at column + RPC",
				ValidFrom:    now,
			},
			expected: true,
		},
		{
			// ARD-1168 anchor: real disagreement, no precedent_ref between them.
			name: "cross-agent same ticket but no precedent link → NOT suppressed (preserves real disagreements)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1168 shared env fallback"),
				Outcome:      "ARD-1168: broad ardent_owned fallback for GET /v1/environments",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: taskCtx("ARD-1168 narrower fallback"),
				Outcome:      "ARD-1168: narrow env_ardent-cloud-only fallback",
				ValidFrom:    now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "cross-agent same ticket, precedent linked, later outcome has supersession kw → NOT suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "ARD-1173 plan",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: taskCtx("ARD-1173 revisit"),
				Outcome:      "Reverted ARD-1173 audit-trigger approach; switched to last_used_only table",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: false,
		},
		{
			name: "cross-agent same ticket, precedent linked, supersession kw on EARLIER outcome only → suppressed (only later is inspected)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "Reverted a prior unrelated ARD-1173 approach",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: taskCtx("ARD-1173 refinement"),
				Outcome:      "Refined ARD-1173 with audit-trigger approach",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: true,
		},
		{
			name: "cross-agent, precedent linked, different tickets → NOT suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "ARD-1173 plan",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: taskCtx("ARD-1174"),
				Outcome:      "ARD-1174 implementation referencing the ARD-1173 architecture",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: false,
		},
		{
			name: "cross-agent, precedent linked, no ticket on either side → NOT suppressed (no shared join key)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: nil,
				Outcome:      "Refactored storage layer",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: nil,
				Outcome:      "Refactored storage layer more",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: false,
		},
		{
			name: "cross-agent, precedent linked, ticket only on one side → NOT suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "ARD-1173 plan",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: nil,
				Outcome:      "Refactored storage layer",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: false,
		},
		{
			name: "cross-agent, precedent linked, same ticket, DIFFERENT projects → NOT suppressed (defense)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "ARD-1173 plan",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &other,
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "ARD-1173 in other project",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: false,
		},
		{
			name: "SAME agent, same ticket, precedent linked → NOT suppressed here (same-agent path handles it)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "ARD-1173 plan",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code", Project: &mono,
				AgentContext: taskCtx("ARD-1173 refinement"),
				Outcome:      "ARD-1173 refined",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: false,
		},
		{
			name: "cross-agent, same ticket from outcome regex on both sides, precedent linked → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code", Project: &mono,
				AgentContext: nil,
				Outcome:      "ARD-958 S2 implemented",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client", Project: &mono,
				AgentContext: nil,
				Outcome:      "ARD-958 follow-up extending S2",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: true,
		},
		{
			name: "cross-agent, precedent linked but no project set on either side (nil == nil) → suppressed when ticket matches",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "ARD-1173 plan",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "codex-mcp-client",
				AgentContext: taskCtx("ARD-1173"),
				Outcome:      "ARD-1173 refinement",
				ValidFrom:    now.Add(time.Hour),
				PrecedentRef: &idA,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isCrossAgentPrecedentRefinement(tt.d, tt.cand))
		})
	}
}
