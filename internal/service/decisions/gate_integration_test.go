//go:build integration

package decisions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/decisions"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/testutil"
)

// newGatedSvc returns a fresh Service with the supplied gate configured.
// We create per-test services so gate state never leaks between tests; the
// shared testSvc keeps its default (off) gate for unrelated suites.
func newGatedSvc(g quality.CompletenessGate) *decisions.Service {
	logger := testutil.TestLogger()
	embedder := embedding.NewNoopProvider(1024)
	svc := decisions.New(testDB, embedder, nil, logger, nil)
	svc.SetCompletenessGate(g)
	return svc
}

// terseDecision returns the kind of trace that scores low against quality.Score:
// short reasoning, no alternatives, no evidence. Under the default factors this
// scores ~0.15 — enough to fail a 0.30 floor but not enough to be inherently
// broken.
func terseDecision() model.TraceDecision {
	short := "small"
	return model.TraceDecision{
		DecisionType: "code_review",
		Outcome:      "approved the change",
		Confidence:   0.5,
		Reasoning:    &short,
	}
}

func TestCompletenessGate_OffMode_AcceptsTerseTrace(t *testing.T) {
	ctx := context.Background()
	svc := newGatedSvc(quality.CompletenessGate{Mode: quality.GateModeOff, Threshold: 0.90})
	agentID := "gate-off-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	result, err := svc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID:  agentID,
		Decision: terseDecision(),
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, result.DecisionID)
	assert.Empty(t, result.Warnings, "off mode must never produce gate warnings")
}

func TestCompletenessGate_RejectMode_BlocksTerseTrace(t *testing.T) {
	ctx := context.Background()
	svc := newGatedSvc(quality.CompletenessGate{Mode: quality.GateModeReject, Threshold: 0.30})
	agentID := "gate-reject-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	_, err := svc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID:  agentID,
		Decision: terseDecision(),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, decisions.ErrCompletenessBelowThreshold),
		"err must wrap ErrCompletenessBelowThreshold so callers can detect it with errors.Is")

	rej := decisions.AsCompletenessRejection(err)
	require.NotNil(t, rej, "AsCompletenessRejection must extract the structured rejection")
	assert.Equal(t, "code_review", rej.DecisionType)
	assert.InDelta(t, 0.30, rej.Threshold, 0.0001)
	assert.Less(t, rej.Score, float32(0.30), "score should be below threshold")
}

func TestCompletenessGate_WarnMode_AcceptsAndWarns(t *testing.T) {
	ctx := context.Background()
	svc := newGatedSvc(quality.CompletenessGate{Mode: quality.GateModeWarn, Threshold: 0.30})
	agentID := "gate-warn-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	result, err := svc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID:  agentID,
		Decision: terseDecision(),
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, result.DecisionID, "warn mode must still persist the decision")
	require.Len(t, result.Warnings, 1, "warn mode must produce exactly one gate warning")
	assert.Contains(t, result.Warnings[0], "code_review")
	assert.Contains(t, result.Warnings[0], "0.30")
}

func TestCompletenessGate_PerTypeOverride_BlocksOnlySpecificType(t *testing.T) {
	ctx := context.Background()
	// Global floor 0; security has a stricter per-type floor. Architecture and
	// code_review must pass freely while security is blocked.
	svc := newGatedSvc(quality.CompletenessGate{
		Mode:      quality.GateModeReject,
		Threshold: 0,
		ByType:    map[string]float32{"security": 0.60},
	})
	agentID := "gate-pertype-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	// Architecture trace with low completeness passes (no global floor).
	archDec := terseDecision()
	archDec.DecisionType = "architecture"
	_, err := svc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID:  agentID,
		Decision: archDec,
	})
	require.NoError(t, err, "architecture must pass — no global floor and no per-type override")

	// Security trace with the same content is blocked.
	secDec := terseDecision()
	secDec.DecisionType = "security"
	_, err = svc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID:  agentID,
		Decision: secDec,
	})
	require.Error(t, err)
	rej := decisions.AsCompletenessRejection(err)
	require.NotNil(t, rej)
	assert.Equal(t, "security", rej.DecisionType)
	assert.InDelta(t, 0.60, rej.Threshold, 0.0001)
}

func TestCompletenessGate_RejectMode_PassesCompleteTrace(t *testing.T) {
	// A trace that meets the bar must pass even in reject mode.
	ctx := context.Background()
	svc := newGatedSvc(quality.CompletenessGate{Mode: quality.GateModeReject, Threshold: 0.50})
	agentID := "gate-passes-" + uuid.New().String()[:8]
	createAgent(t, agentID)

	longReasoning := "We chose Redis over Memcached because we need pub/sub and the operational overhead of running Redis matches existing infrastructure. Memcached lacks pub/sub and would require a second service to be added to the production stack."
	rejectionA := "no pub/sub support, which we need for the notification fan-out"
	rejectionB := "extra ops surface area for a feature subset we already get from Redis"
	_, err := svc.Trace(ctx, uuid.Nil, decisions.TraceInput{
		AgentID: agentID,
		Decision: model.TraceDecision{
			DecisionType: "trade_off",
			Outcome:      "chose Redis over Memcached for cache + pub/sub",
			Confidence:   0.7,
			Reasoning:    &longReasoning,
			Alternatives: []model.TraceAlternative{
				{Label: "Memcached", RejectionReason: &rejectionA},
				{Label: "DragonflyDB", RejectionReason: &rejectionB},
			},
			Evidence: []model.TraceEvidence{
				{SourceType: "document", Content: "Redis is already operated in production for the session store"},
			},
		},
	})
	require.NoError(t, err, "a substantive trace must pass the gate even with threshold=0.50")
}
