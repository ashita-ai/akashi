package quality

import (
	"testing"

	"github.com/ashita-ai/kyoyu/internal/model"
)

func TestScore(t *testing.T) {
	ptr := func(s string) *string { return &s }
	ptrf := func(f float32) *float32 { return &f }

	tests := []struct {
		name     string
		decision model.TraceDecision
		minScore float32
		maxScore float32
	}{
		{
			name:     "empty trace",
			decision: model.TraceDecision{},
			minScore: 0.0,
			maxScore: 0.05,
		},
		{
			name: "minimal trace",
			decision: model.TraceDecision{
				DecisionType: "custom_type",
				Outcome:      "short",
				Confidence:   0.5,
			},
			minScore: 0.10, // just confidence
			maxScore: 0.20,
		},
		{
			name: "good trace with standard type",
			decision: model.TraceDecision{
				DecisionType: "model_selection",
				Outcome:      "chose gpt-4o for summarization tasks",
				Confidence:   0.85,
				Reasoning:    ptr("GPT-4o offers the best balance of quality and cost for this use case, based on benchmark results."),
				Alternatives: []model.TraceAlternative{
					{Label: "gpt-4o", Selected: true},
					{Label: "claude-3-haiku", Selected: false, RejectionReason: ptr("Lower quality on summarization benchmarks")},
				},
			},
			minScore: 0.65,
			maxScore: 0.85,
		},
		{
			name: "excellent trace",
			decision: model.TraceDecision{
				DecisionType: "architecture",
				Outcome:      "chose Redis with 5-minute TTL for session caching",
				Confidence:   0.92,
				Reasoning:    ptr("Redis provides the lowest latency for our expected QPS. A 5-minute TTL balances freshness with cache hit rate based on our session duration analysis."),
				Alternatives: []model.TraceAlternative{
					{Label: "Redis", Selected: true, Score: ptrf(0.95)},
					{Label: "Memcached", Selected: false, Score: ptrf(0.80), RejectionReason: ptr("No native TTL per-key, would require custom eviction logic")},
					{Label: "In-memory map", Selected: false, Score: ptrf(0.60), RejectionReason: ptr("Not shared across instances, breaks horizontal scaling")},
				},
				Evidence: []model.TraceEvidence{
					{SourceType: "benchmark", Content: "Redis p99 latency: 2ms at 10k QPS"},
					{SourceType: "document", Content: "Session duration analysis shows 95th percentile at 4 minutes"},
				},
			},
			minScore: 0.90,
			maxScore: 1.00,
		},
		{
			name: "trace with extreme confidence",
			decision: model.TraceDecision{
				DecisionType: "security",
				Outcome:      "enabled TLS 1.3",
				Confidence:   1.0, // extreme, gets partial credit
				Reasoning:    ptr("TLS 1.3 is the industry standard for secure connections."),
			},
			minScore: 0.30,
			maxScore: 0.50,
		},
		{
			name: "trace with zero confidence",
			decision: model.TraceDecision{
				DecisionType: "deployment",
				Outcome:      "deploying to us-east-1",
				Confidence:   0.0,
				Reasoning:    ptr("No strong preference, defaulting to primary region."),
			},
			minScore: 0.25,
			maxScore: 0.45,
		},
		{
			name: "trace with short reasoning",
			decision: model.TraceDecision{
				DecisionType: "model_selection",
				Outcome:      "chose claude-3-haiku",
				Confidence:   0.7,
				Reasoning:    ptr("fast"), // too short
			},
			minScore: 0.20,
			maxScore: 0.35,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := Score(tt.decision)
			if score < tt.minScore || score > tt.maxScore {
				t.Errorf("Score() = %v, want between %v and %v", score, tt.minScore, tt.maxScore)
			}
		})
	}
}

func TestStandardDecisionTypes(t *testing.T) {
	expected := []string{
		"model_selection",
		"architecture",
		"data_source",
		"error_handling",
		"feature_scope",
		"trade_off",
		"deployment",
		"security",
	}

	for _, dt := range expected {
		if !StandardDecisionTypes[dt] {
			t.Errorf("expected %q to be a standard decision type", dt)
		}
	}

	if StandardDecisionTypes["not_a_type"] {
		t.Error("expected 'not_a_type' to not be a standard decision type")
	}
}
