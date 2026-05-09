package conflicts

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestMatchTicketRef(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty input", "", ""},
		{"canonical uppercase", "ARD-958", "ARD-958"},
		{"lowercase canonicalised", "ard-958", "ARD-958"},
		{"mixed case canonicalised", "Ard-958", "ARD-958"},
		{"in branch path", "evanvolgas/ard-958-snapshot-errors", "ARD-958"},
		{"in outcome prose", "ARD-958 S2 implemented end-to-end on branch", "ARD-958"},
		{"task with stage suffix returns base ticket", "ARD-958 PR-1", "ARD-958"},
		{"first match wins when multiple present", "ARD-958 then ARD-960 follow-up", "ARD-958"},
		{"three-letter prefix accepted", "ENG-42 ship it", "ENG-42"},
		{"ten-letter prefix accepted", "PROJECTABC-1 ok", "PROJECTABC-1"},
		{"two-letter prefix rejected", "PR-7 only", ""},
		{"http-2 admitted (4-letter prefix passes 3-letter floor — known limitation)", "HTTP-2 spec", "HTTP-2"},
		{"single-digit ticket admitted", "ARD-1 first ticket", "ARD-1"},
		{"adjacent word boundary required (no match when embedded in word)", "noARD-958boundary", ""},
		{"eleven-letter prefix rejected", "ABCDEFGHIJK-1 too long", ""},
		{"underscore neighbours not a boundary in RE2 (no match)", "fix_ARD-958_branch", ""},
		{"slash boundary in path", "/ard-958/", "ARD-958"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, matchTicketRef(tt.input))
		})
	}
}

func TestExtractTicketRef_PrioritySources(t *testing.T) {
	tests := []struct {
		name     string
		decision model.Decision
		expected string
	}{
		{
			name: "task field preferred over branch and outcome",
			decision: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{
						"task":       "ARD-958 PR-1",
						"git_branch": "evanvolgas/ard-960-other",
					},
				},
				Outcome: "Implemented something for ARD-961",
			},
			expected: "ARD-958",
		},
		{
			name: "branch fallback when task absent",
			decision: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{
						"git_branch": "evanvolgas/ard-958-stream-wal",
					},
				},
				Outcome: "Implemented something for ARD-961",
			},
			expected: "ARD-958",
		},
		{
			name: "outcome fallback when task and branch absent",
			decision: model.Decision{
				AgentContext: map[string]any{},
				Outcome:      "ARD-958 S2 implemented end-to-end",
			},
			expected: "ARD-958",
		},
		{
			name: "server namespace task respected (mirrors nestedContextString fallback)",
			decision: model.Decision{
				AgentContext: map[string]any{
					"server": map[string]any{"task": "ARD-958 review"},
				},
			},
			expected: "ARD-958",
		},
		{
			name: "flat agent_context.task respected (legacy layout)",
			decision: model.Decision{
				AgentContext: map[string]any{"task": "ARD-958 review"},
			},
			expected: "ARD-958",
		},
		{
			name: "nil agent_context, no outcome match",
			decision: model.Decision{
				AgentContext: nil,
				Outcome:      "Refactored the storage layer",
			},
			expected: "",
		},
		{
			name: "nothing extractable from any source",
			decision: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "main"},
				},
				Outcome: "Refactored the storage layer",
			},
			expected: "",
		},
		{
			name: "branch with no ticket falls through to outcome match",
			decision: model.Decision{
				AgentContext: map[string]any{
					"client": map[string]any{"git_branch": "feature/caching"},
				},
				Outcome: "ARD-958 implementation",
			},
			expected: "ARD-958",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractTicketRef(tt.decision))
		})
	}
}

func TestIsSameAgentSameTicketRefinement(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	idA := uuid.New()
	idB := uuid.New()

	taskCtx := func(task, branch string) map[string]any {
		client := map[string]any{}
		if task != "" {
			client["task"] = task
		}
		if branch != "" {
			client["git_branch"] = branch
		}
		return map[string]any{"client": client}
	}

	tests := []struct {
		name     string
		d        model.Decision
		cand     model.Decision
		expected bool
	}{
		{
			name: "same agent, same ticket via task field, cross-branch → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958 PR-1", "evanvolgas/ard-958-layer-2"),
				Outcome:      "Implemented validator", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958 PR-2", "evanvolgas/ard-958-layer-3"),
				Outcome:      "Refactored validator with batched embedding", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "same agent, same ticket extracted from branch only → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("", "evanvolgas/ard-958-stream-wal"),
				Outcome:      "Initial WAL streaming impl", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: taskCtx("", "evanvolgas/ard-958-rewrite"),
				Outcome:      "Rewrote streaming layer", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "same agent, same ticket extracted from outcome only → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: nil,
				Outcome:      "ARD-958 S2 implemented", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: nil,
				Outcome:      "ARD-958 follow-up adjustments", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "ticket extracted from outcome on one side, branch on the other → suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("", "evanvolgas/ard-958-stream-wal"),
				Outcome:      "Streaming layer first cut", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: nil,
				Outcome:      "ARD-958 follow-up adjustments", ValidFrom: now.Add(time.Hour),
			},
			expected: true,
		},
		{
			name: "different agents, same ticket → not suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""), Outcome: "Impl", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "reviewer",
				AgentContext: taskCtx("ARD-958", ""), Outcome: "Different take", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "same agent, different tickets → not suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""), Outcome: "Impl", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-960", ""), Outcome: "Other impl", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "same agent, no ticket on either side → not suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: nil, Outcome: "Refactored storage layer", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: nil, Outcome: "Refactored storage layer differently", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "same agent, ticket only on one side → not suppressed (no shared join key)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""), Outcome: "Impl", ValidFrom: now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: nil, Outcome: "Refactored storage layer", ValidFrom: now.Add(time.Hour),
			},
			expected: false,
		},
		{
			name: "same agent, same ticket, but precedent_ref linked → not suppressed (LLM decides)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""), Outcome: "Impl", ValidFrom: now,
				PrecedentRef: &idB,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""), Outcome: "Earlier work", ValidFrom: now.Add(-time.Hour),
			},
			expected: false,
		},
		{
			name: "same agent, same ticket, but later outcome reverses prior choice → not suppressed",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""),
				Outcome:      "Reverted ARD-958 caching strategy; switched to write-through instead",
				ValidFrom:    now.Add(time.Hour),
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""),
				Outcome:      "Chose write-back caching for ARD-958",
				ValidFrom:    now,
			},
			expected: false,
		},
		{
			name: "same agent, same ticket, reversal keyword on the EARLIER outcome → still suppressed (we only inspect later)",
			d: model.Decision{
				ID: idA, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""),
				Outcome:      "Reverted prior approach for ARD-958",
				ValidFrom:    now,
			},
			cand: model.Decision{
				ID: idB, AgentID: "claude-code",
				AgentContext: taskCtx("ARD-958", ""),
				Outcome:      "Refined ARD-958 implementation further",
				ValidFrom:    now.Add(time.Hour),
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isSameAgentSameTicketRefinement(tt.d, tt.cand))
		})
	}
}
