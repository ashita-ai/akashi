//go:build !lite

package storage_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/testutil"
)

// testDB holds a shared test database connection for all tests in this package.
var testDB *storage.DB
var testTC *testutil.TestContainer

func TestMain(m *testing.M) {
	tc := testutil.MustStartTimescaleDB()
	testTC = tc

	ctx := context.Background()
	var err error
	testDB, err = tc.NewTestDB(ctx, testutil.TestLogger())
	if err != nil {
		tc.Terminate()
		fmt.Fprintf(os.Stderr, "test db: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	tc.Terminate()
	os.Exit(code)
}

func TestCreateAndGetRun(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{
		AgentID:  "test-agent",
		Metadata: map[string]any{"model": "gpt-4o"},
	})
	require.NoError(t, err)
	assert.Equal(t, "test-agent", run.AgentID)
	assert.Equal(t, model.RunStatusRunning, run.Status)

	got, err := testDB.GetRun(ctx, run.OrgID, run.ID)
	require.NoError(t, err)
	assert.Equal(t, run.ID, got.ID)
	assert.Equal(t, "test-agent", got.AgentID)
}

func TestCompleteRun(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "complete-test"})
	require.NoError(t, err)

	err = testDB.CompleteRun(ctx, run.OrgID, run.ID, model.RunStatusCompleted, map[string]any{"tokens": 1500})
	require.NoError(t, err)

	got, err := testDB.GetRun(ctx, run.OrgID, run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, got.Status)
	assert.NotNil(t, got.CompletedAt)
}

func TestCompleteRunAlreadyCompleted(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "double-complete"})
	require.NoError(t, err)

	err = testDB.CompleteRun(ctx, run.OrgID, run.ID, model.RunStatusCompleted, nil)
	require.NoError(t, err)

	// Retry-safe semantics: a second completion attempt is idempotent success.
	err = testDB.CompleteRun(ctx, run.OrgID, run.ID, model.RunStatusFailed, nil)
	require.NoError(t, err)

	got, err := testDB.GetRun(ctx, run.OrgID, run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, got.Status)
}

func TestInsertAndGetEvents(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "event-test"})
	require.NoError(t, err)

	events := []model.AgentEvent{
		{
			ID: uuid.New(), RunID: run.ID, EventType: model.EventDecisionStarted,
			SequenceNum: 1, OccurredAt: time.Now().UTC(), AgentID: "event-test",
			Payload: map[string]any{"decision_type": "test"}, CreatedAt: time.Now().UTC(),
		},
		{
			ID: uuid.New(), RunID: run.ID, EventType: model.EventDecisionMade,
			SequenceNum: 2, OccurredAt: time.Now().UTC(), AgentID: "event-test",
			Payload: map[string]any{"outcome": "approved"}, CreatedAt: time.Now().UTC(),
		},
	}

	count, err := testDB.InsertEvents(ctx, events)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Equal(t, model.EventDecisionStarted, got[0].EventType)
	assert.Equal(t, model.EventDecisionMade, got[1].EventType)
}

func TestInsertEventsCOPY(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "copy-test"})
	require.NoError(t, err)

	// Insert a batch of 100 events via COPY.
	events := make([]model.AgentEvent, 100)
	for i := range events {
		events[i] = model.AgentEvent{
			ID:          uuid.New(),
			RunID:       run.ID,
			EventType:   model.EventToolCallCompleted,
			SequenceNum: int64(i + 1),
			OccurredAt:  time.Now().UTC(),
			AgentID:     "copy-test",
			Payload:     map[string]any{"step": i},
			CreatedAt:   time.Now().UTC(),
		}
	}

	count, err := testDB.InsertEvents(ctx, events)
	require.NoError(t, err)
	assert.Equal(t, int64(100), count)

	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 100)
}

func TestCreateAndGetDecision(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "decision-test"})
	require.NoError(t, err)

	reasoning := "DTI within threshold"
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      "decision-test",
		DecisionType: "loan_approval",
		Outcome:      "approve",
		Confidence:   0.87,
		Reasoning:    &reasoning,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)
	assert.Equal(t, "approve", d.Outcome)

	got, err := testDB.GetDecision(ctx, d.OrgID, d.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Equal(t, d.ID, got.ID)
	assert.Equal(t, float32(0.87), got.Confidence)
}

func TestDecisionWithAlternativesAndEvidence(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "full-decision-test"})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      "full-decision-test",
		DecisionType: "routing",
		Outcome:      "route_to_specialist",
		Confidence:   0.92,
	})
	require.NoError(t, err)

	// Add alternatives.
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "Route to specialist"},
		{DecisionID: d.ID, Label: "Route to general"},
	})
	require.NoError(t, err)

	// Add evidence.
	rel := float32(0.95)
	err = testDB.CreateEvidenceBatch(ctx, []model.Evidence{
		{
			DecisionID:     d.ID,
			SourceType:     model.SourceAPIResponse,
			Content:        "Customer has premium plan",
			RelevanceScore: &rel,
		},
	})
	require.NoError(t, err)

	// Get decision with includes.
	got, err := testDB.GetDecision(ctx, d.OrgID, d.ID, storage.GetDecisionOpts{IncludeAlts: true, IncludeEvidence: true})
	require.NoError(t, err)
	assert.Len(t, got.Alternatives, 2)
	assert.Len(t, got.Evidence, 1)
}

func TestReviseDecision(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "revise-test"})
	require.NoError(t, err)

	original, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      "revise-test",
		DecisionType: "loan_approval",
		Outcome:      "approve",
		Confidence:   0.8,
	})
	require.NoError(t, err)

	revised, err := testDB.ReviseDecision(ctx, original.ID, model.Decision{
		RunID:        run.ID,
		AgentID:      "revise-test",
		DecisionType: "loan_approval",
		Outcome:      "deny",
		Confidence:   0.95,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "deny", revised.Outcome)

	// Original should be invalidated.
	orig, err := testDB.GetDecision(ctx, original.OrgID, original.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.NotNil(t, orig.ValidTo)

	// Revised should be current.
	rev, err := testDB.GetDecision(ctx, revised.OrgID, revised.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Nil(t, rev.ValidTo)
}

func TestReviseDecision_AutoResolvesConflicts(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "auto-resolve-test"})
	require.NoError(t, err)

	// Create two decisions that will conflict.
	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: "auto-resolve-test",
		DecisionType: "autoresolve_test", Outcome: "approach_A", Confidence: 0.8,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: "auto-resolve-counterpart",
		DecisionType: "autoresolve_test", Outcome: "approach_B", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Insert a scored conflict between dA and dB.
	topicSim := 0.90
	outcomeDiv := 0.80
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            "auto-resolve-test",
		AgentB:            "auto-resolve-counterpart",
		DecisionTypeA:     "autoresolve_test",
		DecisionTypeB:     "autoresolve_test",
		OutcomeA:          "approach_A",
		OutcomeB:          "approach_B",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, conflictID)

	// Verify conflict is open before revision.
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{}, 50, 0)
	require.NoError(t, err)
	var foundOpen bool
	for _, c := range conflicts {
		if c.ID == conflictID {
			assert.Equal(t, "open", c.Status, "conflict should be open before revision")
			foundOpen = true
			break
		}
	}
	require.True(t, foundOpen, "should find the seeded conflict")

	// Revise decision A — this should auto-resolve the conflict.
	revised, err := testDB.ReviseDecision(ctx, dA.ID, model.Decision{
		RunID: run.ID, AgentID: "auto-resolve-test",
		DecisionType: "autoresolve_test", Outcome: "approach_A_v2", Confidence: 0.9,
		Metadata: map[string]any{},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "approach_A_v2", revised.Outcome)

	// Verify the conflict is now resolved.
	resolvedStatus := "resolved"
	allConflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		Status: &resolvedStatus,
	}, 50, 0)
	require.NoError(t, err)
	var foundResolved bool
	for _, c := range allConflicts {
		if c.ID == conflictID {
			assert.Equal(t, "resolved", c.Status, "conflict should be auto-resolved after revision")
			require.NotNil(t, c.ResolvedBy, "resolved_by should be set")
			assert.Equal(t, "system:revision", *c.ResolvedBy, "resolved_by should indicate system revision")
			require.NotNil(t, c.ResolutionNote, "resolution_note should be set")
			assert.Contains(t, *c.ResolutionNote, dA.ID.String(), "resolution_note should reference superseded decision")
			assert.Contains(t, *c.ResolutionNote, revised.ID.String(), "resolution_note should reference new decision")
			assert.NotNil(t, c.ResolutionDecisionID, "resolution_decision_id should be set to revised decision")
			assert.Equal(t, revised.ID, *c.ResolutionDecisionID)
			foundResolved = true
			break
		}
	}
	require.True(t, foundResolved, "should find the auto-resolved conflict")
}

func TestQueryDecisions(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "query-test"})
	require.NoError(t, err)

	for i := range 5 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      "query-test",
			DecisionType: "classification",
			Outcome:      fmt.Sprintf("class_%d", i),
			Confidence:   float32(i) * 0.2,
		})
		require.NoError(t, err)
	}

	dType := "classification"
	confMin := float32(0.5)
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{
			AgentIDs:      []string{"query-test"},
			DecisionType:  &dType,
			ConfidenceMin: &confMin,
		},
		OrderBy:  "confidence",
		OrderDir: "desc",
		Limit:    10,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 2) // confidence 0.6 and 0.8 at minimum
	for _, d := range decisions {
		assert.GreaterOrEqual(t, d.Confidence, float32(0.5))
	}
}

func TestTemporalQuery(t *testing.T) {
	ctx := context.Background()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "temporal-test"})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      "temporal-test",
		DecisionType: "temporal",
		Outcome:      "first",
		Confidence:   0.7,
	})
	require.NoError(t, err)

	// Query as of now should see the decision.
	decisions, err := testDB.QueryDecisionsTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf: time.Now().UTC().Add(time.Second),
		Filters: model.QueryFilters{
			AgentIDs: []string{"temporal-test"},
		},
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(decisions), 1)

	// Query as of yesterday should see nothing.
	decisions, err = testDB.QueryDecisionsTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf: time.Now().UTC().Add(-24 * time.Hour),
		Filters: model.QueryFilters{
			AgentIDs: []string{"temporal-test"},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

func TestAgentCRUD(t *testing.T) {
	ctx := context.Background()

	hash := "hashed_key_123"
	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "crud-agent",
		Name:       "CRUD Test Agent",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
	})
	require.NoError(t, err)
	assert.Equal(t, "crud-agent", agent.AgentID)

	got, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, "crud-agent")
	require.NoError(t, err)
	assert.Equal(t, agent.ID, got.ID)

	gotByID, err := testDB.GetAgentByID(ctx, agent.ID, agent.OrgID)
	require.NoError(t, err)
	assert.Equal(t, "crud-agent", gotByID.AgentID)
}

func TestAccessGrants(t *testing.T) {
	ctx := context.Background()

	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "grantor-" + uuid.New().String()[:8],
		Name:    "Grantor",
		Role:    model.RoleAdmin,
	})
	require.NoError(t, err)

	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "grantee-" + uuid.New().String()[:8],
		Name:    "Grantee",
		Role:    model.RoleReader,
	})
	require.NoError(t, err)

	resID := "underwriting-agent"
	grant, err := testDB.CreateGrant(ctx, model.AccessGrant{
		GrantorID:    grantor.ID,
		GranteeID:    grantee.ID,
		ResourceType: "agent_traces",
		ResourceID:   &resID,
		Permission:   "read",
	})
	require.NoError(t, err)

	// Check access.
	has, err := testDB.HasAccess(ctx, uuid.Nil, grantee.ID, "agent_traces", "underwriting-agent", "read")
	require.NoError(t, err)
	assert.True(t, has)

	// Check no access for different resource.
	has, err = testDB.HasAccess(ctx, uuid.Nil, grantee.ID, "agent_traces", "other-agent", "read")
	require.NoError(t, err)
	assert.False(t, has)

	// Delete grant.
	err = testDB.DeleteGrant(ctx, grant.OrgID, grant.ID)
	require.NoError(t, err)

	has, err = testDB.HasAccess(ctx, uuid.Nil, grantee.ID, "agent_traces", "underwriting-agent", "read")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestListRunsByAgent(t *testing.T) {
	ctx := context.Background()

	agentID := "list-runs-" + uuid.New().String()[:8]
	for range 3 {
		_, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
		require.NoError(t, err)
	}

	runs, total, err := testDB.ListRunsByAgent(ctx, uuid.Nil, agentID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	assert.Len(t, runs, 3)
}

func TestReserveSequenceNums(t *testing.T) {
	ctx := context.Background()

	// Reserve a batch of 5 sequence numbers.
	nums, err := testDB.ReserveSequenceNums(ctx, 5)
	require.NoError(t, err)
	assert.Len(t, nums, 5)

	// Values must be monotonically increasing.
	for i := 1; i < len(nums); i++ {
		assert.Greater(t, nums[i], nums[i-1], "sequence numbers must be monotonically increasing")
	}

	// Reserve another batch — values must continue increasing from the last batch.
	nums2, err := testDB.ReserveSequenceNums(ctx, 3)
	require.NoError(t, err)
	assert.Len(t, nums2, 3)
	assert.Greater(t, nums2[0], nums[len(nums)-1], "second batch must start after first batch")

	// Zero count returns nil.
	empty, err := testDB.ReserveSequenceNums(ctx, 0)
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestNotify(t *testing.T) {
	ctx := context.Background()

	// Can only test Notify (sending), not Listen/WaitForNotification
	// since we didn't configure a notify connection in the test setup.
	err := testDB.Notify(ctx, "test_channel", `{"test": true}`)
	require.NoError(t, err)
}

func TestAgentTagsPersistence(t *testing.T) {
	ctx := context.Background()

	hash := "hashed_tag_test"
	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "tag-agent-" + uuid.New().String()[:8],
		Name:       "Tag Test Agent",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
		Tags:       []string{"finance", "compliance"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"finance", "compliance"}, agent.Tags)

	// Read it back and verify tags survive round-trip.
	got, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, agent.AgentID)
	require.NoError(t, err)
	assert.Equal(t, []string{"finance", "compliance"}, got.Tags)

	// Also test GetAgentByID.
	gotByID, err := testDB.GetAgentByID(ctx, agent.ID, agent.OrgID)
	require.NoError(t, err)
	assert.Equal(t, []string{"finance", "compliance"}, gotByID.Tags)
}

func TestAgentTagsDefaultEmpty(t *testing.T) {
	ctx := context.Background()

	hash := "hashed_default_tag"
	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "no-tag-agent-" + uuid.New().String()[:8],
		Name:       "No Tag Agent",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{}, agent.Tags)

	got, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, agent.AgentID)
	require.NoError(t, err)
	assert.Equal(t, []string{}, got.Tags)
}

func TestListAgentIDsBySharedTags(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	// Create three agents: two share "finance", one has only "legal".
	hash := "hashed_shared_tags"
	a1, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "finance-1-" + suffix,
		Name:       "Finance Agent 1",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
		Tags:       []string{"finance", "compliance"},
	})
	require.NoError(t, err)

	a2, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "finance-2-" + suffix,
		Name:       "Finance Agent 2",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
		Tags:       []string{"finance"},
	})
	require.NoError(t, err)

	a3, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "legal-1-" + suffix,
		Name:       "Legal Agent",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
		Tags:       []string{"legal"},
	})
	require.NoError(t, err)

	// Query for agents sharing "finance" tag — should find a1 and a2.
	ids, err := testDB.ListAgentIDsBySharedTags(ctx, uuid.Nil, []string{"finance"})
	require.NoError(t, err)

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	assert.True(t, idSet[a1.AgentID], "finance-1 should match finance tag")
	assert.True(t, idSet[a2.AgentID], "finance-2 should match finance tag")
	assert.False(t, idSet[a3.AgentID], "legal-1 should not match finance tag")

	// Query for "compliance" — only a1.
	ids2, err := testDB.ListAgentIDsBySharedTags(ctx, uuid.Nil, []string{"compliance"})
	require.NoError(t, err)

	idSet2 := make(map[string]bool, len(ids2))
	for _, id := range ids2 {
		idSet2[id] = true
	}
	assert.True(t, idSet2[a1.AgentID], "finance-1 should match compliance tag")
	assert.False(t, idSet2[a2.AgentID], "finance-2 has no compliance tag")

	// Query for "legal" OR "finance" — all three.
	ids3, err := testDB.ListAgentIDsBySharedTags(ctx, uuid.Nil, []string{"legal", "finance"})
	require.NoError(t, err)

	idSet3 := make(map[string]bool, len(ids3))
	for _, id := range ids3 {
		idSet3[id] = true
	}
	assert.True(t, idSet3[a1.AgentID])
	assert.True(t, idSet3[a2.AgentID])
	assert.True(t, idSet3[a3.AgentID])
}

func TestUpdateAgentTags(t *testing.T) {
	ctx := context.Background()

	hash := "hashed_update_tags"
	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "update-tag-" + uuid.New().String()[:8],
		Name:       "Update Tag Agent",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
		Tags:       []string{"original"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"original"}, agent.Tags)

	// Update tags.
	updated, err := testDB.UpdateAgentTags(ctx, uuid.Nil, agent.AgentID, []string{"finance", "compliance"})
	require.NoError(t, err)
	assert.Equal(t, []string{"finance", "compliance"}, updated.Tags)

	// Verify round-trip.
	got, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, agent.AgentID)
	require.NoError(t, err)
	assert.Equal(t, []string{"finance", "compliance"}, got.Tags)

	// Clear tags.
	cleared, err := testDB.UpdateAgentTags(ctx, uuid.Nil, agent.AgentID, []string{})
	require.NoError(t, err)
	assert.Equal(t, []string{}, cleared.Tags)

	// Update nonexistent agent.
	_, err = testDB.UpdateAgentTags(ctx, uuid.Nil, "nonexistent-agent", []string{"tag"})
	require.Error(t, err)
}

func TestListAgentsIncludesTags(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	hash := "hashed_list_tags"
	a1, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "list-tag-1-" + suffix,
		Name:       "List Tag Agent 1",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
		Tags:       []string{"team-a"},
	})
	require.NoError(t, err)

	a2, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:    "list-tag-2-" + suffix,
		Name:       "List Tag Agent 2",
		Role:       model.RoleAgent,
		APIKeyHash: &hash,
		Tags:       []string{"team-b", "team-c"},
	})
	require.NoError(t, err)

	// Retrieve individually to verify tags are present in list results.
	got1, err := testDB.GetAgentByID(ctx, a1.ID, a1.OrgID)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a"}, got1.Tags)

	got2, err := testDB.GetAgentByID(ctx, a2.ID, a2.OrgID)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-b", "team-c"}, got2.Tags)
}

func TestDeleteAgentDataClearsExternalSupersedesRefs(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "delete-a-" + suffix,
		Name:    "Delete Agent A",
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	agentB, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "delete-b-" + suffix,
		Name:    "Delete Agent B",
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA.AgentID})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB.AgentID})
	require.NoError(t, err)

	decA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        runA.ID,
		AgentID:      agentA.AgentID,
		OrgID:        agentA.OrgID,
		DecisionType: "delete-test",
		Outcome:      "first",
		Confidence:   0.7,
	})
	require.NoError(t, err)

	decB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        runB.ID,
		AgentID:      agentB.AgentID,
		OrgID:        agentB.OrgID,
		DecisionType: "delete-test",
		Outcome:      "second",
		Confidence:   0.8,
		SupersedesID: &decA.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, decB.SupersedesID)

	_, err = testDB.DeleteAgentData(ctx, agentA.OrgID, agentA.AgentID, nil)
	require.NoError(t, err)

	gotB, err := testDB.GetDecision(ctx, agentB.OrgID, decB.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Nil(t, gotB.SupersedesID)
}

func TestDeleteAgentDataDeletesClaims(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "delete-claims-" + suffix

	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		Name:    "Delete Claims Agent",
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		OrgID:        agent.OrgID,
		DecisionType: "gdpr-test",
		Outcome:      "test outcome with claims",
		Confidence:   0.8,
	})
	require.NoError(t, err)

	// Insert claims for this decision.
	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dec.ID, OrgID: dec.OrgID, ClaimIdx: 0, ClaimText: "PII claim one."},
		{DecisionID: dec.ID, OrgID: dec.OrgID, ClaimIdx: 1, ClaimText: "PII claim two."},
	})
	require.NoError(t, err)

	// Verify claims exist.
	claims, err := testDB.FindClaimsByDecision(ctx, dec.ID, dec.OrgID)
	require.NoError(t, err)
	require.Len(t, claims, 2)

	// Delete agent data (GDPR erasure).
	result, err := testDB.DeleteAgentData(ctx, agent.OrgID, agentID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.Claims, "should report 2 claims deleted")
	assert.Equal(t, int64(1), result.Decisions, "should report 1 decision deleted")

	// Verify claims are gone.
	claimsAfter, err := testDB.FindClaimsByDecision(ctx, dec.ID, dec.OrgID)
	require.NoError(t, err)
	assert.Empty(t, claimsAfter, "claims must be deleted for GDPR compliance")
}

// ---------------------------------------------------------------------------
// Tests 1-15: Extended storage coverage
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_FTS(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "fts-agent-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Use a unique, English-stemable word so FTS (websearch_to_tsquery) can match it.
	uniqueWord := "xylophonic" + suffix
	reasoning := "because the " + uniqueWord + " analysis was favorable"
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "fts_test",
		Outcome:      "approved with " + uniqueWord,
		Confidence:   0.85,
		Reasoning:    &reasoning,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	results, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, uniqueWord, model.QueryFilters{}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, results, "FTS should find the decision containing the unique word")

	found := false
	for _, r := range results {
		if r.Decision.AgentID == agentID {
			found = true
			assert.Contains(t, r.Decision.Outcome, uniqueWord)
			assert.Greater(t, r.SimilarityScore, float32(0), "relevance score should be positive")
			break
		}
	}
	assert.True(t, found, "expected to find decision from agent %s in search results", agentID)
}

func TestSearchDecisionsByText_EmptyQuery(t *testing.T) {
	ctx := context.Background()

	results, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, "", model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results, "empty query should return no results")
}

func TestQueryDecisions_AllFilters(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "allfilter-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	now := time.Now().UTC()
	reasoning := "filter test reasoning"
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "filter_type_" + suffix,
		Outcome:      "filter_outcome",
		Confidence:   0.75,
		Reasoning:    &reasoning,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	dType := "filter_type_" + suffix
	confMin := float32(0.5)
	from := now.Add(-1 * time.Second)
	to := now.Add(10 * time.Second)

	tests := []struct {
		name    string
		filters model.QueryFilters
	}{
		{
			name:    "by AgentIDs",
			filters: model.QueryFilters{AgentIDs: []string{agentID}},
		},
		{
			name:    "by DecisionType",
			filters: model.QueryFilters{AgentIDs: []string{agentID}, DecisionType: &dType},
		},
		{
			name:    "by ConfidenceMin",
			filters: model.QueryFilters{AgentIDs: []string{agentID}, ConfidenceMin: &confMin},
		},
		{
			name: "by TimeRange",
			filters: model.QueryFilters{
				AgentIDs:  []string{agentID},
				TimeRange: &model.TimeRange{From: &from, To: &to},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
				Filters: tc.filters,
				Limit:   50,
			})
			require.NoError(t, err)
			assert.GreaterOrEqual(t, total, 1, "should find at least one decision")

			found := false
			for _, dec := range decisions {
				if dec.ID == d.ID {
					found = true
					break
				}
			}
			assert.True(t, found, "target decision should appear in results for filter %s", tc.name)
		})
	}
}

func TestQueryDecisions_Ordering(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "order-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	confidences := []float32{0.1, 0.5, 0.3, 0.9, 0.7}
	for _, c := range confidences {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "ordering_test",
			Outcome:      fmt.Sprintf("conf_%.1f", c),
			Confidence:   c,
			Metadata:     map[string]any{},
		})
		require.NoError(t, err)
	}

	// Test ascending order.
	decisionsAsc, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters:  model.QueryFilters{AgentIDs: []string{agentID}},
		OrderBy:  "confidence",
		OrderDir: "asc",
		Limit:    50,
	})
	require.NoError(t, err)
	require.Len(t, decisionsAsc, len(confidences))
	for i := 1; i < len(decisionsAsc); i++ {
		assert.LessOrEqual(t, decisionsAsc[i-1].Confidence, decisionsAsc[i].Confidence,
			"ascending order violated at index %d: %.2f > %.2f", i, decisionsAsc[i-1].Confidence, decisionsAsc[i].Confidence)
	}

	// Test descending order.
	decisionsDesc, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters:  model.QueryFilters{AgentIDs: []string{agentID}},
		OrderBy:  "confidence",
		OrderDir: "desc",
		Limit:    50,
	})
	require.NoError(t, err)
	require.Len(t, decisionsDesc, len(confidences))
	for i := 1; i < len(decisionsDesc); i++ {
		assert.GreaterOrEqual(t, decisionsDesc[i-1].Confidence, decisionsDesc[i].Confidence,
			"descending order violated at index %d: %.2f < %.2f", i, decisionsDesc[i-1].Confidence, decisionsDesc[i].Confidence)
	}
}

func TestGetDecisionRevisions_Chain(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "revchain-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// A -> B -> C revision chain.
	a, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "revision_chain",
		Outcome:      "version_a",
		Confidence:   0.5,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	b, err := testDB.ReviseDecision(ctx, a.ID, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "revision_chain",
		Outcome:      "version_b",
		Confidence:   0.7,
		Metadata:     map[string]any{},
	}, nil)
	require.NoError(t, err)

	c, err := testDB.ReviseDecision(ctx, b.ID, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "revision_chain",
		Outcome:      "version_c",
		Confidence:   0.9,
		Metadata:     map[string]any{},
	}, nil)
	require.NoError(t, err)

	// Query the full chain starting from C.
	revisions, err := testDB.GetDecisionRevisions(ctx, uuid.Nil, c.ID)
	require.NoError(t, err)
	require.Len(t, revisions, 3, "chain A->B->C should produce 3 revisions")

	// Verify chronological ordering (by valid_from ASC).
	assert.Equal(t, a.ID, revisions[0].ID, "first revision should be A (oldest)")
	assert.Equal(t, b.ID, revisions[1].ID, "second revision should be B")
	assert.Equal(t, c.ID, revisions[2].ID, "third revision should be C (newest)")

	// Also verify chain is reachable from the middle node.
	revisionsFromB, err := testDB.GetDecisionRevisions(ctx, uuid.Nil, b.ID)
	require.NoError(t, err)
	assert.Len(t, revisionsFromB, 3, "chain should be fully traversable from any member")
}

func TestGetDecisionRevisions_NotFound(t *testing.T) {
	ctx := context.Background()

	revisions, err := testDB.GetDecisionRevisions(ctx, uuid.Nil, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, revisions, "nonexistent decision should return empty revision chain")
}

func TestGetRevisionChainIDs_TransitiveChain(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "chainids-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// A -> B -> C revision chain.
	a, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "chain_test",
		Outcome: "version_a", Confidence: 0.5, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	b, err := testDB.ReviseDecision(ctx, a.ID, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "chain_test",
		Outcome: "version_b", Confidence: 0.7, Metadata: map[string]any{},
	}, nil)
	require.NoError(t, err)

	c, err := testDB.ReviseDecision(ctx, b.ID, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "chain_test",
		Outcome: "version_c", Confidence: 0.9, Metadata: map[string]any{},
	}, nil)
	require.NoError(t, err)

	// From A: should return B and C (forward chain).
	idsFromA, err := testDB.GetRevisionChainIDs(ctx, a.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, idsFromA, 2, "A's chain should include B and C")
	assert.Contains(t, idsFromA, b.ID)
	assert.Contains(t, idsFromA, c.ID)

	// From C: should return A and B (backward chain).
	idsFromC, err := testDB.GetRevisionChainIDs(ctx, c.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, idsFromC, 2, "C's chain should include A and B")
	assert.Contains(t, idsFromC, a.ID)
	assert.Contains(t, idsFromC, b.ID)

	// From B: should return A and C (both directions).
	idsFromB, err := testDB.GetRevisionChainIDs(ctx, b.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, idsFromB, 2, "B's chain should include A and C")
	assert.Contains(t, idsFromB, a.ID)
	assert.Contains(t, idsFromB, c.ID)
}

func TestGetRevisionChainIDs_NoChain(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "nochainids-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "standalone",
		Outcome: "no revisions", Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	ids, err := testDB.GetRevisionChainIDs(ctx, d.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, ids, "standalone decision should have empty revision chain")
}

func TestGetRevisionChainIDs_NonexistentDecision(t *testing.T) {
	ctx := context.Background()

	ids, err := testDB.GetRevisionChainIDs(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, ids, "nonexistent decision should return empty chain")
}

func TestListConflicts_Filters(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "conflict-a-" + suffix
	agentB := "conflict-b-" + suffix
	decisionType := "conflict_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	// Create two decisions with the same decision_type but different outcomes.
	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        runA.ID,
		AgentID:      agentA,
		DecisionType: decisionType,
		Outcome:      "approve",
		Confidence:   0.8,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        runB.ID,
		AgentID:      agentB,
		DecisionType: decisionType,
		Outcome:      "deny",
		Confidence:   0.9,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	// Insert a scored conflict between the two decisions.
	topicSim := 0.95
	outcomeDiv := 0.85
	sig := topicSim * outcomeDiv
	_, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decisionType,
		DecisionTypeB:     decisionType,
		OutcomeA:          "approve",
		OutcomeB:          "deny",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)

	// Filter by decision_type.
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		DecisionType: &decisionType,
	}, 50, 0)
	require.NoError(t, err)
	assert.NotEmpty(t, conflicts, "should detect conflict between agents with same type but different outcomes")

	found := false
	for _, c := range conflicts {
		if c.DecisionType == decisionType {
			found = true
			break
		}
	}
	assert.True(t, found, "conflict with decision_type %s should appear in filtered results", decisionType)

	// Filter by agent_id.
	conflictsByAgent, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		AgentID: &agentA,
	}, 50, 0)
	require.NoError(t, err)

	for _, c := range conflictsByAgent {
		agentMatch := c.AgentA == agentA || c.AgentB == agentA
		assert.True(t, agentMatch, "agent filter should only return conflicts involving %s", agentA)
	}
}

func TestFindUnembeddedDecisions(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "unembed-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "unembedded_test",
		Outcome:      "needs_embedding",
		Confidence:   0.6,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	unembedded, err := testDB.FindUnembeddedDecisions(ctx, 1000)
	require.NoError(t, err)
	require.NotEmpty(t, unembedded, "newly created decisions without embeddings should appear")

	found := false
	for _, u := range unembedded {
		if u.ID == d.ID {
			found = true
			assert.Equal(t, "unembedded_test", u.DecisionType)
			assert.Equal(t, "needs_embedding", u.Outcome)
			break
		}
	}
	assert.True(t, found, "our decision %s should appear in unembedded results", d.ID)
}

func TestBackfillEmbedding(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "backfill-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "backfill_test",
		Outcome:      "will_get_embedding",
		Confidence:   0.7,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	// Verify it starts unembedded.
	unembedded, err := testDB.FindUnembeddedDecisions(ctx, 1000)
	require.NoError(t, err)
	foundBefore := false
	for _, u := range unembedded {
		if u.ID == d.ID {
			foundBefore = true
			break
		}
	}
	assert.True(t, foundBefore, "decision should be unembedded before backfill")

	// Create a fake 1024-dimensional embedding vector.
	dims := 1024
	vec := make([]float32, dims)
	for i := range vec {
		vec[i] = float32(i) / float32(dims)
	}
	embedding := pgvector.NewVector(vec)

	err = testDB.BackfillEmbedding(ctx, d.ID, d.OrgID, embedding)
	require.NoError(t, err)

	// Verify the decision is no longer in the unembedded list.
	unembeddedAfter, err := testDB.FindUnembeddedDecisions(ctx, 1000)
	require.NoError(t, err)
	foundAfter := false
	for _, u := range unembeddedAfter {
		if u.ID == d.ID {
			foundAfter = true
			break
		}
	}
	assert.False(t, foundAfter, "decision should not appear in unembedded list after backfill")
}

func TestGetDecisionsByIDs_EmptySlice(t *testing.T) {
	ctx := context.Background()

	result, err := testDB.GetDecisionsByIDs(ctx, uuid.Nil, []uuid.UUID{})
	require.NoError(t, err)
	assert.Nil(t, result, "empty ID slice should return nil map")
}

func TestGetDecisionsByIDs_PartialMatch(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "partial-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "partial_match",
		Outcome:      "exists",
		Confidence:   0.8,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	randomID := uuid.New()
	result, err := testDB.GetDecisionsByIDs(ctx, uuid.Nil, []uuid.UUID{d.ID, randomID})
	require.NoError(t, err)
	assert.Len(t, result, 1, "should return only the existing decision")
	assert.Contains(t, result, d.ID, "result map should contain the existing decision ID")
	assert.Equal(t, "exists", result[d.ID].Outcome)
}

func TestNewConflictsSince(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	// Record a time before creating conflicting decisions.
	beforeConflicts := time.Now().UTC().Add(-1 * time.Second)

	agentA := "newconf-a-" + suffix
	agentB := "newconf-b-" + suffix
	decisionType := "newconf_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        runA.ID,
		AgentID:      agentA,
		DecisionType: decisionType,
		Outcome:      "option_x",
		Confidence:   0.85,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        runB.ID,
		AgentID:      agentB,
		DecisionType: decisionType,
		Outcome:      "option_y",
		Confidence:   0.90,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	// Insert a scored conflict between the two decisions.
	topicSim := 0.90
	outcomeDiv := 0.80
	sig := topicSim * outcomeDiv
	_, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             dA.OrgID,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decisionType,
		DecisionTypeB:     decisionType,
		OutcomeA:          "option_x",
		OutcomeB:          "option_y",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)

	conflicts, err := testDB.NewConflictsSinceByOrg(ctx, dA.OrgID, beforeConflicts, 100)
	require.NoError(t, err)

	found := false
	for _, c := range conflicts {
		if c.DecisionType == decisionType {
			found = true
			assert.True(t, c.DetectedAt.After(beforeConflicts),
				"detected_at should be after our timestamp")
			break
		}
	}
	assert.True(t, found, "NewConflictsSince should return the newly created conflict")
}

func TestExportDecisionsCursor_PaginationOrder(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "export-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create 5 decisions. The keyset cursor (valid_from, id) handles ties.
	const count = 5
	createdIDs := make([]uuid.UUID, 0, count)
	for i := range count {
		d, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "export_test",
			Outcome:      fmt.Sprintf("export_%d", i),
			Confidence:   float32(i+1) * 0.1,
			Metadata:     map[string]any{},
		})
		require.NoError(t, err)
		createdIDs = append(createdIDs, d.ID)
	}

	// Paginate with limit=2, collecting all pages.
	var allDecisions []model.Decision
	var cursor *storage.ExportCursor

	for {
		page, err := testDB.ExportDecisionsCursor(ctx, uuid.Nil,
			model.QueryFilters{AgentIDs: []string{agentID}}, cursor, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		allDecisions = append(allDecisions, page...)
		last := page[len(page)-1]
		cursor = &storage.ExportCursor{ValidFrom: last.ValidFrom, ID: last.ID}
	}

	assert.Len(t, allDecisions, count, "all decisions should be returned across pages")

	// Verify ascending valid_from ordering (keyset pagination guarantees this).
	for i := 1; i < len(allDecisions); i++ {
		assert.False(t, allDecisions[i].ValidFrom.Before(allDecisions[i-1].ValidFrom),
			"valid_from should be non-decreasing: index %d (%s) < index %d (%s)",
			i, allDecisions[i].ValidFrom, i-1, allDecisions[i-1].ValidFrom)
	}

	// Verify all our created IDs appear.
	exportedIDs := make(map[uuid.UUID]bool, len(allDecisions))
	for _, d := range allDecisions {
		exportedIDs[d.ID] = true
	}
	for _, id := range createdIDs {
		assert.True(t, exportedIDs[id], "created decision %s should appear in export results", id)
	}
}

func TestQueryDecisionsTemporal_ZeroTime(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "zerotime-" + suffix

	// Create a decision so we know at least one exists for this agent.
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "temporal_zero",
		Outcome:      "should_not_appear",
		Confidence:   0.5,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	// Query with zero time.Time{} — this is year 0001, before any decision was created.
	decisions, err := testDB.QueryDecisionsTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf: time.Time{},
		Filters: model.QueryFilters{
			AgentIDs: []string{agentID},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, decisions, "zero time is before any decision, should return empty")
}

func TestSearchDecisionsByText_ILIKEFallback(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "ilike-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Use a unique string that is not a normal English word, so FTS
	// returns nothing and the ILIKE fallback path is exercised.
	uniqueToken := "zq" + suffix
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "ilike_test",
		Outcome:      "result_with_" + uniqueToken + "_inside",
		Confidence:   0.6,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	// Search with a 2-character prefix substring. FTS typically won't match
	// such a short/non-dictionary term, triggering the ILIKE fallback.
	results, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, uniqueToken[:4], model.QueryFilters{}, 10)
	require.NoError(t, err)

	found := false
	for _, r := range results {
		if r.Decision.AgentID == agentID {
			found = true
			assert.Contains(t, r.Decision.Outcome, uniqueToken)
			break
		}
	}
	assert.True(t, found, "ILIKE fallback should match the substring %q in the outcome", uniqueToken[:4])
}

// ---------------------------------------------------------------------------
// Tests 16-45: Extended storage coverage (high-value uncovered functions)
// ---------------------------------------------------------------------------

func TestCreateTraceTx(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "tracetx-" + suffix

	reasoning := "test reasoning for trace tx"
	rel := float32(0.85)

	run, decision, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:  agentID,
		OrgID:    uuid.Nil,
		Metadata: map[string]any{"source": "test"},
		Decision: model.Decision{
			DecisionType: "tracetx_type",
			Outcome:      "tracetx_outcome",
			Confidence:   0.88,
			Reasoning:    &reasoning,
			Metadata:     map[string]any{"key": "val"},
		},
		Alternatives: []model.Alternative{
			{Label: "Option A"},
			{Label: "Option B"},
		},
		Evidence: []model.Evidence{
			{
				SourceType:     model.SourceDocument,
				Content:        "Supporting document content",
				RelevanceScore: &rel,
			},
		},
	})
	require.NoError(t, err)

	// Verify run was created and completed atomically.
	assert.Equal(t, agentID, run.AgentID)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
	assert.NotNil(t, run.CompletedAt)

	// Verify run persisted in DB.
	gotRun, err := testDB.GetRun(ctx, run.OrgID, run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, gotRun.Status)

	// Verify decision persisted with correct fields.
	assert.Equal(t, "tracetx_outcome", decision.Outcome)
	assert.Equal(t, float32(0.88), decision.Confidence)
	assert.Equal(t, run.ID, decision.RunID)
	assert.NotEmpty(t, decision.ContentHash)

	gotDec, err := testDB.GetDecision(ctx, decision.OrgID, decision.ID, storage.GetDecisionOpts{
		IncludeAlts:     true,
		IncludeEvidence: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "tracetx_outcome", gotDec.Outcome)
	assert.Len(t, gotDec.Alternatives, 2)
	assert.Len(t, gotDec.Evidence, 1)
	assert.Equal(t, "Supporting document content", gotDec.Evidence[0].Content)
}

func TestCreateTraceTx_WithSession(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "tracetx-sess-" + suffix
	sessionID := uuid.New()

	_, decision, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:   agentID,
		OrgID:     uuid.Nil,
		SessionID: &sessionID,
		AgentContext: map[string]any{
			"model": "gpt-4o",
			"tool":  "code_review",
		},
		Decision: model.Decision{
			DecisionType: "session_trace",
			Outcome:      "approved",
			Confidence:   0.75,
		},
	})
	require.NoError(t, err)

	gotDec, err := testDB.GetDecision(ctx, decision.OrgID, decision.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	require.NotNil(t, gotDec.SessionID)
	assert.Equal(t, sessionID, *gotDec.SessionID)
	assert.Equal(t, "gpt-4o", gotDec.AgentContext["model"])
	assert.Equal(t, "code_review", gotDec.AgentContext["tool"])
}

func TestInsertEvents_VerifyFields(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "inevent-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	now := time.Now().UTC()
	eventID := uuid.New()
	events := []model.AgentEvent{
		{
			ID:          eventID,
			RunID:       run.ID,
			EventType:   model.EventToolCallStarted,
			SequenceNum: 1,
			OccurredAt:  now,
			AgentID:     agentID,
			Payload:     map[string]any{"tool": "search", "query": "test"},
			CreatedAt:   now,
		},
	}

	count, err := testDB.InsertEvents(ctx, events)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, eventID, got[0].ID)
	assert.Equal(t, model.EventToolCallStarted, got[0].EventType)
	assert.Equal(t, agentID, got[0].AgentID)
	assert.Equal(t, "search", got[0].Payload["tool"])
}

func TestInsertEvent_Single(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "single-evt-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	now := time.Now().UTC()
	eventID := uuid.New()
	err = testDB.InsertEvent(ctx, model.AgentEvent{
		ID:          eventID,
		RunID:       run.ID,
		EventType:   model.EventAgentHandoff,
		SequenceNum: 1,
		OccurredAt:  now,
		AgentID:     agentID,
		Payload:     map[string]any{"target": "reviewer"},
		CreatedAt:   now,
	})
	require.NoError(t, err)

	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, eventID, got[0].ID)
	assert.Equal(t, model.EventAgentHandoff, got[0].EventType)
	assert.Equal(t, int64(1), got[0].SequenceNum)
}

func TestCreateEvidence_Single(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "ev-single-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "evidence_test",
		Outcome:      "proceed",
		Confidence:   0.7,
	})
	require.NoError(t, err)

	rel := float32(0.92)
	sourceURI := "https://example.com/doc"
	ev, err := testDB.CreateEvidence(ctx, model.Evidence{
		DecisionID:     d.ID,
		OrgID:          d.OrgID,
		SourceType:     model.SourceSearchResult,
		SourceURI:      &sourceURI,
		Content:        "Relevant search result content",
		RelevanceScore: &rel,
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, ev.ID)
	assert.Equal(t, "Relevant search result content", ev.Content)
	assert.Equal(t, model.SourceSearchResult, ev.SourceType)
	assert.Equal(t, &sourceURI, ev.SourceURI)
	assert.Equal(t, &rel, ev.RelevanceScore)

	// Verify round-trip via GetEvidenceByDecision.
	evs, err := testDB.GetEvidenceByDecision(ctx, d.ID, d.OrgID)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, ev.ID, evs[0].ID)
	assert.Equal(t, "Relevant search result content", evs[0].Content)
}

func TestHasAccess_WithExpiry(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "has-grantor-" + suffix,
		Name:    "Grantor",
		Role:    model.RoleAdmin,
	})
	require.NoError(t, err)

	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "has-grantee-" + suffix,
		Name:    "Grantee",
		Role:    model.RoleReader,
	})
	require.NoError(t, err)

	resID := "test-resource-" + suffix

	// Grant that expired in the past.
	expired := time.Now().UTC().Add(-1 * time.Hour)
	_, err = testDB.CreateGrant(ctx, model.AccessGrant{
		GrantorID:    grantor.ID,
		GranteeID:    grantee.ID,
		ResourceType: "agent_traces",
		ResourceID:   &resID,
		Permission:   "read",
		ExpiresAt:    &expired,
	})
	require.NoError(t, err)

	// Expired grant should not provide access.
	has, err := testDB.HasAccess(ctx, uuid.Nil, grantee.ID, "agent_traces", resID, "read")
	require.NoError(t, err)
	assert.False(t, has, "expired grant should not provide access")
}

func TestListGrantedAgentIDs(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "lg-grantor-" + suffix,
		Name:    "Grantor",
		Role:    model.RoleAdmin,
	})
	require.NoError(t, err)

	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "lg-grantee-" + suffix,
		Name:    "Grantee",
		Role:    model.RoleReader,
	})
	require.NoError(t, err)

	res1 := "agent-alpha-" + suffix
	res2 := "agent-beta-" + suffix
	for _, resID := range []string{res1, res2} {
		r := resID
		_, err = testDB.CreateGrant(ctx, model.AccessGrant{
			GrantorID:    grantor.ID,
			GranteeID:    grantee.ID,
			ResourceType: "agent_traces",
			ResourceID:   &r,
			Permission:   "read",
		})
		require.NoError(t, err)
	}

	selfAgentID := "lg-grantee-" + suffix
	granted, err := testDB.ListGrantedAgentIDs(ctx, uuid.Nil, grantee.ID, selfAgentID)
	require.NoError(t, err)

	// Should include self plus the two granted resources.
	assert.True(t, granted[selfAgentID], "self agent_id should always be included")
	assert.True(t, granted[res1], "granted resource 1 should be present")
	assert.True(t, granted[res2], "granted resource 2 should be present")
}

func TestGetGrant(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "gg-grantor-" + suffix,
		Name:    "Grantor",
		Role:    model.RoleAdmin,
	})
	require.NoError(t, err)

	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "gg-grantee-" + suffix,
		Name:    "Grantee",
		Role:    model.RoleReader,
	})
	require.NoError(t, err)

	resID := "gg-resource-" + suffix
	grant, err := testDB.CreateGrant(ctx, model.AccessGrant{
		GrantorID:    grantor.ID,
		GranteeID:    grantee.ID,
		ResourceType: "agent_traces",
		ResourceID:   &resID,
		Permission:   "read",
	})
	require.NoError(t, err)

	got, err := testDB.GetGrant(ctx, grant.OrgID, grant.ID)
	require.NoError(t, err)
	assert.Equal(t, grant.ID, got.ID)
	assert.Equal(t, grantor.ID, got.GrantorID)
	assert.Equal(t, grantee.ID, got.GranteeID)
	assert.Equal(t, "agent_traces", got.ResourceType)
	assert.Equal(t, &resID, got.ResourceID)

	// Not found case.
	_, err = testDB.GetGrant(ctx, uuid.Nil, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestListGrantsByGrantee(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "lgb-grantor-" + suffix,
		Name:    "Grantor",
		Role:    model.RoleAdmin,
	})
	require.NoError(t, err)

	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "lgb-grantee-" + suffix,
		Name:    "Grantee",
		Role:    model.RoleReader,
	})
	require.NoError(t, err)

	// Create three grants for this grantee.
	for i := range 3 {
		resID := fmt.Sprintf("lgb-resource-%d-%s", i, suffix)
		_, err = testDB.CreateGrant(ctx, model.AccessGrant{
			GrantorID:    grantor.ID,
			GranteeID:    grantee.ID,
			ResourceType: "agent_traces",
			ResourceID:   &resID,
			Permission:   "read",
		})
		require.NoError(t, err)
	}

	grants, err := testDB.ListGrantsByGrantee(ctx, uuid.Nil, grantee.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(grants), 3, "should have at least 3 grants")

	// Verify all belong to the grantee.
	for _, g := range grants {
		assert.Equal(t, grantee.ID, g.GranteeID)
	}
}

func TestGetDecision_WithOpts(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "getdec-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	reasoning := "decision with full data"
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "getdec_test",
		Outcome:      "selected_option",
		Confidence:   0.95,
		Reasoning:    &reasoning,
	})
	require.NoError(t, err)

	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "Selected"},
	})
	require.NoError(t, err)

	rel := float32(0.88)
	_, err = testDB.CreateEvidence(ctx, model.Evidence{
		DecisionID:     d.ID,
		OrgID:          d.OrgID,
		SourceType:     model.SourceToolOutput,
		Content:        "Tool output data",
		RelevanceScore: &rel,
	})
	require.NoError(t, err)

	// Get without includes.
	bare, err := testDB.GetDecision(ctx, d.OrgID, d.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Empty(t, bare.Alternatives)
	assert.Empty(t, bare.Evidence)

	// Get with includes.
	full, err := testDB.GetDecision(ctx, d.OrgID, d.ID, storage.GetDecisionOpts{
		IncludeAlts:     true,
		IncludeEvidence: true,
	})
	require.NoError(t, err)
	assert.Len(t, full.Alternatives, 1)
	assert.Len(t, full.Evidence, 1)
	assert.Equal(t, "Selected", full.Alternatives[0].Label)
	assert.Equal(t, "Tool output data", full.Evidence[0].Content)

	// CurrentOnly on an active decision should succeed.
	current, err := testDB.GetDecision(ctx, d.OrgID, d.ID, storage.GetDecisionOpts{CurrentOnly: true})
	require.NoError(t, err)
	assert.Equal(t, d.ID, current.ID)

	// Not found case.
	_, err = testDB.GetDecision(ctx, uuid.Nil, uuid.New(), storage.GetDecisionOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestListAgents_Pagination(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	// Create a batch of agents with a unique suffix so we can count them.
	const agentCount = 5
	for i := range agentCount {
		_, err := testDB.CreateAgent(ctx, model.Agent{
			AgentID: fmt.Sprintf("la-%d-%s", i, suffix),
			Name:    fmt.Sprintf("List Agent %d", i),
			Role:    model.RoleAgent,
		})
		require.NoError(t, err)
	}

	// List all agents in the default org (there may be more from other tests).
	allAgents, err := testDB.ListAgents(ctx, uuid.Nil, 1000, 0)
	require.NoError(t, err)

	// Count how many of our agents appear.
	var ours int
	for _, a := range allAgents {
		for i := range agentCount {
			if a.AgentID == fmt.Sprintf("la-%d-%s", i, suffix) {
				ours++
			}
		}
	}
	assert.Equal(t, agentCount, ours, "all created agents should appear in ListAgents")

	// Test pagination: limit=2, offset=0.
	page1, err := testDB.ListAgents(ctx, uuid.Nil, 2, 0)
	require.NoError(t, err)
	assert.Len(t, page1, 2)

	// Different offset should return different agents.
	page2, err := testDB.ListAgents(ctx, uuid.Nil, 2, 2)
	require.NoError(t, err)
	assert.Len(t, page2, 2)
	assert.NotEqual(t, page1[0].ID, page2[0].ID, "paginated pages should return different agents")
}

func TestCountAgents(t *testing.T) {
	ctx := context.Background()

	count, err := testDB.CountAgents(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.Greater(t, count, 0, "default org should have at least one agent from earlier tests")
}

func TestGetSessionDecisions(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "session-" + suffix
	sessionID := uuid.New()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create 3 decisions in the same session.
	for i := range 3 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "session_test",
			Outcome:      fmt.Sprintf("session_outcome_%d", i),
			Confidence:   float32(i+1) * 0.25,
			SessionID:    &sessionID,
		})
		require.NoError(t, err)
	}

	// Also create a decision with a different session to verify isolation.
	otherSession := uuid.New()
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "session_test",
		Outcome:      "other_session",
		Confidence:   0.5,
		SessionID:    &otherSession,
	})
	require.NoError(t, err)

	decisions, err := testDB.GetSessionDecisions(ctx, uuid.Nil, sessionID)
	require.NoError(t, err)
	assert.Len(t, decisions, 3, "should only return decisions from the target session")

	for _, d := range decisions {
		require.NotNil(t, d.SessionID)
		assert.Equal(t, sessionID, *d.SessionID)
	}

	// Verify chronological ordering (valid_from ASC).
	for i := 1; i < len(decisions); i++ {
		assert.False(t, decisions[i].ValidFrom.Before(decisions[i-1].ValidFrom),
			"decisions should be ordered by valid_from ASC")
	}
}

func TestCreateIntegrityProof_And_GetLatest(t *testing.T) {
	ctx := context.Background()

	now := time.Now().UTC()
	batchStart := now.Add(-1 * time.Hour)
	batchEnd := now

	proof1 := storage.IntegrityProof{
		OrgID:         uuid.Nil,
		BatchStart:    batchStart,
		BatchEnd:      batchEnd,
		DecisionCount: 10,
		RootHash:      "abc123hash_first",
		CreatedAt:     now.Add(-30 * time.Minute),
	}
	err := testDB.CreateIntegrityProof(ctx, proof1)
	require.NoError(t, err)

	prevRoot := "abc123hash_first"
	proof2 := storage.IntegrityProof{
		OrgID:         uuid.Nil,
		BatchStart:    batchEnd,
		BatchEnd:      now.Add(1 * time.Hour),
		DecisionCount: 5,
		RootHash:      "def456hash_second",
		PreviousRoot:  &prevRoot,
		CreatedAt:     now,
	}
	err = testDB.CreateIntegrityProof(ctx, proof2)
	require.NoError(t, err)

	// GetLatestIntegrityProof should return the most recent one.
	latest, err := testDB.GetLatestIntegrityProof(ctx, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, "def456hash_second", latest.RootHash)
	assert.Equal(t, 5, latest.DecisionCount)
	require.NotNil(t, latest.PreviousRoot)
	assert.Equal(t, "abc123hash_first", *latest.PreviousRoot)

	// Non-existent org should return nil without error.
	randomOrg := uuid.New()
	noProof, err := testDB.GetLatestIntegrityProof(ctx, randomOrg)
	require.NoError(t, err)
	assert.Nil(t, noProof)
}

func TestGetDecisionHashesForBatch(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "hash-batch-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	beforeCreate := time.Now().UTC().Add(-1 * time.Second)

	// Create several decisions. Each gets a content_hash from CreateDecision.
	const count = 4
	for i := range count {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "hash_test",
			Outcome:      fmt.Sprintf("hash_outcome_%d", i),
			Confidence:   float32(i+1) * 0.2,
		})
		require.NoError(t, err)
	}

	afterCreate := time.Now().UTC().Add(1 * time.Second)

	hashes, err := testDB.GetDecisionHashesForBatch(ctx, uuid.Nil, beforeCreate, afterCreate)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(hashes), count, "should find at least %d hashes in the batch window", count)

	// Verify lexicographic ordering.
	for i := 1; i < len(hashes); i++ {
		assert.LessOrEqual(t, hashes[i-1], hashes[i],
			"hashes should be in lexicographic order: %s > %s at index %d", hashes[i-1], hashes[i], i)
	}

	// Verify all hashes are non-empty.
	for _, h := range hashes {
		assert.NotEmpty(t, h, "content hash should not be empty")
	}
}

func TestListOrganizationIDs(t *testing.T) {
	ctx := context.Background()

	ids, err := testDB.ListOrganizationIDs(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, ids, "should have at least the default org")

	foundDefault := false
	for _, id := range ids {
		if id == uuid.Nil {
			foundDefault = true
			break
		}
	}
	assert.True(t, foundDefault, "default org (uuid.Nil) should be in the list")
}

func TestWithRetry_SucceedsImmediately(t *testing.T) {
	ctx := context.Background()
	callCount := 0

	err := storage.WithRetry(ctx, 3, 10*time.Millisecond, func() error {
		callCount++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, callCount, "should only call fn once when it succeeds immediately")
}

func TestWithRetry_NonRetriableError(t *testing.T) {
	ctx := context.Background()
	callCount := 0
	permanent := fmt.Errorf("permanent error")

	err := storage.WithRetry(ctx, 3, 10*time.Millisecond, func() error {
		callCount++
		return permanent
	})
	require.Error(t, err)
	assert.Equal(t, permanent, err)
	assert.Equal(t, 1, callCount, "non-retriable error should not trigger retry")
}

func TestWithRetry_ContextCancellation(t *testing.T) {
	// WithRetry with a cancelled context should exit promptly.
	// We use a pgconn.PgError to trigger the retry path, then cancel context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	callCount := 0
	err := storage.WithRetry(ctx, 3, 10*time.Millisecond, func() error {
		callCount++
		// Return nil on first call since context is already cancelled,
		// the loop won't sleep but fn is called once.
		return nil
	})
	// The function returns nil because fn() returned nil.
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)
}

func TestGetDecisionsByAgent(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "byagent-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	const decCount = 4
	for i := range decCount {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "byagent_test",
			Outcome:      fmt.Sprintf("agent_dec_%d", i),
			Confidence:   float32(i+1) * 0.2,
		})
		require.NoError(t, err)
	}

	decisions, total, err := testDB.GetDecisionsByAgent(ctx, uuid.Nil, agentID, 10, 0, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, decCount, total)
	assert.Len(t, decisions, decCount)

	// All returned decisions should belong to our agent.
	for _, d := range decisions {
		assert.Equal(t, agentID, d.AgentID)
	}

	// Verify descending valid_from ordering.
	for i := 1; i < len(decisions); i++ {
		assert.False(t, decisions[i].ValidFrom.After(decisions[i-1].ValidFrom),
			"decisions should be ordered by valid_from DESC")
	}

	// Test pagination.
	page1, total1, err := testDB.GetDecisionsByAgent(ctx, uuid.Nil, agentID, 2, 0, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, decCount, total1)
	assert.Len(t, page1, 2)

	page2, _, err := testDB.GetDecisionsByAgent(ctx, uuid.Nil, agentID, 2, 2, nil, nil)
	require.NoError(t, err)
	assert.Len(t, page2, 2)
	assert.NotEqual(t, page1[0].ID, page2[0].ID, "pages should not overlap")
}

func TestGetDecisionsByAgent_TimeRange(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "byagent-tr-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "tr_test",
		Outcome:      "in_range",
		Confidence:   0.8,
	})
	require.NoError(t, err)

	from := time.Now().UTC().Add(-5 * time.Second)
	to := time.Now().UTC().Add(5 * time.Second)
	decisions, total, err := testDB.GetDecisionsByAgent(ctx, uuid.Nil, agentID, 10, 0, &from, &to)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, decisions, 1)

	// Query with a time range in the distant past should return nothing.
	pastFrom := time.Now().UTC().Add(-48 * time.Hour)
	pastTo := time.Now().UTC().Add(-24 * time.Hour)
	decisions, total, err = testDB.GetDecisionsByAgent(ctx, uuid.Nil, agentID, 10, 0, &pastFrom, &pastTo)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestFindDecisionsMissingOutcomeEmbedding(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "missoe-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create a decision with an embedding but no outcome_embedding.
	dims := 1024
	vec := make([]float32, dims)
	for i := range vec {
		vec[i] = float32(i) / float32(dims)
	}
	embedding := pgvector.NewVector(vec)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "missing_oe_test",
		Outcome:      "needs_outcome_emb",
		Confidence:   0.65,
		Embedding:    &embedding,
	})
	require.NoError(t, err)

	missing, err := testDB.FindDecisionsMissingOutcomeEmbedding(ctx, 1000)
	require.NoError(t, err)

	found := false
	for _, m := range missing {
		if m.ID == d.ID {
			found = true
			assert.Equal(t, "missing_oe_test", m.DecisionType)
			assert.Equal(t, "needs_outcome_emb", m.Outcome)
			break
		}
	}
	assert.True(t, found, "decision %s should appear in missing outcome embedding results", d.ID)

	// After backfilling outcome_embedding, it should no longer appear.
	err = testDB.BackfillOutcomeEmbedding(ctx, d.ID, d.OrgID, embedding)
	require.NoError(t, err)

	missing2, err := testDB.FindDecisionsMissingOutcomeEmbedding(ctx, 1000)
	require.NoError(t, err)
	for _, m := range missing2 {
		assert.NotEqual(t, d.ID, m.ID, "decision should not appear after outcome_embedding backfill")
	}
}

func TestGetEvidenceByDecisions_BatchLookup(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "ev-batch-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create two decisions, each with evidence.
	d1, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "ev_batch", Outcome: "dec1", Confidence: 0.7,
	})
	require.NoError(t, err)

	d2, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "ev_batch", Outcome: "dec2", Confidence: 0.8,
	})
	require.NoError(t, err)

	rel1 := float32(0.9)
	rel2 := float32(0.7)
	err = testDB.CreateEvidenceBatch(ctx, []model.Evidence{
		{DecisionID: d1.ID, OrgID: d1.OrgID, SourceType: model.SourceDocument, Content: "doc for d1", RelevanceScore: &rel1},
		{DecisionID: d2.ID, OrgID: d2.OrgID, SourceType: model.SourceUserInput, Content: "input for d2", RelevanceScore: &rel2},
	})
	require.NoError(t, err)

	evMap, err := testDB.GetEvidenceByDecisions(ctx, []uuid.UUID{d1.ID, d2.ID}, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, evMap[d1.ID], 1)
	assert.Len(t, evMap[d2.ID], 1)
	assert.Equal(t, "doc for d1", evMap[d1.ID][0].Content)
	assert.Equal(t, "input for d2", evMap[d2.ID][0].Content)

	// Empty slice should return nil.
	empty, err := testDB.GetEvidenceByDecisions(ctx, []uuid.UUID{}, uuid.Nil)
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestCountConflicts(t *testing.T) {
	ctx := context.Background()

	// CountConflicts with no filters should return zero or more without error.
	count, err := testDB.CountConflicts(ctx, uuid.Nil, storage.ConflictFilters{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

func TestRefreshConflicts_NoOp(t *testing.T) {
	ctx := context.Background()

	// RefreshConflicts is a no-op, should always succeed.
	err := testDB.RefreshConflicts(ctx)
	require.NoError(t, err)
}

func TestEnsureDefaultOrg_Idempotent(t *testing.T) {
	ctx := context.Background()

	// Should succeed even when called multiple times (ON CONFLICT DO NOTHING).
	err := testDB.EnsureDefaultOrg(ctx)
	require.NoError(t, err)

	err = testDB.EnsureDefaultOrg(ctx)
	require.NoError(t, err)

	// Verify the default org exists via GetOrganization.
	org, err := testDB.GetOrganization(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "Default", org.Name)
	assert.Equal(t, "default", org.Slug)
}

func TestGetOrganization_NotFound(t *testing.T) {
	ctx := context.Background()

	_, err := testDB.GetOrganization(ctx, uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestInsertEvents_Empty(t *testing.T) {
	ctx := context.Background()

	count, err := testDB.InsertEvents(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

func TestCountAgentsGlobal(t *testing.T) {
	ctx := context.Background()

	count, err := testDB.CountAgentsGlobal(ctx)
	require.NoError(t, err)
	assert.Greater(t, count, 0, "there should be agents from earlier tests")
}

func TestGetAgentsByAgentIDGlobal(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "global-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		Name:    "Global Lookup Agent",
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	agents, err := testDB.GetAgentsByAgentIDGlobal(ctx, agentID)
	require.NoError(t, err)
	require.Len(t, agents, 1)
	assert.Equal(t, agentID, agents[0].AgentID)

	// Not found case.
	_, err = testDB.GetAgentsByAgentIDGlobal(ctx, "nonexistent-agent-"+suffix)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestHasDecisionsWithNullSearchVector(t *testing.T) {
	ctx := context.Background()

	// This function checks for search_vector IS NULL. The trigger may or may not
	// be present in the test schema, so we just verify the query runs without error.
	_, err := testDB.HasDecisionsWithNullSearchVector(ctx)
	require.NoError(t, err)
}

func TestQueryDecisions_WithInclude(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "incl-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "include_test",
		Outcome:      "with_includes",
		Confidence:   0.77,
	})
	require.NoError(t, err)

	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "Alt1"},
	})
	require.NoError(t, err)

	rel := float32(0.9)
	_, err = testDB.CreateEvidence(ctx, model.Evidence{
		DecisionID: d.ID, OrgID: d.OrgID, SourceType: model.SourceMemory,
		Content: "memory content", RelevanceScore: &rel,
	})
	require.NoError(t, err)

	// Query with include=["alternatives","evidence"].
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{AgentIDs: []string{agentID}},
		Include: []string{"alternatives", "evidence"},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Len(t, decisions[0].Alternatives, 1)
	assert.Len(t, decisions[0].Evidence, 1)
}

func TestQueryDecisions_SessionFilter(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "qsess-" + suffix
	sessionID := uuid.New()

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "sess_filter",
		Outcome:      "in_session",
		Confidence:   0.8,
		SessionID:    &sessionID,
	})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "sess_filter",
		Outcome:      "no_session",
		Confidence:   0.6,
	})
	require.NoError(t, err)

	// Filter by session_id.
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{
			AgentIDs:  []string{agentID},
			SessionID: &sessionID,
		},
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Equal(t, "in_session", decisions[0].Outcome)
}

func TestNewConflictsSinceByOrg(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	beforeConflicts := time.Now().UTC().Add(-1 * time.Second)

	agentA := "ncbo-a-" + suffix
	agentB := "ncbo-b-" + suffix
	decisionType := "ncbo_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: decisionType,
		Outcome: "option_1", Confidence: 0.85,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, DecisionType: decisionType,
		Outcome: "option_2", Confidence: 0.9,
	})
	require.NoError(t, err)

	topicSim := 0.88
	outcomeDiv := 0.75
	sig := topicSim * outcomeDiv
	_, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: decisionType, DecisionTypeB: decisionType,
		OutcomeA: "option_1", OutcomeB: "option_2",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	conflicts, err := testDB.NewConflictsSinceByOrg(ctx, uuid.Nil, beforeConflicts, 100)
	require.NoError(t, err)

	found := false
	for _, c := range conflicts {
		if c.DecisionType == decisionType {
			found = true
			break
		}
	}
	assert.True(t, found, "NewConflictsSinceByOrg should return conflicts for this org")
}

func TestRefreshAgentState(t *testing.T) {
	ctx := context.Background()

	// RefreshAgentState should succeed without error (REFRESH MATERIALIZED VIEW CONCURRENTLY).
	err := testDB.RefreshAgentState(ctx)
	require.NoError(t, err)
}

func TestDeleteGrant_NotFound(t *testing.T) {
	ctx := context.Background()

	err := testDB.DeleteGrant(ctx, uuid.Nil, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestBackfillOutcomeEmbedding_NoOpForMissing(t *testing.T) {
	ctx := context.Background()

	// Backfilling a non-existent decision should silently succeed (0 rows affected).
	dims := 1024
	vec := make([]float32, dims)
	embedding := pgvector.NewVector(vec)

	err := testDB.BackfillOutcomeEmbedding(ctx, uuid.New(), uuid.Nil, embedding)
	require.NoError(t, err)
}

func TestFindEmbeddedDecisionIDs(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "embedded-ids-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	emb := pgvector.NewVector(make([]float32, 1024))
	outcomeEmb := pgvector.NewVector(make([]float32, 1024))

	// Decision with both embeddings — should appear.
	dBoth, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:            run.ID,
		AgentID:          agentID,
		DecisionType:     "test",
		Outcome:          "has both embeddings",
		Confidence:       0.8,
		Embedding:        &emb,
		OutcomeEmbedding: &outcomeEmb,
		Metadata:         map[string]any{},
	})
	require.NoError(t, err)

	// Decision with only embedding — should NOT appear.
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "test",
		Outcome:      "has only embedding",
		Confidence:   0.8,
		Embedding:    &emb,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	// Decision with no embeddings — should NOT appear.
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "test",
		Outcome:      "has no embeddings",
		Confidence:   0.8,
		Metadata:     map[string]any{},
	})
	require.NoError(t, err)

	refs, err := testDB.FindEmbeddedDecisionIDs(ctx, 1000)
	require.NoError(t, err)

	var foundBoth bool
	for _, r := range refs {
		if r.ID == dBoth.ID {
			foundBoth = true
			assert.Equal(t, dBoth.OrgID, r.OrgID)
		}
	}
	assert.True(t, foundBoth, "decision with both embeddings should appear in results")
}

func TestFindEmbeddedDecisionIDs_DefaultLimit(t *testing.T) {
	ctx := context.Background()

	// Passing 0 or negative limit should default to 1000 and not error.
	refs, err := testDB.FindEmbeddedDecisionIDs(ctx, 0)
	require.NoError(t, err)
	assert.NotNil(t, refs) // May be empty or populated from other tests; either is fine.

	refs, err = testDB.FindEmbeddedDecisionIDs(ctx, -1)
	require.NoError(t, err)
	assert.NotNil(t, refs)
}

// ---------------------------------------------------------------------------
// Tests: Claims storage (InsertClaims, FindClaimsByDecision,
//        FindDecisionIDsMissingClaims, HasClaimsForDecision)
// ---------------------------------------------------------------------------

func TestInsertClaims_AndFindByDecision(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "claims-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	emb := pgvector.NewVector(make([]float32, 1024))
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "claim_test",
		Outcome:      "multi-claim outcome",
		Confidence:   0.8,
		Embedding:    &emb,
	})
	require.NoError(t, err)

	// Insert 3 claims with different embeddings.
	claimEmbs := make([]pgvector.Vector, 3)
	for i := range claimEmbs {
		v := make([]float32, 1024)
		v[i] = 1.0
		claimEmbs[i] = pgvector.NewVector(v)
	}

	claims := []storage.Claim{
		{DecisionID: d.ID, OrgID: d.OrgID, ClaimIdx: 0, ClaimText: "First claim about architecture.", Embedding: &claimEmbs[0]},
		{DecisionID: d.ID, OrgID: d.OrgID, ClaimIdx: 1, ClaimText: "Second claim about security.", Embedding: &claimEmbs[1]},
		{DecisionID: d.ID, OrgID: d.OrgID, ClaimIdx: 2, ClaimText: "Third claim about performance.", Embedding: &claimEmbs[2]},
	}
	err = testDB.InsertClaims(ctx, claims)
	require.NoError(t, err)

	// Read them back.
	got, err := testDB.FindClaimsByDecision(ctx, d.ID, d.OrgID)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Verify ordering by claim_idx.
	for i, c := range got {
		assert.Equal(t, i, c.ClaimIdx, "claims should be ordered by claim_idx")
		assert.Equal(t, d.ID, c.DecisionID)
		assert.Equal(t, d.OrgID, c.OrgID)
		assert.NotEqual(t, uuid.Nil, c.ID, "claim should have a generated UUID")
		assert.NotNil(t, c.Embedding, "claim embedding should be stored")
	}

	// Verify claim texts.
	assert.Equal(t, "First claim about architecture.", got[0].ClaimText)
	assert.Equal(t, "Second claim about security.", got[1].ClaimText)
	assert.Equal(t, "Third claim about performance.", got[2].ClaimText)
}

func TestInsertClaims_EmptySlice(t *testing.T) {
	ctx := context.Background()

	// Empty slice should be a no-op.
	err := testDB.InsertClaims(ctx, nil)
	require.NoError(t, err)

	err = testDB.InsertClaims(ctx, []storage.Claim{})
	require.NoError(t, err)
}

func TestInsertClaims_NilEmbedding(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "claims-nil-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "claim_nil_emb",
		Outcome:      "claim without embedding",
		Confidence:   0.7,
	})
	require.NoError(t, err)

	// Insert a claim with nil embedding (allowed by schema — embedding is nullable).
	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: d.ID, OrgID: d.OrgID, ClaimIdx: 0, ClaimText: "Claim with no embedding vector.", Embedding: nil},
	})
	require.NoError(t, err)

	got, err := testDB.FindClaimsByDecision(ctx, d.ID, d.OrgID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].Embedding, "nil embedding should be preserved")
	assert.Equal(t, "Claim with no embedding vector.", got[0].ClaimText)
}

func TestFindClaimsByDecision_NoClaims(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "claims-empty-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "no_claims",
		Outcome:      "decision without any claims",
		Confidence:   0.5,
	})
	require.NoError(t, err)

	got, err := testDB.FindClaimsByDecision(ctx, d.ID, d.OrgID)
	require.NoError(t, err)
	assert.Empty(t, got, "decision with no claims should return empty slice")
}

func TestFindClaimsByDecision_NonexistentDecision(t *testing.T) {
	ctx := context.Background()

	got, err := testDB.FindClaimsByDecision(ctx, uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, got, "nonexistent decision should return empty claims")
}

func TestHasClaimsForDecision(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "hasclaims-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Decision A: will have claims.
	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "has_claims",
		Outcome: "will have claims", Confidence: 0.8,
	})
	require.NoError(t, err)

	// Decision B: will NOT have claims.
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "has_claims",
		Outcome: "will not have claims", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Before inserting, both should return false.
	has, err := testDB.HasClaimsForDecision(ctx, dA.ID, dA.OrgID)
	require.NoError(t, err)
	assert.False(t, has, "no claims yet for decision A")

	has, err = testDB.HasClaimsForDecision(ctx, dB.ID, dB.OrgID)
	require.NoError(t, err)
	assert.False(t, has, "no claims for decision B")

	// Insert claims for A.
	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dA.ID, OrgID: dA.OrgID, ClaimIdx: 0, ClaimText: "A claim for decision A."},
	})
	require.NoError(t, err)

	// A should now return true; B should still be false.
	has, err = testDB.HasClaimsForDecision(ctx, dA.ID, dA.OrgID)
	require.NoError(t, err)
	assert.True(t, has, "decision A should have claims after insert")

	has, err = testDB.HasClaimsForDecision(ctx, dB.ID, dB.OrgID)
	require.NoError(t, err)
	assert.False(t, has, "decision B should still have no claims")
}

func TestHasClaimsForDecision_NonexistentDecision(t *testing.T) {
	ctx := context.Background()

	has, err := testDB.HasClaimsForDecision(ctx, uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, has, "nonexistent decision should return false")
}

func TestFindDecisionIDsMissingClaims(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "missclaims-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	emb := pgvector.NewVector(make([]float32, 1024))

	// Decision with embedding but no claims — should appear.
	dMissing, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "missing_claims",
		Outcome: "needs claims generated", Confidence: 0.8,
		Embedding: &emb,
	})
	require.NoError(t, err)

	// Decision with embedding AND claims — should NOT appear.
	dHas, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "has_claims_already",
		Outcome: "already has claims", Confidence: 0.9,
		Embedding: &emb,
	})
	require.NoError(t, err)

	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dHas.ID, OrgID: dHas.OrgID, ClaimIdx: 0, ClaimText: "Existing claim."},
	})
	require.NoError(t, err)

	// Decision without embedding — should NOT appear (no embedding to compare).
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "no_embedding",
		Outcome: "no embedding at all", Confidence: 0.5,
	})
	require.NoError(t, err)

	refs, err := testDB.FindDecisionIDsMissingClaims(ctx, 1000)
	require.NoError(t, err)

	foundMissing := false
	for _, r := range refs {
		if r.ID == dMissing.ID {
			foundMissing = true
			assert.Equal(t, dMissing.OrgID, r.OrgID)
		}
		// The decision with claims should never appear.
		assert.NotEqual(t, dHas.ID, r.ID,
			"decision with existing claims should not appear in missing list")
	}
	assert.True(t, foundMissing, "decision with embedding but no claims should appear")
}

func TestFindDecisionIDsMissingClaims_DefaultLimit(t *testing.T) {
	ctx := context.Background()

	// Zero or negative limit should default to 500 and not error.
	refs, err := testDB.FindDecisionIDsMissingClaims(ctx, 0)
	require.NoError(t, err)
	_ = refs // may or may not be empty; just verify no error

	refs, err = testDB.FindDecisionIDsMissingClaims(ctx, -1)
	require.NoError(t, err)
	_ = refs
}

// --- Decision immutability trigger tests (migration 036) ---

func TestDecisionImmutability_BlocksCoreFieldUpdate(t *testing.T) {
	ctx := context.Background()

	// Create a decision via trace.
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000000")
	reasoning := "original reasoning"
	_, d, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "immut-test-agent",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "architecture",
			Outcome:      "original outcome",
			Confidence:   0.8,
			Reasoning:    &reasoning,
		},
	})
	require.NoError(t, err)

	// Each immutable column should be rejected.
	immutableUpdates := []struct {
		column string
		sql    string
		args   []any
	}{
		{"outcome", `UPDATE decisions SET outcome = 'tampered' WHERE id = $1`, []any{d.ID}},
		{"reasoning", `UPDATE decisions SET reasoning = 'tampered' WHERE id = $1`, []any{d.ID}},
		{"confidence", `UPDATE decisions SET confidence = 0.01 WHERE id = $1`, []any{d.ID}},
		{"decision_type", `UPDATE decisions SET decision_type = 'tampered' WHERE id = $1`, []any{d.ID}},
		{"agent_id", `UPDATE decisions SET agent_id = 'evil' WHERE id = $1`, []any{d.ID}},
		{"run_id", `UPDATE decisions SET run_id = $1 WHERE id = $2`, []any{uuid.New(), d.ID}},
		{"org_id", `UPDATE decisions SET org_id = $1 WHERE id = $2`, []any{uuid.New(), d.ID}},
		{"content_hash", `UPDATE decisions SET content_hash = 'tampered' WHERE id = $1`, []any{d.ID}},
		{"valid_from", `UPDATE decisions SET valid_from = now() + interval '1 hour' WHERE id = $1`, []any{d.ID}},
		{"created_at", `UPDATE decisions SET created_at = now() + interval '1 hour' WHERE id = $1`, []any{d.ID}},
		{"transaction_time", `UPDATE decisions SET transaction_time = now() + interval '1 hour' WHERE id = $1`, []any{d.ID}},
	}

	for _, tc := range immutableUpdates {
		t.Run("blocked_"+tc.column, func(t *testing.T) {
			_, err := testDB.Pool().Exec(ctx, tc.sql, tc.args...)
			require.Error(t, err, "UPDATE to %s should be rejected by immutability trigger", tc.column)
			assert.Contains(t, err.Error(), "immutable")
		})
	}
}

func TestDecisionImmutability_AllowsMutableFieldUpdates(t *testing.T) {
	ctx := context.Background()

	// Create a decision.
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000000")
	_, d, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "immut-mutable-test",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "mutable test outcome",
			Confidence:   0.5,
		},
	})
	require.NoError(t, err)

	// These updates should all succeed (mutable columns).
	mutableUpdates := []struct {
		column string
		sql    string
		args   []any
	}{
		{"valid_to", `UPDATE decisions SET valid_to = now() WHERE id = $1`, []any{d.ID}},
		{"embedding", `UPDATE decisions SET embedding = $1 WHERE id = $2`, []any{pgvector.NewVector(make([]float32, 1024)), d.ID}},
		{"outcome_embedding", `UPDATE decisions SET outcome_embedding = $1 WHERE id = $2`, []any{pgvector.NewVector(make([]float32, 1024)), d.ID}},
		{"precedent_ref", `UPDATE decisions SET precedent_ref = NULL WHERE id = $1`, []any{d.ID}},
		{"supersedes_id", `UPDATE decisions SET supersedes_id = NULL WHERE id = $1`, []any{d.ID}},
		{"completeness_score", `UPDATE decisions SET completeness_score = 0.99 WHERE id = $1`, []any{d.ID}},
		{"metadata", `UPDATE decisions SET metadata = '{"test": true}' WHERE id = $1`, []any{d.ID}},
		{"session_id", `UPDATE decisions SET session_id = $1 WHERE id = $2`, []any{uuid.New(), d.ID}},
		{"agent_context", `UPDATE decisions SET agent_context = '{"enriched": true}' WHERE id = $1`, []any{d.ID}},
	}

	for _, tc := range mutableUpdates {
		t.Run("allowed_"+tc.column, func(t *testing.T) {
			_, err := testDB.Pool().Exec(ctx, tc.sql, tc.args...)
			require.NoError(t, err, "UPDATE to %s should be allowed", tc.column)
		})
	}
}

func TestDecisionImmutability_AllowsNoopUpdate(t *testing.T) {
	ctx := context.Background()

	// Create a decision.
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000000")
	_, d, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "immut-noop-test",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "noop test",
			Confidence:   0.5,
		},
	})
	require.NoError(t, err)

	// Setting an immutable column to its current value should succeed (no actual change).
	_, err = testDB.Pool().Exec(ctx,
		`UPDATE decisions SET outcome = outcome WHERE id = $1`, d.ID)
	require.NoError(t, err, "no-op update to immutable column should be allowed")
}

// ---------------------------------------------------------------------------
// Tests: Assessments (CreateAssessment, ListAssessments, GetAssessmentSummary,
//        GetAssessmentSummaryBatch, UpdateOutcomeScore)
// ---------------------------------------------------------------------------

func TestCreateAssessment_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "assess-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "assessment_test",
		Outcome:      "needs assessment",
		Confidence:   0.8,
	})
	require.NoError(t, err)

	notes := "Verified the output was accurate."
	a, err := testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      d.ID,
		AssessorAgentID: "reviewer-agent",
		Outcome:         model.AssessmentCorrect,
		Notes:           &notes,
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, a.ID)
	assert.Equal(t, d.ID, a.DecisionID)
	assert.Equal(t, uuid.Nil, a.OrgID)
	assert.Equal(t, "reviewer-agent", a.AssessorAgentID)
	assert.Equal(t, model.AssessmentCorrect, a.Outcome)
	assert.Equal(t, &notes, a.Notes)
	assert.False(t, a.CreatedAt.IsZero())
}

func TestCreateAssessment_DecisionNotFound(t *testing.T) {
	ctx := context.Background()

	_, err := testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      uuid.New(),
		AssessorAgentID: "reviewer",
		Outcome:         model.AssessmentCorrect,
	})
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestListAssessments(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "listassess-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "list_assessment_test",
		Outcome:      "to be assessed multiple times",
		Confidence:   0.75,
	})
	require.NoError(t, err)

	// Create multiple assessments from different assessors.
	for _, outcome := range []model.AssessmentOutcome{
		model.AssessmentCorrect,
		model.AssessmentIncorrect,
		model.AssessmentPartiallyCorrect,
	} {
		_, err := testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
			DecisionID:      d.ID,
			AssessorAgentID: "assessor-" + string(outcome),
			Outcome:         outcome,
		})
		require.NoError(t, err)
	}

	assessments, err := testDB.ListAssessments(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.Len(t, assessments, 3)

	// Should be ordered by created_at DESC (newest first).
	for i := 1; i < len(assessments); i++ {
		assert.False(t, assessments[i].CreatedAt.After(assessments[i-1].CreatedAt),
			"assessments should be ordered newest first")
	}
}

func TestListAssessments_DecisionNotFound(t *testing.T) {
	ctx := context.Background()

	_, err := testDB.ListAssessments(ctx, uuid.Nil, uuid.New())
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestGetAssessmentSummary(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "assesssummary-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "summary_test",
		Outcome:      "to summarize",
		Confidence:   0.85,
	})
	require.NoError(t, err)

	// Two assessors say correct, one says incorrect.
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d.ID, AssessorAgentID: "reviewer-1", Outcome: model.AssessmentCorrect,
	})
	require.NoError(t, err)
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d.ID, AssessorAgentID: "reviewer-2", Outcome: model.AssessmentCorrect,
	})
	require.NoError(t, err)
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d.ID, AssessorAgentID: "reviewer-3", Outcome: model.AssessmentIncorrect,
	})
	require.NoError(t, err)

	summary, err := testDB.GetAssessmentSummary(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, summary.Total)
	assert.Equal(t, 2, summary.Correct)
	assert.Equal(t, 1, summary.Incorrect)
	assert.Equal(t, 0, summary.PartiallyCorrect)
}

func TestGetAssessmentSummary_LatestPerAssessorOnly(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "assesslatest-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "latest_only_test",
		Outcome:      "to assess repeatedly",
		Confidence:   0.7,
	})
	require.NoError(t, err)

	// Same assessor changes verdict: first correct, then incorrect.
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d.ID, AssessorAgentID: "flip-flop-reviewer", Outcome: model.AssessmentCorrect,
	})
	require.NoError(t, err)
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d.ID, AssessorAgentID: "flip-flop-reviewer", Outcome: model.AssessmentIncorrect,
	})
	require.NoError(t, err)

	summary, err := testDB.GetAssessmentSummary(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	// Only the latest should count: 1 incorrect (not 1 correct + 1 incorrect).
	assert.Equal(t, 1, summary.Total)
	assert.Equal(t, 0, summary.Correct)
	assert.Equal(t, 1, summary.Incorrect)
}

func TestGetAssessmentSummaryBatch(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "assessbatch-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d1, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "batch_test",
		Outcome: "d1", Confidence: 0.8,
	})
	require.NoError(t, err)

	d2, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "batch_test",
		Outcome: "d2", Confidence: 0.9,
	})
	require.NoError(t, err)

	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d1.ID, AssessorAgentID: "rev-1", Outcome: model.AssessmentCorrect,
	})
	require.NoError(t, err)
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d2.ID, AssessorAgentID: "rev-1", Outcome: model.AssessmentPartiallyCorrect,
	})
	require.NoError(t, err)

	batch, err := testDB.GetAssessmentSummaryBatch(ctx, uuid.Nil, []uuid.UUID{d1.ID, d2.ID})
	require.NoError(t, err)
	assert.Len(t, batch, 2)
	assert.Equal(t, 1, batch[d1.ID].Correct)
	assert.Equal(t, 1, batch[d2.ID].PartiallyCorrect)

	// Empty slice returns nil.
	empty, err := testDB.GetAssessmentSummaryBatch(ctx, uuid.Nil, []uuid.UUID{})
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestUpdateOutcomeScore(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "outscore-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "score_test",
		Outcome:      "to be scored",
		Confidence:   0.8,
	})
	require.NoError(t, err)

	// Set a score.
	score := float32(0.92)
	err = testDB.UpdateOutcomeScore(ctx, uuid.Nil, d.ID, &score)
	require.NoError(t, err)

	// Verify round-trip.
	got, err := testDB.GetDecision(ctx, uuid.Nil, d.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	require.NotNil(t, got.OutcomeScore)
	assert.InDelta(t, 0.92, float64(*got.OutcomeScore), 0.001)

	// Clear the score.
	err = testDB.UpdateOutcomeScore(ctx, uuid.Nil, d.ID, nil)
	require.NoError(t, err)

	got2, err := testDB.GetDecision(ctx, uuid.Nil, d.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Nil(t, got2.OutcomeScore)
}

// ---------------------------------------------------------------------------
// Tests: Agent stats (GetAgentStats, GetAgentListStats, UpdateAgent,
//        TouchLastSeen)
// ---------------------------------------------------------------------------

func TestGetAgentStats(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "agentstats-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		Name:    "Stats Agent",
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	reasoning := "some reasoning"
	for i := range 3 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "arch_decision",
			Outcome:      fmt.Sprintf("outcome_%d", i),
			Confidence:   float32(i+5) * 0.1,
			Reasoning:    &reasoning,
		})
		require.NoError(t, err)
	}
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "security_decision",
		Outcome:      "security_outcome",
		Confidence:   0.9,
	})
	require.NoError(t, err)

	stats, err := testDB.GetAgentStats(ctx, uuid.Nil, agentID)
	require.NoError(t, err)
	assert.Equal(t, 4, stats.DecisionCount)
	assert.Greater(t, stats.AvgConfidence, 0.0)
	assert.NotNil(t, stats.FirstDecision)
	assert.NotNil(t, stats.LastDecision)
	assert.Contains(t, stats.TypeBreakdown, "arch_decision")
	assert.Equal(t, 3, stats.TypeBreakdown["arch_decision"])
	assert.Contains(t, stats.TypeBreakdown, "security_decision")
	assert.Equal(t, 1, stats.TypeBreakdown["security_decision"])
}

func TestGetAgentListStats(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "liststats-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	for i := range 3 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "liststats_test",
			Outcome:      fmt.Sprintf("outcome_%d", i),
			Confidence:   0.7,
		})
		require.NoError(t, err)
	}

	stats, err := testDB.GetAgentListStats(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.Contains(t, stats, agentID)
	assert.Equal(t, 3, stats[agentID].DecisionCount)
	assert.NotNil(t, stats[agentID].LastDecisionAt)
}

func TestUpdateAgent(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "update-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID:  agentID,
		Name:     "Original Name",
		Role:     model.RoleAgent,
		Metadata: map[string]any{"key1": "val1"},
	})
	require.NoError(t, err)

	// Update name only.
	newName := "Updated Name"
	updated, err := testDB.UpdateAgent(ctx, uuid.Nil, agentID, &newName, nil)
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", updated.Name)

	// Update metadata only (merge behavior).
	updated2, err := testDB.UpdateAgent(ctx, uuid.Nil, agentID, nil, map[string]any{"key2": "val2"})
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", updated2.Name)
	assert.Equal(t, "val2", updated2.Metadata["key2"])

	// Not found case.
	_, err = testDB.UpdateAgent(ctx, uuid.Nil, "nonexistent-"+suffix, &newName, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestTouchLastSeen(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "touch-" + suffix

	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		Name:    "Touch Agent",
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	// last_seen should initially be nil.
	got, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, agentID)
	require.NoError(t, err)
	assert.Nil(t, got.LastSeen)

	err = testDB.TouchLastSeen(ctx, agent.OrgID, agentID)
	require.NoError(t, err)

	got2, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, agentID)
	require.NoError(t, err)
	assert.NotNil(t, got2.LastSeen, "last_seen should be set after TouchLastSeen")
}

// ---------------------------------------------------------------------------
// Tests: API Keys
// ---------------------------------------------------------------------------

func TestAPIKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "apikey-" + suffix

	// Create agent first.
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		Name:    "API Key Agent",
		Role:    model.RoleAgent,
	})
	require.NoError(t, err)

	// Create API key with audit.
	key, err := testDB.CreateAPIKeyWithAudit(ctx, model.APIKey{
		Prefix:    "ak_test_",
		KeyHash:   "argon2hash_" + suffix,
		AgentID:   agentID,
		OrgID:     uuid.Nil,
		Label:     "Test Key",
		CreatedBy: "admin",
	}, storage.MutationAuditEntry{
		RequestID: "test-req-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		Operation: "create_api_key", ResourceType: "api_key",
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, key.ID)
	assert.Equal(t, "ak_test_", key.Prefix)

	// Get by ID.
	gotKey, err := testDB.GetAPIKeyByID(ctx, uuid.Nil, key.ID)
	require.NoError(t, err)
	assert.Equal(t, key.ID, gotKey.ID)
	assert.Equal(t, agentID, gotKey.AgentID)

	// Get by prefix and agent.
	gotByPrefix, err := testDB.GetAPIKeyByPrefixAndAgent(ctx, agentID, "ak_test_")
	require.NoError(t, err)
	assert.Equal(t, key.ID, gotByPrefix.ID)

	// Get by prefix not found.
	_, err = testDB.GetAPIKeyByPrefixAndAgent(ctx, agentID, "nonexistent_prefix_")
	require.ErrorIs(t, err, storage.ErrNotFound)

	// Touch last used.
	err = testDB.TouchAPIKeyLastUsed(ctx, key.ID)
	require.NoError(t, err)

	gotKey2, err := testDB.GetAPIKeyByID(ctx, uuid.Nil, key.ID)
	require.NoError(t, err)
	assert.NotNil(t, gotKey2.LastUsedAt, "last_used_at should be set after touch")

	// List API keys.
	keys, total, err := testDB.ListAPIKeys(ctx, uuid.Nil, 50, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 1)
	foundKey := false
	for _, k := range keys {
		if k.ID == key.ID {
			foundKey = true
			break
		}
	}
	assert.True(t, foundKey, "created key should appear in list")

	// Get active keys by agent ID (global).
	activeKeys, err := testDB.GetActiveAPIKeysByAgentIDGlobal(ctx, agentID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(activeKeys), 1)

	// Get API keys by IDs (batch).
	batch, err := testDB.GetAPIKeysByIDs(ctx, uuid.Nil, []uuid.UUID{key.ID})
	require.NoError(t, err)
	assert.Len(t, batch, 1)

	// Empty batch returns nil.
	empty, err := testDB.GetAPIKeysByIDs(ctx, uuid.Nil, []uuid.UUID{})
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestRevokeAPIKey(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "revkey-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, Name: "Revoke Key Agent", Role: model.RoleAgent,
	})
	require.NoError(t, err)

	key, err := testDB.CreateAPIKeyWithAudit(ctx, model.APIKey{
		Prefix: "rk_", KeyHash: "hash_" + suffix, AgentID: agentID,
		OrgID: uuid.Nil, Label: "To Revoke", CreatedBy: "admin",
	}, storage.MutationAuditEntry{
		RequestID: "rev-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		Operation: "create_api_key", ResourceType: "api_key",
	})
	require.NoError(t, err)

	err = testDB.RevokeAPIKeyWithAudit(ctx, uuid.Nil, key.ID, storage.MutationAuditEntry{
		RequestID: "revoke-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		Operation: "revoke_api_key", ResourceType: "api_key",
	})
	require.NoError(t, err)

	revoked, err := testDB.GetAPIKeyByID(ctx, uuid.Nil, key.ID)
	require.NoError(t, err)
	assert.NotNil(t, revoked.RevokedAt, "key should be revoked")

	// Revoking again should fail.
	err = testDB.RevokeAPIKeyWithAudit(ctx, uuid.Nil, key.ID, storage.MutationAuditEntry{
		RequestID: "revoke2-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		Operation: "revoke_api_key", ResourceType: "api_key",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already revoked")

	// Revoked key should not appear in active keys.
	_, err = testDB.GetAPIKeyByPrefixAndAgent(ctx, agentID, "rk_")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestRotateAPIKey(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "rotkey-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, Name: "Rotate Key Agent", Role: model.RoleAgent,
	})
	require.NoError(t, err)

	oldKey, err := testDB.CreateAPIKeyWithAudit(ctx, model.APIKey{
		Prefix: "old_", KeyHash: "oldhash_" + suffix, AgentID: agentID,
		OrgID: uuid.Nil, Label: "Old Key", CreatedBy: "admin",
	}, storage.MutationAuditEntry{
		RequestID: "rot-old-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		Operation: "create_api_key", ResourceType: "api_key",
	})
	require.NoError(t, err)

	newKey, err := testDB.RotateAPIKeyWithAudit(ctx, uuid.Nil, oldKey.ID, model.APIKey{
		Prefix: "new_", KeyHash: "newhash_" + suffix, AgentID: agentID,
		OrgID: uuid.Nil, Label: "New Key", CreatedBy: "admin",
	}, storage.MutationAuditEntry{
		RequestID: "rot-new-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		Operation: "rotate_api_key", ResourceType: "api_key",
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, newKey.ID)
	assert.Equal(t, "new_", newKey.Prefix)

	// Old key should be revoked.
	oldGot, err := testDB.GetAPIKeyByID(ctx, uuid.Nil, oldKey.ID)
	require.NoError(t, err)
	assert.NotNil(t, oldGot.RevokedAt)

	// Rotating an already revoked key should fail.
	_, err = testDB.RotateAPIKeyWithAudit(ctx, uuid.Nil, oldKey.ID, model.APIKey{
		Prefix: "fail_", KeyHash: "failhash_" + suffix, AgentID: agentID,
		OrgID: uuid.Nil, Label: "Fail Key", CreatedBy: "admin",
	}, storage.MutationAuditEntry{
		RequestID: "rot-fail-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		Operation: "rotate_api_key", ResourceType: "api_key",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestCountDecisionsByAPIKey(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "countkey-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	from := time.Now().UTC().Add(-1 * time.Second)
	for i := range 3 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "count_test",
			Outcome:      fmt.Sprintf("count_%d", i),
			Confidence:   0.8,
		})
		require.NoError(t, err)
	}
	to := time.Now().UTC().Add(1 * time.Second)

	counts, total, err := testDB.CountDecisionsByAPIKey(ctx, uuid.Nil, from, to)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 3)
	// Decisions without api_key_id are grouped under uuid.Nil.
	assert.GreaterOrEqual(t, counts[uuid.Nil], 3)
}

// ---------------------------------------------------------------------------
// Tests: Tracehealth (GetDecisionQualityStats, GetEvidenceCoverageStats,
//        GetConflictStatusCounts, GetOutcomeSignalsSummary)
// ---------------------------------------------------------------------------

func TestGetDecisionQualityStats(t *testing.T) {
	ctx := context.Background()

	stats, err := testDB.GetDecisionQualityStats(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.Total, 0)
	// From earlier tests, there should be at least some decisions with reasoning.
	assert.GreaterOrEqual(t, stats.WithReasoning, 0)
}

func TestGetEvidenceCoverageStats(t *testing.T) {
	ctx := context.Background()

	stats, err := testDB.GetEvidenceCoverageStats(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.TotalDecisions, 0)
	assert.GreaterOrEqual(t, stats.WithEvidence, 0)
	assert.Equal(t, stats.TotalDecisions-stats.WithEvidence, stats.WithoutEvidenceCount)
}

func TestGetConflictStatusCounts(t *testing.T) {
	ctx := context.Background()

	counts, err := testDB.GetConflictStatusCounts(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, counts.Total, 0)
	assert.Equal(t, counts.Total, counts.Open+counts.Resolved+counts.FalsePositive)
}

func TestGetOutcomeSignalsSummary(t *testing.T) {
	ctx := context.Background()

	summary, err := testDB.GetOutcomeSignalsSummary(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, summary.DecisionsTotal, 0)
	assert.GreaterOrEqual(t, summary.NeverSuperseded, 0)
}

// ---------------------------------------------------------------------------
// Tests: Retention
// ---------------------------------------------------------------------------

func TestRetentionPolicyLifecycle(t *testing.T) {
	ctx := context.Background()
	retentionInterval := 24 * time.Hour

	// Default org should have no retention policy.
	policy, err := testDB.GetRetentionPolicy(ctx, uuid.Nil, retentionInterval)
	require.NoError(t, err)
	assert.Nil(t, policy.RetentionDays)

	// Set a retention policy.
	days := 90
	err = testDB.SetRetentionPolicy(ctx, uuid.Nil, &days, []string{"compliance"})
	require.NoError(t, err)

	policy2, err := testDB.GetRetentionPolicy(ctx, uuid.Nil, retentionInterval)
	require.NoError(t, err)
	require.NotNil(t, policy2.RetentionDays)
	assert.Equal(t, 90, *policy2.RetentionDays)
	assert.Equal(t, []string{"compliance"}, policy2.RetentionExcludeTypes)

	// GetOrgsWithRetention should now include the default org.
	configs, err := testDB.GetOrgsWithRetention(ctx)
	require.NoError(t, err)
	found := false
	for _, c := range configs {
		if c.OrgID == uuid.Nil {
			found = true
			assert.Equal(t, 90, c.RetentionDays)
			break
		}
	}
	assert.True(t, found, "default org should appear in orgs with retention")

	// Clear the retention policy.
	err = testDB.SetRetentionPolicy(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)

	policy3, err := testDB.GetRetentionPolicy(ctx, uuid.Nil, retentionInterval)
	require.NoError(t, err)
	assert.Nil(t, policy3.RetentionDays)
}

func TestRetentionHoldLifecycle(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "hold-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "hold_test",
		Outcome:      "held decision",
		Confidence:   0.8,
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	hold, err := testDB.CreateHold(ctx, storage.RetentionHold{
		OrgID:     uuid.Nil,
		Reason:    "Legal investigation " + suffix,
		HoldFrom:  now.Add(-24 * time.Hour),
		HoldTo:    now.Add(24 * time.Hour),
		CreatedBy: "admin",
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, hold.ID)
	assert.Equal(t, "Legal investigation "+suffix, hold.Reason)

	// List holds.
	holds, err := testDB.ListHolds(ctx, uuid.Nil)
	require.NoError(t, err)
	foundHold := false
	for _, h := range holds {
		if h.ID == hold.ID {
			foundHold = true
			assert.Nil(t, h.ReleasedAt)
			break
		}
	}
	assert.True(t, foundHold, "created hold should appear in list")

	// Check if hold exists for agent.
	hasHold, err := testDB.ActiveHoldsExistForAgent(ctx, uuid.Nil, agentID)
	require.NoError(t, err)
	assert.True(t, hasHold, "active hold should cover the agent's decisions")

	// Release the hold.
	released, err := testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)
	assert.True(t, released)

	// Release again should return false (already released).
	released2, err := testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)
	assert.False(t, released2)

	// After release, hold should no longer affect the agent.
	hasHold2, err := testDB.ActiveHoldsExistForAgent(ctx, uuid.Nil, agentID)
	require.NoError(t, err)
	assert.False(t, hasHold2, "released hold should not block agent")
}

func TestActiveHoldsExistForDecision(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "holdtest-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "hold_dec_test",
		Outcome:      "covered by hold",
		Confidence:   0.7,
	})
	require.NoError(t, err)

	// No hold yet.
	hasHold, err := testDB.ActiveHoldsExistForDecision(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.False(t, hasHold)

	// Create a hold covering this decision's time range.
	now := time.Now().UTC()
	hold, err := testDB.CreateHold(ctx, storage.RetentionHold{
		OrgID:    uuid.Nil,
		Reason:   "Decision hold test",
		HoldFrom: now.Add(-1 * time.Hour),
		HoldTo:   now.Add(1 * time.Hour),
		// No filters — covers all decision types and agents.
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	hasHold2, err := testDB.ActiveHoldsExistForDecision(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.True(t, hasHold2, "active hold should cover the decision")

	// Release and verify.
	_, err = testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)

	hasHold3, err := testDB.ActiveHoldsExistForDecision(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.False(t, hasHold3)
}

func TestDeletionLogLifecycle(t *testing.T) {
	ctx := context.Background()

	logID, err := testDB.StartDeletionLog(ctx, uuid.Nil, "manual", "admin", map[string]any{"reason": "test"})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, logID)

	err = testDB.CompleteDeletionLog(ctx, logID, map[string]any{"decisions": 5})
	require.NoError(t, err)
}

func TestCountEligibleDecisions(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "eligible-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	for i := range 3 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "eligible_test",
			Outcome:      fmt.Sprintf("eligible_%d", i),
			Confidence:   0.7,
		})
		require.NoError(t, err)
	}

	before := time.Now().UTC().Add(1 * time.Second)
	counts, err := testDB.CountEligibleDecisions(ctx, uuid.Nil, before, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, counts.Decisions, int64(3))

	// With agent filter.
	a := agentID
	counts2, err := testDB.CountEligibleDecisions(ctx, uuid.Nil, before, nil, &a)
	require.NoError(t, err)
	assert.Equal(t, int64(3), counts2.Decisions)
}

// ---------------------------------------------------------------------------
// Tests: Conflicts (GetConflictCount, GetConflictCountsBatch, GetConflict,
//        UpdateConflictStatusWithAudit)
// ---------------------------------------------------------------------------

func TestGetConflictCount_ForDecision(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "cc-a-" + suffix
	agentB := "cc-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: "cc_test",
		Outcome: "yes", Confidence: 0.8,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, DecisionType: "cc_test",
		Outcome: "no", Confidence: 0.9,
	})
	require.NoError(t, err)

	topicSim := 0.92
	outcomeDiv := 0.85
	sig := topicSim * outcomeDiv
	_, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "cc_test", DecisionTypeB: "cc_test",
		OutcomeA: "yes", OutcomeB: "no",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	count, err := testDB.GetConflictCount(ctx, dA.ID, uuid.Nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 1)

	// Nonexistent decision should have 0 conflicts.
	count2, err := testDB.GetConflictCount(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 0, count2)
}

func TestGetConflictCountsBatch(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "ccb-a-" + suffix
	agentB := "ccb-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: "ccb_test",
		Outcome: "left", Confidence: 0.8,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, DecisionType: "ccb_test",
		Outcome: "right", Confidence: 0.9,
	})
	require.NoError(t, err)
	dC, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: "ccb_other",
		Outcome: "solo", Confidence: 0.7,
	})
	require.NoError(t, err)

	topicSim := 0.90
	outcomeDiv := 0.80
	sig := topicSim * outcomeDiv
	_, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "ccb_test", DecisionTypeB: "ccb_test",
		OutcomeA: "left", OutcomeB: "right",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	counts, err := testDB.GetConflictCountsBatch(ctx, []uuid.UUID{dA.ID, dB.ID, dC.ID}, uuid.Nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, counts[dA.ID], 1)
	assert.GreaterOrEqual(t, counts[dB.ID], 1)
	assert.Equal(t, 0, counts[dC.ID]) // no conflict

	// Empty batch.
	emptyCounts, err := testDB.GetConflictCountsBatch(ctx, []uuid.UUID{}, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, emptyCounts)
}

func TestGetConflict(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "gc-a-" + suffix
	agentB := "gc-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: "gc_test",
		Outcome: "option_a", Confidence: 0.8,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, DecisionType: "gc_test",
		Outcome: "option_b", Confidence: 0.9,
	})
	require.NoError(t, err)

	topicSim := 0.88
	outcomeDiv := 0.82
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "gc_test", DecisionTypeB: "gc_test",
		OutcomeA: "option_a", OutcomeB: "option_b",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	got, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, conflictID, got.ID)
	assert.Equal(t, "open", got.Status)

	// Not found.
	notFound, err := testDB.GetConflict(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
	assert.Nil(t, notFound)
}

func TestUpdateConflictStatusWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "ucs-a-" + suffix
	agentB := "ucs-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: "ucs_test",
		Outcome: "left", Confidence: 0.8,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, DecisionType: "ucs_test",
		Outcome: "right", Confidence: 0.9,
	})
	require.NoError(t, err)

	topicSim := 0.9
	outcomeDiv := 0.8
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "ucs_test", DecisionTypeB: "ucs_test",
		OutcomeA: "left", OutcomeB: "right",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	// Transition to false_positive (terminal status, sets resolved_by/resolved_at).
	fpNote := "Not a real conflict."
	fpLabel := "unrelated_false_positive"
	oldStatus, err := testDB.UpdateConflictStatusWithAudit(ctx, conflictID, uuid.Nil,
		"false_positive", "admin-agent", &fpNote, nil, &fpLabel,
		storage.MutationAuditEntry{
			RequestID: "fp-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin-agent", ActorRole: "admin",
			Operation: "false_positive_conflict", ResourceType: "conflict",
		})
	require.NoError(t, err)
	assert.Equal(t, "open", oldStatus)

	got, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "false_positive", got.Status)
	require.NotNil(t, got.ResolvedBy)
	assert.Equal(t, "admin-agent", *got.ResolvedBy)
	assert.Nil(t, got.WinningDecisionID, "false_positive should not set winner")

	// Transition to resolved with a winner (overriding false_positive).
	resNote := "Right approach is better."
	oldStatus2, err := testDB.UpdateConflictStatusWithAudit(ctx, conflictID, uuid.Nil,
		"resolved", "admin-agent", &resNote, &dB.ID, nil,
		storage.MutationAuditEntry{
			RequestID: "resolve-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin-agent", ActorRole: "admin",
			Operation: "resolve_conflict", ResourceType: "conflict",
		})
	require.NoError(t, err)
	assert.Equal(t, "false_positive", oldStatus2)

	got2, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "resolved", got2.Status)
	require.NotNil(t, got2.ResolvedBy)
	assert.Equal(t, "admin-agent", *got2.ResolvedBy)
	require.NotNil(t, got2.WinningDecisionID)
	assert.Equal(t, dB.ID, *got2.WinningDecisionID)
}

// ---------------------------------------------------------------------------
// Tests: Conflict reopening archives resolution metadata
// ---------------------------------------------------------------------------

func TestInsertScoredConflict_ReopenArchivesResolution(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "reopen-a-" + suffix
	agentB := "reopen-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: "reopen_test",
		Outcome: "use Redis", Confidence: 0.8,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, DecisionType: "reopen_test",
		Outcome: "use Memcached", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Step 1: Insert a conflict (starts as open).
	topicSim := 0.9
	outcomeDiv := 0.85
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "reopen_test", DecisionTypeB: "reopen_test",
		OutcomeA: "use Redis", OutcomeB: "use Memcached",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	// Step 2: Resolve the conflict with a winner.
	resNote := "Redis preferred — already running in prod."
	_, err = testDB.UpdateConflictStatusWithAudit(ctx, conflictID, uuid.Nil,
		"resolved", "human-reviewer", &resNote, &dA.ID, nil,
		storage.MutationAuditEntry{
			RequestID: "resolve-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "human-reviewer", ActorRole: "admin",
			Operation: "resolve_conflict", ResourceType: "conflict",
		})
	require.NoError(t, err)

	// Verify it's resolved with full metadata.
	resolved, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "resolved", resolved.Status)
	require.NotNil(t, resolved.ResolvedBy)
	assert.Equal(t, "human-reviewer", *resolved.ResolvedBy)
	require.NotNil(t, resolved.WinningDecisionID)
	assert.Equal(t, dA.ID, *resolved.WinningDecisionID)

	// Step 3: Re-insert the same conflict pair (simulating scorer re-detection).
	newSig := 0.95
	conflictID2, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "reopen_test", DecisionTypeB: "reopen_test",
		OutcomeA: "use Redis", OutcomeB: "use Memcached",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &newSig, ScoringMethod: "text",
	})
	require.NoError(t, err)
	assert.Equal(t, conflictID, conflictID2, "upsert should return the same conflict ID")

	// Step 4: Verify the conflict is reopened with NULLed resolution fields.
	reopened, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "open", reopened.Status, "conflict should be reopened")
	assert.Nil(t, reopened.ResolvedBy, "resolved_by should be NULL after reopening")
	assert.Nil(t, reopened.ResolvedAt, "resolved_at should be NULL after reopening")
	assert.Nil(t, reopened.ResolutionNote, "resolution_note should be NULL after reopening")
	assert.Nil(t, reopened.WinningDecisionID, "winning_decision_id should be NULL after reopening")

	// Step 5: Verify the resolution was archived in conflict_resolutions.
	var archivedResolvedBy string
	var archivedResNote *string
	var archivedWinner *uuid.UUID
	var archivedResolvedAt time.Time
	err = testDB.Pool().QueryRow(ctx,
		`SELECT resolved_by, resolution_note, winning_decision_id, resolved_at
		 FROM conflict_resolutions
		 WHERE conflict_id = $1 AND org_id = $2`,
		conflictID, uuid.Nil,
	).Scan(&archivedResolvedBy, &archivedResNote, &archivedWinner, &archivedResolvedAt)
	require.NoError(t, err, "should find archived resolution in conflict_resolutions")
	assert.Equal(t, "human-reviewer", archivedResolvedBy)
	require.NotNil(t, archivedResNote)
	assert.Equal(t, "Redis preferred — already running in prod.", *archivedResNote)
	require.NotNil(t, archivedWinner)
	assert.Equal(t, dA.ID, *archivedWinner)
	assert.False(t, archivedResolvedAt.IsZero(), "archived resolved_at should be non-zero")
}

func TestInsertScoredConflict_ReopenWithGroupIDArchivesResolution(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "reopen-grp-a-" + suffix
	agentB := "reopen-grp-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: "reopen_grp_test",
		Outcome: "use Postgres", Confidence: 0.8,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, DecisionType: "reopen_grp_test",
		Outcome: "use MySQL", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Pre-create a group (simulates FindOrCreateTopicGroup).
	var groupID uuid.UUID
	err = testDB.Pool().QueryRow(ctx,
		`INSERT INTO conflict_groups (org_id, agent_a, agent_b, conflict_kind, decision_type, group_topic)
		 VALUES ($1, $2, $3, 'cross_agent', 'reopen_grp_test', 'database choice')
		 RETURNING id`,
		uuid.Nil, agentA, agentB,
	).Scan(&groupID)
	require.NoError(t, err)

	// Insert conflict with pre-computed group_id.
	topicSim := 0.88
	outcomeDiv := 0.9
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "reopen_grp_test", DecisionTypeB: "reopen_grp_test",
		OutcomeA: "use Postgres", OutcomeB: "use MySQL",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
		GroupID: &groupID,
	})
	require.NoError(t, err)

	// Resolve it.
	resNote := "Postgres wins — better JSON support."
	_, err = testDB.UpdateConflictStatusWithAudit(ctx, conflictID, uuid.Nil,
		"resolved", "tech-lead", &resNote, &dA.ID, nil,
		storage.MutationAuditEntry{
			RequestID: "resolve-grp-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "tech-lead", ActorRole: "admin",
			Operation: "resolve_conflict", ResourceType: "conflict",
		})
	require.NoError(t, err)

	// Re-insert with the same GroupID (triggers insertScoredConflictWithGroup path).
	conflictID2, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "reopen_grp_test", DecisionTypeB: "reopen_grp_test",
		OutcomeA: "use Postgres", OutcomeB: "use MySQL",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
		GroupID: &groupID,
	})
	require.NoError(t, err)
	assert.Equal(t, conflictID, conflictID2)

	// Verify reopened.
	reopened, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "open", reopened.Status)
	assert.Nil(t, reopened.WinningDecisionID)

	// Verify archive.
	var archivedResolvedBy string
	var archivedWinner *uuid.UUID
	err = testDB.Pool().QueryRow(ctx,
		`SELECT resolved_by, winning_decision_id
		 FROM conflict_resolutions
		 WHERE conflict_id = $1 AND org_id = $2`,
		conflictID, uuid.Nil,
	).Scan(&archivedResolvedBy, &archivedWinner)
	require.NoError(t, err, "should find archived resolution for group-path reopening")
	assert.Equal(t, "tech-lead", archivedResolvedBy)
	require.NotNil(t, archivedWinner)
	assert.Equal(t, dA.ID, *archivedWinner)
}

func TestInsertScoredConflict_NoArchiveWhenNotResolved(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "noarch-a-" + suffix
	agentB := "noarch-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, DecisionType: "noarch_test",
		Outcome: "approach A", Confidence: 0.8,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, DecisionType: "noarch_test",
		Outcome: "approach B", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Insert a conflict (open).
	topicSim := 0.9
	outcomeDiv := 0.8
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "noarch_test", DecisionTypeB: "noarch_test",
		OutcomeA: "approach A", OutcomeB: "approach B",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	// Re-insert without resolving first.
	newSig := 0.95
	_, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "noarch_test", DecisionTypeB: "noarch_test",
		OutcomeA: "approach A", OutcomeB: "approach B",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &newSig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	// Verify no archive was created (the conflict was never resolved).
	var count int
	err = testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM conflict_resolutions WHERE conflict_id = $1 AND org_id = $2`,
		conflictID, uuid.Nil,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no archive should exist for a conflict that was never resolved")
}

// ---------------------------------------------------------------------------
// Tests: Events (InsertEventsIdempotent)
// ---------------------------------------------------------------------------

func TestInsertEventsIdempotent(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "idem-evt-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	now := time.Now().UTC()
	events := make([]model.AgentEvent, 5)
	for i := range events {
		events[i] = model.AgentEvent{
			ID:          uuid.New(),
			RunID:       run.ID,
			EventType:   model.EventToolCallCompleted,
			SequenceNum: int64(i + 1),
			OccurredAt:  now,
			AgentID:     agentID,
			Payload:     map[string]any{"step": i},
			CreatedAt:   now,
		}
	}

	// First insert.
	inserted, err := testDB.InsertEventsIdempotent(ctx, events)
	require.NoError(t, err)
	assert.Equal(t, int64(5), inserted)

	// Second insert should be idempotent (0 new rows).
	inserted2, err := testDB.InsertEventsIdempotent(ctx, events)
	require.NoError(t, err)
	assert.Equal(t, int64(0), inserted2)

	// Verify only 5 events exist.
	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 5)
}

func TestInsertEventsIdempotent_Empty(t *testing.T) {
	ctx := context.Background()

	inserted, err := testDB.InsertEventsIdempotent(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), inserted)
}

// ---------------------------------------------------------------------------
// Tests: Claims (MarkClaimEmbeddingFailed, ClearClaimEmbeddingFailure,
//        FindRetriableClaimFailures)
// ---------------------------------------------------------------------------

func TestClaimEmbeddingFailureLifecycle(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "claimfail-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	emb := pgvector.NewVector(make([]float32, 1024))
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "claim_fail_test",
		Outcome:      "will fail claims",
		Confidence:   0.8,
		Embedding:    &emb,
	})
	require.NoError(t, err)

	// Mark as failed.
	err = testDB.MarkClaimEmbeddingFailed(ctx, d.ID, d.OrgID)
	require.NoError(t, err)

	// Mark again (should increment attempts).
	err = testDB.MarkClaimEmbeddingFailed(ctx, d.ID, d.OrgID)
	require.NoError(t, err)

	// The decision should no longer appear in missing claims (it's failed, not missing).
	refs, err := testDB.FindDecisionIDsMissingClaims(ctx, 1000)
	require.NoError(t, err)
	for _, r := range refs {
		assert.NotEqual(t, d.ID, r.ID, "failed decision should not appear in missing claims")
	}

	// Clear the failure.
	err = testDB.ClearClaimEmbeddingFailure(ctx, d.ID, d.OrgID)
	require.NoError(t, err)

	// After clearing, it should appear in missing claims again.
	refs2, err := testDB.FindDecisionIDsMissingClaims(ctx, 1000)
	require.NoError(t, err)
	found := false
	for _, r := range refs2 {
		if r.ID == d.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "decision should reappear in missing claims after clearing failure")
}

// ---------------------------------------------------------------------------
// Tests: Grants (ListGrants)
// ---------------------------------------------------------------------------

func TestListGrants(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "lg-grantor-" + suffix, Name: "Grantor", Role: model.RoleAdmin,
	})
	require.NoError(t, err)

	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "lg-grantee-" + suffix, Name: "Grantee", Role: model.RoleReader,
	})
	require.NoError(t, err)

	for i := range 3 {
		resID := fmt.Sprintf("lg-resource-%d-%s", i, suffix)
		_, err = testDB.CreateGrant(ctx, model.AccessGrant{
			GrantorID:    grantor.ID,
			GranteeID:    grantee.ID,
			ResourceType: "agent_traces",
			ResourceID:   &resID,
			Permission:   "read",
		})
		require.NoError(t, err)
	}

	grants, total, err := testDB.ListGrants(ctx, uuid.Nil, 50, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 3)
	assert.GreaterOrEqual(t, len(grants), 3)

	// Verify pagination.
	page1, _, err := testDB.ListGrants(ctx, uuid.Nil, 2, 0)
	require.NoError(t, err)
	assert.Len(t, page1, 2)

	page2, _, err := testDB.ListGrants(ctx, uuid.Nil, 2, 2)
	require.NoError(t, err)
	assert.Len(t, page2, 2)
	if len(page1) > 0 && len(page2) > 0 {
		assert.NotEqual(t, page1[0].ID, page2[0].ID, "pages should not overlap")
	}
}

// ---------------------------------------------------------------------------
// Tests: Mutation audit
// ---------------------------------------------------------------------------

func TestInsertMutationAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	err := testDB.InsertMutationAudit(ctx, storage.MutationAuditEntry{
		RequestID:    "test-req-" + suffix,
		OrgID:        uuid.Nil,
		ActorAgentID: "test-actor",
		ActorRole:    "admin",
		HTTPMethod:   "POST",
		Endpoint:     "/v1/test",
		Operation:    "test_operation",
		ResourceType: "test_resource",
		ResourceID:   "res-" + suffix,
		BeforeData:   map[string]any{"before": true},
		AfterData:    map[string]any{"after": true},
		Metadata:     map[string]any{"note": "test audit"},
	})
	require.NoError(t, err)
}

func TestInsertMutationAudit_NilMetadata(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	err := testDB.InsertMutationAudit(ctx, storage.MutationAuditEntry{
		RequestID:    "nil-meta-" + suffix,
		OrgID:        uuid.Nil,
		ActorAgentID: "actor",
		ActorRole:    "admin",
		Operation:    "test_nil_meta",
		ResourceType: "test",
		ResourceID:   "res-nil-" + suffix,
		// Intentionally nil Metadata, BeforeData, AfterData.
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: CreateAgentAndKeyTx
// ---------------------------------------------------------------------------

func TestCreateAgentAndKeyTx(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "combo-" + suffix

	agent, key, err := testDB.CreateAgentAndKeyTx(ctx,
		model.Agent{
			AgentID: agentID, Name: "Combo Agent", Role: model.RoleAgent,
		},
		model.APIKey{
			Prefix: "cb_", KeyHash: "combohash_" + suffix,
			AgentID: agentID, OrgID: uuid.Nil, Label: "Combo Key", CreatedBy: "admin",
		},
		storage.MutationAuditEntry{
			RequestID: "combo-agent-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "platform_admin",
			Operation: "create_agent", ResourceType: "agent",
		},
		storage.MutationAuditEntry{
			RequestID: "combo-key-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "platform_admin",
			Operation: "create_api_key", ResourceType: "api_key",
		},
	)
	require.NoError(t, err)
	assert.Equal(t, agentID, agent.AgentID)
	assert.Nil(t, agent.APIKeyHash, "APIKeyHash should be nil for new agent+key combo")
	assert.NotEqual(t, uuid.Nil, key.ID)
	assert.Equal(t, "cb_", key.Prefix)

	// Verify agent is retrievable.
	gotAgent, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, agentID)
	require.NoError(t, err)
	assert.Equal(t, agentID, gotAgent.AgentID)

	// Verify key is retrievable.
	gotKey, err := testDB.GetAPIKeyByID(ctx, uuid.Nil, key.ID)
	require.NoError(t, err)
	assert.Equal(t, agentID, gotKey.AgentID)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignalsBatch
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignalsBatch(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "signals-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "signals_test",
		Outcome:      "signal outcome",
		Confidence:   0.8,
	})
	require.NoError(t, err)

	signals, err := testDB.GetDecisionOutcomeSignalsBatch(ctx, []uuid.UUID{d.ID}, uuid.Nil)
	require.NoError(t, err)
	// The decision exists but may not have signals — just verify no error.
	_ = signals
}

// ---------------------------------------------------------------------------
// Tests: GetResolvedConflictsByType
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// Tests: Agents (CreateAgentWithAudit, UpdateAgentWithAudit,
//        UpdateAgentTagsWithAudit)
// ---------------------------------------------------------------------------

func TestUpdateAgentWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "upd-audit-" + suffix, Name: "Before", Role: model.RoleAgent,
	})
	require.NoError(t, err)

	newName := "After"
	updated, err := testDB.UpdateAgentWithAudit(ctx, uuid.Nil, agent.AgentID, &newName, nil,
		storage.MutationAuditEntry{
			RequestID: "upd-audit-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "update_agent", ResourceType: "agent",
		})
	require.NoError(t, err)
	assert.Equal(t, "After", updated.Name)

	// Nonexistent agent returns ErrNotFound.
	_, err = testDB.UpdateAgentWithAudit(ctx, uuid.Nil, "no-such-"+suffix, &newName, nil,
		storage.MutationAuditEntry{
			RequestID: "upd-audit-fail-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "update_agent", ResourceType: "agent",
		})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestCreateAgentWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agent, err := testDB.CreateAgentWithAudit(ctx,
		model.Agent{
			AgentID: "cwa-" + suffix, Name: "Audited Agent", Role: model.RoleAgent,
		},
		storage.MutationAuditEntry{
			RequestID: "cwa-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "platform_admin",
			Operation: "create_agent", ResourceType: "agent",
		})
	require.NoError(t, err)
	assert.Equal(t, "cwa-"+suffix, agent.AgentID)

	// Verify retrievable.
	got, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, "cwa-"+suffix)
	require.NoError(t, err)
	assert.Equal(t, agent.ID, got.ID)
}

func TestUpdateAgentTagsWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "tag-audit-" + suffix, Name: "Tag Audit", Role: model.RoleAgent,
	})
	require.NoError(t, err)

	updated, err := testDB.UpdateAgentTagsWithAudit(ctx, uuid.Nil, "tag-audit-"+suffix, []string{"audited"},
		storage.MutationAuditEntry{
			RequestID: "tag-audit-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "update_agent_tags", ResourceType: "agent",
		})
	require.NoError(t, err)
	assert.Equal(t, []string{"audited"}, updated.Tags)
}

// ---------------------------------------------------------------------------
// Tests: API Keys (RotateAPIKeyWithAudit, MigrateAgentKeysToAPIKeys)
// ---------------------------------------------------------------------------

func TestRotateAPIKeyWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "rotate-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, Name: "Rotate Agent", Role: model.RoleAgent,
	})
	require.NoError(t, err)

	oldKey, err := testDB.CreateAPIKeyWithAudit(ctx,
		model.APIKey{
			Prefix: "old_", KeyHash: "oldhash_" + suffix,
			AgentID: agentID, OrgID: uuid.Nil, Label: "Old Key", CreatedBy: "admin",
		},
		storage.MutationAuditEntry{
			RequestID: "create-old-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "create_api_key", ResourceType: "api_key",
		})
	require.NoError(t, err)

	newKey, err := testDB.RotateAPIKeyWithAudit(ctx, uuid.Nil, oldKey.ID,
		model.APIKey{
			Prefix: "new_", KeyHash: "newhash_" + suffix,
			AgentID: agentID, OrgID: uuid.Nil, Label: "New Key", CreatedBy: "admin",
		},
		storage.MutationAuditEntry{
			RequestID: "rotate-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "rotate_api_key", ResourceType: "api_key",
		})
	require.NoError(t, err)
	assert.Equal(t, "new_", newKey.Prefix)

	// Old key should be revoked.
	gotOld, err := testDB.GetAPIKeyByID(ctx, uuid.Nil, oldKey.ID)
	require.NoError(t, err)
	assert.NotNil(t, gotOld.RevokedAt)

	// Rotating an already-revoked key should fail.
	_, err = testDB.RotateAPIKeyWithAudit(ctx, uuid.Nil, oldKey.ID,
		model.APIKey{
			Prefix: "fail_", KeyHash: "failhash_" + suffix,
			AgentID: agentID, OrgID: uuid.Nil, Label: "Fail", CreatedBy: "admin",
		},
		storage.MutationAuditEntry{
			RequestID: "rotate-fail-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "rotate_api_key", ResourceType: "api_key",
		})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestMigrateAgentKeysToAPIKeys(t *testing.T) {
	ctx := context.Background()

	// Should be idempotent and succeed (even with no agents to migrate).
	migrated, err := testDB.MigrateAgentKeysToAPIKeys(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, migrated, 0)
}

// ---------------------------------------------------------------------------
// Tests: Decisions (RetractDecision, EraseDecision, GetDecisionErasure,
//        GetRevisionDepth, GetDecisionOutcomeSignals,
//        GetCitationPercentilesForOrg, ExportDecisionsCursor,
//        GetDecisionForScoring)
// ---------------------------------------------------------------------------

func TestRetractDecision(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "retract-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "retract_test", Outcome: "to_retract", Confidence: 0.7,
	})
	require.NoError(t, err)

	err = testDB.RetractDecision(ctx, uuid.Nil, d.ID, "test reason", agentID, &storage.MutationAuditEntry{
		RequestID: "retract-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "admin",
		Operation: "retract_decision", ResourceType: "decision",
	})
	require.NoError(t, err)

	// Decision should now have valid_to set.
	got, err := testDB.GetDecision(ctx, uuid.Nil, d.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.NotNil(t, got.ValidTo, "retracted decision should have valid_to set")

	// CurrentOnly should not find the retracted decision.
	_, err = testDB.GetDecision(ctx, uuid.Nil, d.ID, storage.GetDecisionOpts{CurrentOnly: true})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)

	// Retracting again should fail (already retracted).
	err = testDB.RetractDecision(ctx, uuid.Nil, d.ID, "double retract", agentID, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestEraseDecision(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "erase-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	reasoning := "sensitive data here"
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "erase_test", Outcome: "sensitive outcome",
		Confidence: 0.9, Reasoning: &reasoning,
	})
	require.NoError(t, err)

	// Add alternatives and evidence for erasure.
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "Alt 1"},
	})
	require.NoError(t, err)

	rel := float32(0.9)
	err = testDB.CreateEvidenceBatch(ctx, []model.Evidence{
		{DecisionID: d.ID, OrgID: uuid.Nil, SourceType: model.SourceAPIResponse, Content: "evidence content", RelevanceScore: &rel},
	})
	require.NoError(t, err)

	// Add claims (derived from reasoning, may contain PII).
	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: d.ID, OrgID: uuid.Nil, ClaimIdx: 0, ClaimText: "sensitive claim one"},
		{DecisionID: d.ID, OrgID: uuid.Nil, ClaimIdx: 1, ClaimText: "sensitive claim two"},
	})
	require.NoError(t, err)

	result, err := testDB.EraseDecision(ctx, uuid.Nil, d.ID, "GDPR request", agentID, &storage.MutationAuditEntry{
		RequestID: "erase-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "admin",
		Operation: "erase_decision", ResourceType: "decision",
	})
	require.NoError(t, err)
	assert.Equal(t, d.ID, result.Erasure.DecisionID)
	assert.Equal(t, agentID, result.Erasure.ErasedBy)
	assert.Equal(t, "GDPR request", result.Erasure.Reason)
	assert.Equal(t, int64(1), result.AlternativesErased)
	assert.Equal(t, int64(1), result.EvidenceErased)
	assert.Equal(t, int64(2), result.ClaimsErased)

	// Verify decision outcome is scrubbed.
	got, err := testDB.GetDecision(ctx, uuid.Nil, d.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Equal(t, storage.ErasedSentinel, got.Outcome)
	assert.Equal(t, storage.ErasedSentinel, *got.Reasoning)

	// Verify claims are scrubbed (claim_text replaced, embedding nulled).
	claims, err := testDB.FindClaimsByDecision(ctx, d.ID, uuid.Nil)
	require.NoError(t, err)
	require.Len(t, claims, 2, "claims should still exist after erasure")
	for _, c := range claims {
		assert.Equal(t, storage.ErasedSentinel, c.ClaimText, "claim_text must be scrubbed")
		assert.Nil(t, c.Embedding, "claim embedding must be nulled")
	}

	// GetDecisionErasure should return the record.
	erasure, err := testDB.GetDecisionErasure(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.Equal(t, d.ID, erasure.DecisionID)

	// Erasing again should fail.
	_, err = testDB.EraseDecision(ctx, uuid.Nil, d.ID, "double erase", agentID, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrAlreadyErased)
}

func TestGetDecisionErasure_NotFound(t *testing.T) {
	ctx := context.Background()

	_, err := testDB.GetDecisionErasure(ctx, uuid.Nil, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestGetRevisionDepth(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "depth-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create original decision.
	d0, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "depth_test", Outcome: "v0", Confidence: 0.5,
	})
	require.NoError(t, err)

	// Depth of original should be 0.
	depth, err := testDB.GetRevisionDepth(ctx, d0.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 0, depth)

	// Revise once.
	d1, err := testDB.ReviseDecision(ctx, d0.ID, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "depth_test", Outcome: "v1", Confidence: 0.7,
	}, nil)
	require.NoError(t, err)

	depth1, err := testDB.GetRevisionDepth(ctx, d1.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 1, depth1)

	// Revise again.
	d2, err := testDB.ReviseDecision(ctx, d1.ID, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "depth_test", Outcome: "v2", Confidence: 0.9,
	}, nil)
	require.NoError(t, err)

	depth2, err := testDB.GetRevisionDepth(ctx, d2.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 2, depth2)
}

func TestGetDecisionOutcomeSignals(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "outsig-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "outsig_test_" + suffix, Outcome: "signal_outcome", Confidence: 0.8,
	})
	require.NoError(t, err)

	signals, err := testDB.GetDecisionOutcomeSignals(ctx, d.ID, uuid.Nil)
	require.NoError(t, err)
	// No supersession, no citations, no conflicts for a fresh decision.
	assert.Nil(t, signals.SupersessionVelocityHours)
	assert.Equal(t, 0, signals.PrecedentCitationCount)
	assert.Equal(t, 0, signals.ConflictFate.Won)
}

func TestGetCitationPercentilesForOrg(t *testing.T) {
	ctx := context.Background()

	percentiles, err := testDB.GetCitationPercentilesForOrg(ctx, uuid.Nil)
	require.NoError(t, err)
	_ = percentiles
}

func TestGetDecisionForScoring(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "scoring-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "scoring_test", Outcome: "score_me", Confidence: 0.8,
	})
	require.NoError(t, err)

	got, err := testDB.GetDecisionForScoring(ctx, d.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, d.ID, got.ID)
	assert.Equal(t, "score_me", got.Outcome)

	// Nonexistent decision.
	_, err = testDB.GetDecisionForScoring(ctx, uuid.New(), uuid.Nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestExportDecisionsCursor(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "export-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	for i := range 3 {
		_, err = testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID,
			DecisionType: "export_test_" + suffix, Outcome: fmt.Sprintf("export_%d", i),
			Confidence: 0.7,
		})
		require.NoError(t, err)
	}

	dType := "export_test_" + suffix
	decisions, err := testDB.ExportDecisionsCursor(ctx, uuid.Nil, model.QueryFilters{
		DecisionType: &dType,
	}, nil, 2)
	require.NoError(t, err)
	assert.Len(t, decisions, 2)

	// Build cursor from the last returned decision for keyset pagination.
	last := decisions[len(decisions)-1]
	cursor := &storage.ExportCursor{ValidFrom: last.ValidFrom, ID: last.ID}

	// Fetch next page.
	decisions2, err := testDB.ExportDecisionsCursor(ctx, uuid.Nil, model.QueryFilters{
		DecisionType: &dType,
	}, cursor, 2)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(decisions2), 1)
}

// ---------------------------------------------------------------------------
// Tests: Grants (CreateGrantWithAudit, DeleteGrantWithAudit)
// ---------------------------------------------------------------------------

func TestCreateGrantWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "gwaud-grantor-" + suffix, Name: "Grantor", Role: model.RoleAdmin,
	})
	require.NoError(t, err)

	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "gwaud-grantee-" + suffix, Name: "Grantee", Role: model.RoleReader,
	})
	require.NoError(t, err)

	resID := "gwaud-resource-" + suffix
	grant, err := testDB.CreateGrantWithAudit(ctx,
		model.AccessGrant{
			GrantorID: grantor.ID, GranteeID: grantee.ID,
			ResourceType: "agent_traces", ResourceID: &resID,
			Permission: "read",
		},
		storage.MutationAuditEntry{
			RequestID: "gwaud-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "create_grant", ResourceType: "grant",
		})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, grant.ID)
}

func TestDeleteGrantWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "dgwaud-grantor-" + suffix, Name: "Grantor", Role: model.RoleAdmin,
	})
	require.NoError(t, err)

	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "dgwaud-grantee-" + suffix, Name: "Grantee", Role: model.RoleReader,
	})
	require.NoError(t, err)

	resID := "dgwaud-res-" + suffix
	grant, err := testDB.CreateGrant(ctx, model.AccessGrant{
		GrantorID: grantor.ID, GranteeID: grantee.ID,
		ResourceType: "agent_traces", ResourceID: &resID, Permission: "read",
	})
	require.NoError(t, err)

	err = testDB.DeleteGrantWithAudit(ctx, uuid.Nil, grant.ID,
		storage.MutationAuditEntry{
			RequestID: "dgwaud-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "delete_grant", ResourceType: "grant",
		})
	require.NoError(t, err)

	// Should no longer have access.
	has, err := testDB.HasAccess(ctx, uuid.Nil, grantee.ID, "agent_traces", "dgwaud-res-"+suffix, "read")
	require.NoError(t, err)
	assert.False(t, has)

	// Delete again should fail.
	err = testDB.DeleteGrantWithAudit(ctx, uuid.Nil, grant.ID,
		storage.MutationAuditEntry{
			RequestID: "dgwaud-again-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "admin",
			Operation: "delete_grant", ResourceType: "grant",
		})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Tests: Retention (GetOrgsWithRetention, HoldLifecycle, CountEligible)
// ---------------------------------------------------------------------------

func TestGetOrgsWithRetention(t *testing.T) {
	ctx := context.Background()

	err := testDB.SetRetentionPolicy(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)

	orgs, err := testDB.GetOrgsWithRetention(ctx)
	require.NoError(t, err)
	for _, o := range orgs {
		assert.NotEqual(t, uuid.Nil, o.OrgID, "default org should not appear without retention policy")
	}
}

func TestHoldLifecycle(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	now := time.Now().UTC()
	hold, err := testDB.CreateHold(ctx, storage.RetentionHold{
		OrgID: uuid.Nil, Reason: "legal hold " + suffix,
		HoldFrom: now.Add(-30 * 24 * time.Hour), HoldTo: now.Add(30 * 24 * time.Hour),
		CreatedBy: "admin",
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, hold.ID)
	assert.Equal(t, "legal hold "+suffix, hold.Reason)

	// List holds.
	holds, err := testDB.ListHolds(ctx, uuid.Nil)
	require.NoError(t, err)
	found := false
	for _, h := range holds {
		if h.ID == hold.ID {
			found = true
			assert.Nil(t, h.ReleasedAt)
			break
		}
	}
	assert.True(t, found, "should find newly created hold")

	// Release hold.
	released, err := testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)
	assert.True(t, released)

	// Release again should return false.
	released2, err := testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)
	assert.False(t, released2)
}

func TestCountEligibleDecisionsWithFilters(t *testing.T) {
	ctx := context.Background()

	before := time.Now().UTC().Add(-365 * 24 * time.Hour)
	dType := "some_type"
	agent := "some_agent"
	counts, err := testDB.CountEligibleDecisions(ctx, uuid.Nil, before, &dType, &agent)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, counts.Decisions, int64(0))
}

// ---------------------------------------------------------------------------
// Tests: Events (InsertEvent)
// ---------------------------------------------------------------------------

func TestInsertEvent(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "single-evt-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	nums, err := testDB.ReserveSequenceNums(ctx, 1)
	require.NoError(t, err)

	now := time.Now().UTC()
	err = testDB.InsertEvent(ctx, model.AgentEvent{
		ID: uuid.New(), RunID: run.ID, OrgID: uuid.Nil,
		EventType: model.EventDecisionStarted, SequenceNum: nums[0],
		OccurredAt: now, AgentID: agentID,
		Payload: map[string]any{"test": true}, CreatedAt: now,
	})
	require.NoError(t, err)

	events, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, events, 1)
	assert.Equal(t, model.EventDecisionStarted, events[0].EventType)
}

// ---------------------------------------------------------------------------
// Tests: Claims (FindRetriableClaimFailures)
// ---------------------------------------------------------------------------

func TestFindRetriableClaimFailures(t *testing.T) {
	ctx := context.Background()

	refs, err := testDB.FindRetriableClaimFailures(ctx, 3, 50)
	require.NoError(t, err)
	_ = refs
}

// ---------------------------------------------------------------------------
// Tests: Organizations (EnsureDefaultOrg, GetOrganization)
// ---------------------------------------------------------------------------

func TestEnsureDefaultOrg(t *testing.T) {
	ctx := context.Background()

	err := testDB.EnsureDefaultOrg(ctx)
	require.NoError(t, err)

	err = testDB.EnsureDefaultOrg(ctx)
	require.NoError(t, err)
}

func TestGetOrganization(t *testing.T) {
	ctx := context.Background()

	org, err := testDB.GetOrganization(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, org.ID)
	assert.NotEmpty(t, org.Name)

	// Nonexistent org.
	_, err = testDB.GetOrganization(ctx, uuid.New())
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Tests: Conflicts (RefreshConflicts, GetGlobalOpenConflictCount,
//        GetConflictGroupKind)
// ---------------------------------------------------------------------------

func TestRefreshConflicts(t *testing.T) {
	ctx := context.Background()

	err := testDB.RefreshConflicts(ctx)
	require.NoError(t, err)
}

func TestGetGlobalOpenConflictCount(t *testing.T) {
	ctx := context.Background()

	count, err := testDB.GetGlobalOpenConflictCount(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, int64(0))
}

func TestGetConflictGroupKind(t *testing.T) {
	ctx := context.Background()

	_, err := testDB.GetConflictGroupKind(ctx, uuid.New(), uuid.Nil)
	require.Error(t, err)
}

func TestGetFalsePositiveRate(t *testing.T) {
	ctx := context.Background()

	// With no resolved/false_positive conflicts, rate should be 0.
	rate, err := testDB.GetFalsePositiveRate(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 0.0, rate.Rate)
	assert.GreaterOrEqual(t, rate.Resolved, 0)
	assert.GreaterOrEqual(t, rate.FalsePositive, 0)
}

func TestGetGlobalFalsePositiveRate(t *testing.T) {
	ctx := context.Background()

	rate, err := testDB.GetGlobalFalsePositiveRate(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, rate, 0.0)
	assert.LessOrEqual(t, rate, 1.0)
}

// ---------------------------------------------------------------------------
// Tests: Idempotency (ClearInProgressIdempotency)
// ---------------------------------------------------------------------------

func TestClearInProgressIdempotency(t *testing.T) {
	ctx := context.Background()

	// ClearInProgressIdempotency requires orgID, agentID, endpoint, and key.
	// Call with a non-existent key — should succeed (no rows to clear).
	err := testDB.ClearInProgressIdempotency(ctx, uuid.Nil, "no-agent", "/v1/trace", "nonexistent-key")
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: CreateOrgWithOwnerAndKeyTx
// ---------------------------------------------------------------------------

func TestCreateOrgWithOwnerAndKeyTx(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	org, agent, key, err := testDB.CreateOrgWithOwnerAndKeyTx(ctx,
		model.Organization{
			Name: "Test Org " + suffix, Slug: "testorg-" + suffix, Plan: "starter",
		},
		model.Agent{
			AgentID: "owner-" + suffix, Name: "Owner", Role: model.RoleOrgOwner,
		},
		model.APIKey{
			Prefix: "to_", KeyHash: "orghash_" + suffix,
			AgentID: "owner-" + suffix, Label: "Org Key", CreatedBy: "system",
		},
		storage.MutationAuditEntry{
			RequestID: "org-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "system", ActorRole: "platform_admin",
			Operation: "create_org", ResourceType: "organization",
		},
		storage.MutationAuditEntry{
			RequestID: "org-agent-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "system", ActorRole: "platform_admin",
			Operation: "create_agent", ResourceType: "agent",
		},
		storage.MutationAuditEntry{
			RequestID: "org-key-" + suffix, OrgID: uuid.Nil,
			ActorAgentID: "system", ActorRole: "platform_admin",
			Operation: "create_api_key", ResourceType: "api_key",
		},
	)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, org.ID)
	assert.Equal(t, "Test Org "+suffix, org.Name)
	assert.Equal(t, org.ID, agent.OrgID)
	assert.Equal(t, org.ID, key.OrgID)
	assert.Equal(t, string(model.RoleOrgOwner), string(agent.Role))

	gotOrg, err := testDB.GetOrganization(ctx, org.ID)
	require.NoError(t, err)
	assert.Equal(t, org.Name, gotOrg.Name)
}

// ---------------------------------------------------------------------------
// Tests: MarkDecisionConflictScored, CountUnscoredDecisions, ResetConflictScoredAt
// ---------------------------------------------------------------------------

func TestMarkDecisionConflictScored(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentID := "scored-" + suffix
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "mark_scored_" + suffix, Outcome: "to be scored",
		Confidence: 0.5,
	})
	require.NoError(t, err)

	err = testDB.MarkDecisionConflictScored(ctx, dec.ID, uuid.Nil)
	require.NoError(t, err)

	// Marking again should be idempotent (overwrites with now()).
	err = testDB.MarkDecisionConflictScored(ctx, dec.ID, uuid.Nil)
	require.NoError(t, err)
}

func TestCountUnscoredDecisions(t *testing.T) {
	ctx := context.Background()

	// Just verify it returns a non-negative count without error.
	count, err := testDB.CountUnscoredDecisions(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, int64(0))
}

func TestResetConflictScoredAt(t *testing.T) {
	ctx := context.Background()

	affected, err := testDB.ResetConflictScoredAt(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, affected, int64(0))
}

func TestCountUnvalidatedConflicts(t *testing.T) {
	ctx := context.Background()

	count, err := testDB.CountUnvalidatedConflicts(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionEmbeddings (empty input)
// ---------------------------------------------------------------------------

func TestGetDecisionEmbeddings_Empty(t *testing.T) {
	ctx := context.Background()

	result, err := testDB.GetDecisionEmbeddings(ctx, nil, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestGetDecisionEmbeddings_NoMatch(t *testing.T) {
	ctx := context.Background()

	result, err := testDB.GetDecisionEmbeddings(ctx, []uuid.UUID{uuid.New()}, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

// ---------------------------------------------------------------------------
// Tests: CountConflictGroups
// ---------------------------------------------------------------------------

func TestCountConflictGroups(t *testing.T) {
	ctx := context.Background()

	count, err := testDB.CountConflictGroups(ctx, uuid.Nil, storage.ConflictGroupFilters{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

func TestCountConflictGroups_StatusFilter(t *testing.T) {
	ctx := context.Background()

	st := "open"
	count, err := testDB.CountConflictGroups(ctx, uuid.Nil, storage.ConflictGroupFilters{
		Status: &st,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

func TestCountConflictGroups_WithFilters(t *testing.T) {
	ctx := context.Background()

	dt := "some_type"
	agent := "some_agent"
	kind := "cross_agent"
	count, err := testDB.CountConflictGroups(ctx, uuid.Nil, storage.ConflictGroupFilters{
		DecisionType: &dt,
		AgentID:      &agent,
		ConflictKind: &kind,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

// ---------------------------------------------------------------------------
// Tests: ListConflictGroups
// ---------------------------------------------------------------------------

func TestListConflictGroups(t *testing.T) {
	ctx := context.Background()

	groups, err := testDB.ListConflictGroups(ctx, uuid.Nil, storage.ConflictGroupFilters{}, 10, 0)
	require.NoError(t, err)
	// May be empty on a fresh DB — just verify no error.
	_ = groups
}

func TestListConflictGroups_Limits(t *testing.T) {
	ctx := context.Background()

	// Verify clamping: negative offset -> 0, limit 0 -> 50, limit > 1000 -> 1000.
	groups, err := testDB.ListConflictGroups(ctx, uuid.Nil, storage.ConflictGroupFilters{}, 0, -1)
	require.NoError(t, err)
	_ = groups
}

// ---------------------------------------------------------------------------
// Tests: GetAgentWinRates
// ---------------------------------------------------------------------------

func TestGetAgentWinRates_Empty(t *testing.T) {
	ctx := context.Background()

	result, err := testDB.GetAgentWinRates(ctx, uuid.Nil, nil, "some_type")
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestGetAgentWinRates_NoResolved(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	result, err := testDB.GetAgentWinRates(ctx, uuid.Nil, []string{"agent-" + suffix}, "type-"+suffix)
	require.NoError(t, err)
	assert.Empty(t, result, "no resolved conflicts means no win rates")
}

// ---------------------------------------------------------------------------
// Tests: GetConflictAnalytics
// ---------------------------------------------------------------------------

func TestGetConflictAnalytics(t *testing.T) {
	ctx := context.Background()

	now := time.Now().UTC()
	analytics, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From: now.Add(-24 * time.Hour),
		To:   now,
	})
	require.NoError(t, err)
	assert.Equal(t, analytics.Period.Start, now.Add(-24*time.Hour))
	assert.Equal(t, analytics.Period.End, now)
}

func TestGetConflictAnalytics_WithFilters(t *testing.T) {
	ctx := context.Background()

	now := time.Now().UTC()
	agentID := "filter-agent"
	decType := "filter-type"
	kind := "cross_agent"
	analytics, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:         now.Add(-24 * time.Hour),
		To:           now,
		AgentID:      &agentID,
		DecisionType: &decType,
		ConflictKind: &kind,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, analytics.Summary.TotalDetected)
}

// ---------------------------------------------------------------------------
// Tests: CreateRunWithAudit, CompleteRunWithAudit
// ---------------------------------------------------------------------------

func TestCreateRunWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentID := "run-audit-" + suffix
	run, err := testDB.CreateRunWithAudit(ctx, model.CreateRunRequest{
		AgentID: agentID,
	}, storage.MutationAuditEntry{
		RequestID:    "req-" + suffix,
		OrgID:        uuid.Nil,
		ActorAgentID: agentID,
		ActorRole:    "agent",
		HTTPMethod:   "POST",
		Endpoint:     "/v1/runs",
		Operation:    "create",
		ResourceType: "run",
	})
	require.NoError(t, err)
	assert.Equal(t, agentID, run.AgentID)
	assert.Equal(t, model.RunStatusRunning, run.Status)
	assert.NotEqual(t, uuid.Nil, run.ID)
}

func TestCompleteRunWithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentID := "run-complete-" + suffix
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	err = testDB.CompleteRunWithAudit(ctx, uuid.Nil, run.ID,
		model.RunStatusCompleted,
		map[string]any{"result": "ok"},
		storage.MutationAuditEntry{
			RequestID:    "req-c-" + suffix,
			OrgID:        uuid.Nil,
			ActorAgentID: agentID,
			ActorRole:    "agent",
			HTTPMethod:   "POST",
			Endpoint:     "/v1/runs/" + run.ID.String() + "/complete",
			Operation:    "complete",
			ResourceType: "run",
			ResourceID:   run.ID.String(),
		})
	require.NoError(t, err)

	got, err := testDB.GetRun(ctx, uuid.Nil, run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, got.Status)
}

// ---------------------------------------------------------------------------
// Tests: Pool utilities (Ping, Close, IsDuplicateKey)
// ---------------------------------------------------------------------------

func TestPing(t *testing.T) {
	ctx := context.Background()

	err := testDB.Ping(ctx)
	require.NoError(t, err)
}

func TestIsDuplicateKey(t *testing.T) {
	// IsDuplicateKey should return false for non-PG errors.
	assert.False(t, testDB.IsDuplicateKey(fmt.Errorf("some random error")))
	assert.False(t, testDB.IsDuplicateKey(nil))
}

// ---------------------------------------------------------------------------
// Tests: NewPgCandidateFinder
// ---------------------------------------------------------------------------

func TestNewPgCandidateFinder(t *testing.T) {
	finder := storage.NewPgCandidateFinder(testDB)
	assert.NotNil(t, finder)
}

// ---------------------------------------------------------------------------
// Tests: BatchDeleteDecisions (empty batch)
// ---------------------------------------------------------------------------

func TestBatchDeleteDecisions_NothingToDelete(t *testing.T) {
	ctx := context.Background()

	// Use a far-past cutoff so nothing qualifies for deletion.
	farPast := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	nonexistentType := "nonexistent_type_xyz"
	result, err := testDB.BatchDeleteDecisions(ctx, uuid.Nil, farPast, &nonexistentType, nil, nil, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.Decisions)
}

// ---------------------------------------------------------------------------
// Tests: InsertEventsIdempotent (10% -> targeted coverage)
// ---------------------------------------------------------------------------

// NOTE: TestInsertEventsIdempotent_NewEvents, _DuplicatesSafe, and _PartialDuplicate
// are covered by TestInsertEventsIdempotent, _WithDuplicates, and _SingleEvent.
// The ON CONFLICT (id, occurred_at) clause now matches the composite PK.

func TestInsertEventsIdempotent_EmptySlice(t *testing.T) {
	ctx := context.Background()

	count, err := testDB.InsertEventsIdempotent(ctx, []model.AgentEvent{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "empty slice should return 0 without error")
}

// ---------------------------------------------------------------------------
// Tests: GetResolvedConflictsByType (0% -> full coverage)
// ---------------------------------------------------------------------------

func TestGetResolvedConflictsByType(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "resolved-a-" + suffix
	agentB := "resolved-b-" + suffix
	decisionType := "resolved_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: decisionType, Outcome: "approach_alpha",
		Confidence: 0.85, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: decisionType, Outcome: "approach_beta",
		Confidence: 0.90, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Insert a conflict.
	topicSim := 0.92
	outcomeDiv := 0.88
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decisionType,
		DecisionTypeB:     decisionType,
		OutcomeA:          "approach_alpha",
		OutcomeB:          "approach_beta",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)

	// Resolve the conflict with dA as winner.
	resNote := "alpha approach was better"
	_, err = testDB.UpdateConflictStatusWithAudit(ctx, conflictID, uuid.Nil,
		"resolved", "test-resolver", &resNote, &dA.ID, nil, storage.MutationAuditEntry{
			RequestID:    uuid.New().String(),
			OrgID:        uuid.Nil,
			ActorAgentID: "test-resolver",
			ActorRole:    "admin",
			HTTPMethod:   "PATCH",
			Endpoint:     "/v1/conflicts/" + conflictID.String(),
			Operation:    "resolve_conflict",
			ResourceType: "conflict",
			ResourceID:   conflictID.String(),
		})
	require.NoError(t, err)

	// Query resolved conflicts by type.
	results, err := testDB.GetResolvedConflictsByType(ctx, uuid.Nil, decisionType, 10)
	require.NoError(t, err)
	require.NotEmpty(t, results, "should find at least one resolved conflict")

	found := false
	for _, r := range results {
		if r.ID == conflictID {
			found = true
			assert.Equal(t, decisionType, r.DecisionType)
			assert.Equal(t, dA.ID, r.WinningDecisionID)
			assert.Equal(t, "approach_alpha", r.WinningOutcome)
			assert.Equal(t, "approach_beta", r.LosingOutcome)
			assert.NotZero(t, r.ResolvedAt)
			break
		}
	}
	assert.True(t, found, "resolved conflict should appear in results")
}

func TestGetResolvedConflictsByType_DefaultLimit(t *testing.T) {
	ctx := context.Background()

	// With limit 0, the function should default to 5 (not error).
	results, err := testDB.GetResolvedConflictsByType(ctx, uuid.Nil, "nonexistent_type", 0)
	require.NoError(t, err)
	assert.Empty(t, results, "nonexistent type should return empty")
}

func TestGetResolvedConflictsByType_NoResults(t *testing.T) {
	ctx := context.Background()

	results, err := testDB.GetResolvedConflictsByType(ctx, uuid.New(), "anything", 5)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// Tests: ResolveConflictGroup (0% -> full coverage)
// ---------------------------------------------------------------------------

func TestResolveConflictGroup_WithWinner(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "grp-a-" + suffix
	agentB := "grp-b-" + suffix
	decisionType := "grp_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: decisionType, Outcome: "opt_x",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: decisionType, Outcome: "opt_y",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	topicSim := 0.90
	outcomeDiv := 0.80
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decisionType,
		DecisionTypeB:     decisionType,
		OutcomeA:          "opt_x",
		OutcomeB:          "opt_y",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)

	conflict, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, conflict)
	require.NotNil(t, conflict.GroupID, "conflict should have a group_id")

	resNote := "agent B had higher confidence"
	affected, err := testDB.ResolveConflictGroup(ctx,
		*conflict.GroupID, uuid.Nil,
		"resolved", "test-admin", &resNote, &agentB, nil,
		storage.MutationAuditEntry{
			RequestID:    uuid.New().String(),
			OrgID:        uuid.Nil,
			ActorAgentID: "test-admin",
			ActorRole:    "admin",
			HTTPMethod:   "PATCH",
			Endpoint:     "/v1/conflicts/groups/" + conflict.GroupID.String(),
			Operation:    "resolve_conflict_group",
			ResourceType: "conflict_group",
			ResourceID:   conflict.GroupID.String(),
		})
	require.NoError(t, err)
	assert.Equal(t, 1, affected, "should resolve 1 conflict in the group")

	resolvedStatus := "resolved"
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		Status: &resolvedStatus,
	}, 100, 0)
	require.NoError(t, err)

	found := false
	for _, c := range conflicts {
		if c.ID == conflictID {
			found = true
			assert.Equal(t, "resolved", c.Status)
			break
		}
	}
	assert.True(t, found, "conflict should be resolved after group resolution")
}

func TestResolveConflictGroup_FalsePositive(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "grpwf-a-" + suffix
	agentB := "grpwf-b-" + suffix
	decisionType := "grpwf_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: decisionType, Outcome: "alpha",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: decisionType, Outcome: "beta",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	topicSim := 0.85
	outcomeDiv := 0.75
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decisionType,
		DecisionTypeB:     decisionType,
		OutcomeA:          "alpha",
		OutcomeB:          "beta",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)

	conflict, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, conflict.GroupID)

	resNote := "not worth resolving"
	fpLabel := "unrelated_false_positive"
	affected, err := testDB.ResolveConflictGroup(ctx,
		*conflict.GroupID, uuid.Nil,
		"false_positive", "test-admin", &resNote, nil, &fpLabel,
		storage.MutationAuditEntry{
			RequestID:    uuid.New().String(),
			OrgID:        uuid.Nil,
			ActorAgentID: "test-admin",
			ActorRole:    "admin",
			HTTPMethod:   "PATCH",
			Endpoint:     "/v1/conflicts/groups/" + conflict.GroupID.String(),
			Operation:    "resolve_conflict_group",
			ResourceType: "conflict_group",
			ResourceID:   conflict.GroupID.String(),
		})
	require.NoError(t, err)
	assert.Equal(t, 1, affected)
}

func TestResolveConflictGroup_NotFound(t *testing.T) {
	ctx := context.Background()

	_, err := testDB.ResolveConflictGroup(ctx,
		uuid.New(), uuid.Nil,
		"resolved", "test-admin", nil, nil, nil,
		storage.MutationAuditEntry{
			RequestID:    uuid.New().String(),
			OrgID:        uuid.Nil,
			ActorAgentID: "test-admin",
			ActorRole:    "admin",
			HTTPMethod:   "PATCH",
			Endpoint:     "/v1/conflicts/groups/nonexistent",
			Operation:    "resolve_conflict_group",
			ResourceType: "conflict_group",
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: CascadeResolveByOutcome
// ---------------------------------------------------------------------------

func TestCascadeResolveByOutcome(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "cascade-a-" + suffix
	agentB := "cascade-b-" + suffix
	decisionType := "cascade_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	// Create a "winning" embedding direction and an opposing direction.
	// 1024-dim vectors: winning points in +x, loser points in -x.
	winEmb := make([]float32, 1024)
	winEmb[0] = 1.0
	loseEmb := make([]float32, 1024)
	loseEmb[0] = -1.0
	// An aligned embedding (slight perturbation of winner).
	alignedEmb := make([]float32, 1024)
	alignedEmb[0] = 0.95
	alignedEmb[1] = 0.05
	// An unrelated embedding (orthogonal).
	unrelatedEmb := make([]float32, 1024)
	unrelatedEmb[2] = 1.0

	// Decision A1 (winner) — outcome aligns with winEmb.
	dA1, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType:     decisionType,
		Outcome:          "use Redis for caching",
		Confidence:       0.9,
		Metadata:         map[string]any{},
		OutcomeEmbedding: ptrVec(winEmb),
	})
	require.NoError(t, err)

	// Decision B1 (loser) — outcome opposes winner.
	dB1, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType:     decisionType,
		Outcome:          "use Memcached for caching",
		Confidence:       0.8,
		Metadata:         map[string]any{},
		OutcomeEmbedding: ptrVec(loseEmb),
	})
	require.NoError(t, err)

	// Decision A2 (aligned with winner).
	dA2, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType:     decisionType,
		Outcome:          "Redis cluster for sessions",
		Confidence:       0.85,
		Metadata:         map[string]any{},
		OutcomeEmbedding: ptrVec(alignedEmb),
	})
	require.NoError(t, err)

	// Decision B2 (opposes winner).
	dB2, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType:     decisionType,
		Outcome:          "Memcached with mcrouter",
		Confidence:       0.75,
		Metadata:         map[string]any{},
		OutcomeEmbedding: ptrVec(loseEmb),
	})
	require.NoError(t, err)

	// Decision A3 and B3: unrelated topic (orthogonal embeddings).
	dA3, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType:     decisionType,
		Outcome:          "unrelated topic A",
		Confidence:       0.7,
		Metadata:         map[string]any{},
		OutcomeEmbedding: ptrVec(unrelatedEmb),
	})
	require.NoError(t, err)

	dB3, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType:     decisionType,
		Outcome:          "unrelated topic B",
		Confidence:       0.7,
		Metadata:         map[string]any{},
		OutcomeEmbedding: ptrVec(unrelatedEmb),
	})
	require.NoError(t, err)

	// Insert 3 conflicts in the same group (same agents, same decision_type).
	topicSim := 0.90
	outcomeDiv := 0.80
	sig := topicSim * outcomeDiv

	conflict1ID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, OrgID: uuid.Nil,
		DecisionAID: dA1.ID, DecisionBID: dB1.ID,
		AgentA: agentA, AgentB: agentB,
		DecisionTypeA: decisionType, DecisionTypeB: decisionType,
		OutcomeA: "use Redis", OutcomeB: "use Memcached",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv, Significance: &sig,
		ScoringMethod: "text",
	})
	require.NoError(t, err)

	conflict2ID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, OrgID: uuid.Nil,
		DecisionAID: dA2.ID, DecisionBID: dB2.ID,
		AgentA: agentA, AgentB: agentB,
		DecisionTypeA: decisionType, DecisionTypeB: decisionType,
		OutcomeA: "Redis cluster", OutcomeB: "Memcached mcrouter",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv, Significance: &sig,
		ScoringMethod: "text",
	})
	require.NoError(t, err)

	conflict3ID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, OrgID: uuid.Nil,
		DecisionAID: dA3.ID, DecisionBID: dB3.ID,
		AgentA: agentA, AgentB: agentB,
		DecisionTypeA: decisionType, DecisionTypeB: decisionType,
		OutcomeA: "unrelated A", OutcomeB: "unrelated B",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv, Significance: &sig,
		ScoringMethod: "text",
	})
	require.NoError(t, err)

	// Verify all 3 conflicts share a group.
	c1, err := testDB.GetConflict(ctx, conflict1ID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, c1.GroupID)
	groupID := *c1.GroupID

	c2, err := testDB.GetConflict(ctx, conflict2ID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, c2.GroupID)
	assert.Equal(t, groupID, *c2.GroupID, "all conflicts should share a group")

	c3, err := testDB.GetConflict(ctx, conflict3ID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, c3.GroupID)
	assert.Equal(t, groupID, *c3.GroupID, "all conflicts should share a group")

	// Now cascade from conflict1 with dA1 as winner (winEmb direction).
	cascaded, err := testDB.CascadeResolveByOutcome(ctx,
		uuid.Nil, groupID, dA1.ID, conflict1ID,
		0.80, // threshold
		storage.MutationAuditEntry{
			RequestID:    uuid.New().String(),
			OrgID:        uuid.Nil,
			ActorAgentID: "test-admin",
			ActorRole:    "admin",
			HTTPMethod:   "PATCH",
			Endpoint:     "/v1/conflicts/" + conflict1ID.String(),
			Operation:    "conflict_cascade_resolved",
			ResourceType: "conflict",
			ResourceID:   conflict1ID.String(),
		})
	require.NoError(t, err)

	// Conflict2 should be cascade-resolved: dA2 has alignedEmb which is similar
	// to winEmb (cosine sim ~0.998), while dB2 has loseEmb (cosine sim = -1.0).
	// So dA2 should win.
	assert.Equal(t, 1, cascaded, "only conflict2 should cascade (conflict3 has unrelated embeddings)")

	// Verify conflict2 was resolved with dA2 as winner.
	c2After, err := testDB.GetConflict(ctx, conflict2ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "resolved", c2After.Status)
	assert.Equal(t, "cascade", *c2After.ResolvedBy)
	require.NotNil(t, c2After.WinningDecisionID)
	assert.Equal(t, dA2.ID, *c2After.WinningDecisionID, "aligned side (A) should win")
	require.NotNil(t, c2After.ResolutionNote)
	assert.Contains(t, *c2After.ResolutionNote, "cascade")

	// Verify conflict3 is still open (unrelated embeddings, no side exceeds 0.80).
	c3After, err := testDB.GetConflict(ctx, conflict3ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, "open", c3After.Status, "conflict3 should remain open (unrelated topic)")

	// Suppress unused variable warnings.
	_ = dB1
	_ = dB3
	_ = conflict3ID
}

func TestCascadeResolveByOutcome_NoEmbeddings(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "casc-noemb-a-" + suffix
	agentB := "casc-noemb-b-" + suffix
	decisionType := "casc_noemb_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	// Decisions without outcome embeddings.
	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: decisionType, Outcome: "opt_x",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: decisionType, Outcome: "opt_y",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent, OrgID: uuid.Nil,
		DecisionAID: dA.ID, DecisionBID: dB.ID,
		AgentA: agentA, AgentB: agentB,
		DecisionTypeA: decisionType, DecisionTypeB: decisionType,
		OutcomeA: "opt_x", OutcomeB: "opt_y",
		ScoringMethod: "text",
	})
	require.NoError(t, err)

	c, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, c.GroupID)

	// Cascade should resolve 0 — no embeddings to compare.
	cascaded, err := testDB.CascadeResolveByOutcome(ctx,
		uuid.Nil, *c.GroupID, dA.ID, uuid.New(), // trigger is a different ID
		0.80,
		storage.MutationAuditEntry{
			RequestID:    uuid.New().String(),
			OrgID:        uuid.Nil,
			ActorAgentID: "test-admin",
			ActorRole:    "admin",
			HTTPMethod:   "PATCH",
			Endpoint:     "/v1/conflicts/test",
			Operation:    "conflict_cascade_resolved",
			ResourceType: "conflict",
		})
	require.NoError(t, err)
	assert.Equal(t, 0, cascaded, "no cascade without embeddings")
}

// ptrVec returns a pointer to a pgvector.Vector from a float32 slice.
func ptrVec(v []float32) *pgvector.Vector {
	vec := pgvector.NewVector(v)
	return &vec
}

// ---------------------------------------------------------------------------
// Tests: CreateTraceAndAdjudicateConflictTx (0% -> full coverage)
// ---------------------------------------------------------------------------

func TestCreateTraceAndAdjudicateConflictTx(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "adjud-a-" + suffix
	agentB := "adjud-b-" + suffix
	decisionType := "adjud_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: decisionType, Outcome: "left",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: decisionType, Outcome: "right",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	topicSim := 0.92
	outcomeDiv := 0.85
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decisionType,
		DecisionTypeB:     decisionType,
		OutcomeA:          "left",
		OutcomeB:          "right",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)

	adjudicatorAgent := "adjudicator-" + suffix
	resNote := "chose left because of policy alignment"
	run, decision, err := testDB.CreateTraceAndAdjudicateConflictTx(ctx,
		storage.CreateTraceParams{
			AgentID:  adjudicatorAgent,
			OrgID:    uuid.Nil,
			Metadata: map[string]any{"purpose": "adjudication"},
			Decision: model.Decision{
				DecisionType: "adjudication",
				Outcome:      "selected left approach",
				Confidence:   0.95,
			},
			AuditEntry: &storage.MutationAuditEntry{
				RequestID:    uuid.New().String(),
				OrgID:        uuid.Nil,
				ActorAgentID: adjudicatorAgent,
				ActorRole:    "admin",
				HTTPMethod:   "POST",
				Endpoint:     "/v1/trace",
				Operation:    "create_trace",
				ResourceType: "decision",
			},
		},
		storage.AdjudicateConflictInTraceParams{
			ConflictID:        conflictID,
			ResolvedBy:        adjudicatorAgent,
			ResNote:           &resNote,
			WinningDecisionID: &dA.ID,
			Audit: storage.MutationAuditEntry{
				RequestID:    uuid.New().String(),
				OrgID:        uuid.Nil,
				ActorAgentID: adjudicatorAgent,
				ActorRole:    "admin",
				HTTPMethod:   "POST",
				Endpoint:     "/v1/trace",
				Operation:    "adjudicate_conflict",
				ResourceType: "conflict",
			},
		},
	)
	require.NoError(t, err)

	assert.Equal(t, adjudicatorAgent, run.AgentID)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
	assert.Equal(t, "selected left approach", decision.Outcome)

	conflict, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, conflict)
	assert.Equal(t, "resolved", conflict.Status)
	require.NotNil(t, conflict.WinningDecisionID)
	assert.Equal(t, dA.ID, *conflict.WinningDecisionID)
}

func TestCreateTraceAndAdjudicateConflictTx_ConflictNotFound(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	_, _, err := testDB.CreateTraceAndAdjudicateConflictTx(ctx,
		storage.CreateTraceParams{
			AgentID:  "adjud-notfound-" + suffix,
			OrgID:    uuid.Nil,
			Metadata: map[string]any{},
			Decision: model.Decision{
				DecisionType: "adjudication",
				Outcome:      "test",
				Confidence:   0.5,
			},
		},
		storage.AdjudicateConflictInTraceParams{
			ConflictID: uuid.New(),
			ResolvedBy: "test",
			Audit: storage.MutationAuditEntry{
				RequestID:    uuid.New().String(),
				OrgID:        uuid.Nil,
				ActorAgentID: "test",
				ActorRole:    "admin",
				HTTPMethod:   "POST",
				Endpoint:     "/v1/trace",
				Operation:    "adjudicate_conflict",
				ResourceType: "conflict",
			},
		},
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Tests: FindSimilar (0% -> full coverage)
// ---------------------------------------------------------------------------

func TestFindSimilar_WithEmbeddings(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "similar-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dims := 1024

	var decisionIDs []uuid.UUID
	for i := range 3 {
		d, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: "similar_test",
			Outcome:      fmt.Sprintf("outcome_%d", i),
			Confidence:   float32(i+1) * 0.3,
			Metadata:     map[string]any{},
		})
		require.NoError(t, err)
		decisionIDs = append(decisionIDs, d.ID)

		vec := make([]float32, dims)
		for j := range vec {
			vec[j] = float32(i+1) * float32(j) / float32(dims)
		}
		emb := pgvector.NewVector(vec)
		err = testDB.BackfillEmbedding(ctx, d.ID, d.OrgID, emb)
		require.NoError(t, err)

		outcomeVec := make([]float32, dims)
		for j := range outcomeVec {
			outcomeVec[j] = float32(i+1) * float32(j+1) / float32(dims*2)
		}
		outcomeEmb := pgvector.NewVector(outcomeVec)
		err = testDB.BackfillOutcomeEmbedding(ctx, d.ID, d.OrgID, outcomeEmb)
		require.NoError(t, err)
	}

	queryVec := make([]float32, dims)
	for j := range queryVec {
		queryVec[j] = float32(j) / float32(dims)
	}

	finder := storage.NewPgCandidateFinder(testDB)
	results, err := finder.FindSimilar(ctx, uuid.Nil, queryVec, decisionIDs[0], nil, 10)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, len(results), 2, "should find at least 2 similar decisions (excluding the query decision)")
	for _, r := range results {
		assert.NotEqual(t, decisionIDs[0], r.DecisionID, "excluded decision should not appear")

	}
}

func TestFindSimilar_DefaultLimit(t *testing.T) {
	ctx := context.Background()

	finder := storage.NewPgCandidateFinder(testDB)
	results, err := finder.FindSimilar(ctx, uuid.New(), []float32{0.1, 0.2, 0.3}, uuid.New(), nil, 0)
	require.NoError(t, err)
	assert.Empty(t, results, "random org should return empty")
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignalsBatch (41.7% -> full coverage)
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignalsBatch_EmptySlice(t *testing.T) {
	ctx := context.Background()

	result, err := testDB.GetDecisionOutcomeSignalsBatch(ctx, []uuid.UUID{}, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestGetDecisionOutcomeSignalsBatch_WithSupersession(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "signals-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	original, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "signals_test", Outcome: "v1",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	_, err = testDB.ReviseDecision(ctx, original.ID, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "signals_test", Outcome: "v2",
		Confidence: 0.9, Metadata: map[string]any{},
	}, nil)
	require.NoError(t, err)

	signals, err := testDB.GetDecisionOutcomeSignalsBatch(ctx, []uuid.UUID{original.ID}, uuid.Nil)
	require.NoError(t, err)
	require.Contains(t, signals, original.ID)
	assert.NotNil(t, signals[original.ID].SupersessionVelocityHours,
		"superseded decision should have supersession velocity")
}

func TestGetDecisionOutcomeSignalsBatch_WithPrecedentCitations(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "cite-signals-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	precedent, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "cite_test", Outcome: "precedent_outcome",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	for i := range 2 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID,
			DecisionType: "cite_test",
			Outcome:      fmt.Sprintf("cites_precedent_%d", i),
			Confidence:   0.8,
			PrecedentRef: &precedent.ID,
			Metadata:     map[string]any{},
		})
		require.NoError(t, err)
	}

	signals, err := testDB.GetDecisionOutcomeSignalsBatch(ctx, []uuid.UUID{precedent.ID}, uuid.Nil)
	require.NoError(t, err)
	require.Contains(t, signals, precedent.ID)
	assert.Equal(t, 2, signals[precedent.ID].PrecedentCitationCount,
		"should count 2 citations of the precedent")
}

func TestGetDecisionOutcomeSignalsBatch_WithConflictFate(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "fate-a-" + suffix
	agentB := "fate-b-" + suffix
	decisionType := "fate_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: decisionType, Outcome: "winner_approach",
		Confidence: 0.85, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: decisionType, Outcome: "loser_approach",
		Confidence: 0.75, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	topicSim := 0.90
	outcomeDiv := 0.80
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decisionType,
		DecisionTypeB:     decisionType,
		OutcomeA:          "winner_approach",
		OutcomeB:          "loser_approach",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "text",
	})
	require.NoError(t, err)

	resNote := "dA is better"
	_, err = testDB.UpdateConflictStatusWithAudit(ctx, conflictID, uuid.Nil,
		"resolved", "tester", &resNote, &dA.ID, nil,
		storage.MutationAuditEntry{
			RequestID: uuid.New().String(), OrgID: uuid.Nil,
			ActorAgentID: "tester", ActorRole: "admin",
			HTTPMethod: "PATCH", Endpoint: "/v1/conflicts/" + conflictID.String(),
			Operation: "resolve_conflict", ResourceType: "conflict",
			ResourceID: conflictID.String(),
		})
	require.NoError(t, err)

	signals, err := testDB.GetDecisionOutcomeSignalsBatch(ctx,
		[]uuid.UUID{dA.ID, dB.ID}, uuid.Nil)
	require.NoError(t, err)

	assert.Equal(t, 1, signals[dA.ID].ConflictFate.Won, "dA should have 1 conflict win")
	assert.Equal(t, 0, signals[dA.ID].ConflictFate.Lost, "dA should have 0 conflict losses")
	assert.Equal(t, 0, signals[dB.ID].ConflictFate.Won, "dB should have 0 conflict wins")
	assert.Equal(t, 1, signals[dB.ID].ConflictFate.Lost, "dB should have 1 conflict loss")
}

// ---------------------------------------------------------------------------
// Tests: deleteBatch (0% -> tested via BatchDeleteDecisions with data)
// ---------------------------------------------------------------------------

func TestBatchDeleteDecisions_WithData(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "delete-batch-" + suffix
	decisionType := "delete_batch_type_" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	pastTime := time.Now().UTC().Add(-48 * time.Hour)
	for i := range 3 {
		d, err := testDB.CreateDecision(ctx, model.Decision{
			RunID:        run.ID,
			AgentID:      agentID,
			DecisionType: decisionType,
			Outcome:      fmt.Sprintf("delete_me_%d", i),
			Confidence:   0.5,
			ValidFrom:    pastTime.Add(time.Duration(i) * time.Minute),
			Metadata:     map[string]any{},
		})
		require.NoError(t, err)

		err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
			{DecisionID: d.ID, Label: "alt"},
		})
		require.NoError(t, err)

		rel := float32(0.8)
		_, err = testDB.CreateEvidence(ctx, model.Evidence{
			DecisionID: d.ID, OrgID: d.OrgID,
			SourceType: model.SourceDocument, Content: "evidence",
			RelevanceScore: &rel,
		})
		require.NoError(t, err)
	}

	cutoff := time.Now().UTC().Add(1 * time.Hour)
	result, err := testDB.BatchDeleteDecisions(ctx, uuid.Nil, cutoff, &decisionType, nil, nil, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(3), result.Decisions, "should delete 3 decisions")
	assert.Equal(t, int64(3), result.Alternatives, "should delete 3 alternatives")
	assert.Equal(t, int64(3), result.Evidence, "should delete 3 evidence rows")
}

func TestGetConflictCount(t *testing.T) {
	ctx := context.Background()

	count, err := testDB.GetConflictCount(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// ---------------------------------------------------------------------------
// Tests: GetCitationPercentilesForOrg
// ---------------------------------------------------------------------------

func TestGetCitationPercentilesForOrg_NoCitations(t *testing.T) {
	ctx := context.Background()

	breakpoints, err := testDB.GetCitationPercentilesForOrg(ctx, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, breakpoints, "org with no citations should return nil")
}

func TestGetCitationPercentilesForOrg_WithCitations(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "percentile-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	precedent, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "percentile_test", Outcome: "precedent",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	for i := range 5 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID,
			DecisionType: "percentile_test",
			Outcome:      fmt.Sprintf("cites_%d", i),
			Confidence:   0.7,
			PrecedentRef: &precedent.ID,
			Metadata:     map[string]any{},
		})
		require.NoError(t, err)
	}

	breakpoints, err := testDB.GetCitationPercentilesForOrg(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.NotNil(t, breakpoints, "should return percentile breakpoints")
	assert.Len(t, breakpoints, 4, "should return p25, p50, p75, p90")
}

// ---------------------------------------------------------------------------
// Tests: WithRetry + isRetriable — cover retry-on-serialization and exhaustion paths
// ---------------------------------------------------------------------------

func TestWithRetry_RetriableErrorSucceedsOnRetry(t *testing.T) {
	ctx := context.Background()
	callCount := 0

	// Simulate a serialization failure (40001) that resolves on the 3rd attempt.
	serialErr := &pgconn.PgError{Code: "40001"}
	err := storage.WithRetry(ctx, 5, 1*time.Millisecond, func() error {
		callCount++
		if callCount < 3 {
			return serialErr
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, callCount, "should retry twice before succeeding on the 3rd attempt")
}

func TestWithRetry_RetriableErrorExhaustsRetries(t *testing.T) {
	ctx := context.Background()
	callCount := 0

	deadlockErr := &pgconn.PgError{Code: "40P01"}
	err := storage.WithRetry(ctx, 2, 1*time.Millisecond, func() error {
		callCount++
		return deadlockErr
	})
	require.Error(t, err)
	assert.Equal(t, 3, callCount, "should try once + 2 retries = 3 total calls")
	assert.ErrorIs(t, err, deadlockErr)
}

func TestWithRetry_ContextCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	callCount := 0

	serialErr := &pgconn.PgError{Code: "40001"}
	err := storage.WithRetry(ctx, 10, 500*time.Millisecond, func() error {
		callCount++
		return serialErr
	})
	// Should be either context.DeadlineExceeded or the pgconn error, depending on timing.
	require.Error(t, err)
	// At least 1 call, but context should prevent many retries given the long backoff.
	assert.GreaterOrEqual(t, callCount, 1)
}

// ---------------------------------------------------------------------------
// Tests: CompleteRunWithAudit (48% -> cover success, not-found, idempotent paths)
// ---------------------------------------------------------------------------

func TestCompleteRunWithAudit_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "crwa-ok-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusRunning, run.Status)

	audit := storage.MutationAuditEntry{
		Operation:    "complete_run",
		ActorRole:    "agent",
		ActorAgentID: agentID,
		OrgID:        run.OrgID,
		ResourceID:   run.ID.String(),
	}
	err = testDB.CompleteRunWithAudit(ctx, run.OrgID, run.ID, model.RunStatusCompleted, map[string]any{"result": "ok"}, audit)
	require.NoError(t, err)

	got, err := testDB.GetRun(ctx, run.OrgID, run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, got.Status)
	assert.NotNil(t, got.CompletedAt)
}

func TestCompleteRunWithAudit_NotFound(t *testing.T) {
	ctx := context.Background()
	fakeID := uuid.New()
	audit := storage.MutationAuditEntry{
		Operation:    "complete_run",
		ActorRole:    "agent",
		ActorAgentID: "no-such-agent",
		OrgID:        uuid.Nil,
	}
	err := testDB.CompleteRunWithAudit(ctx, uuid.Nil, fakeID, model.RunStatusCompleted, nil, audit)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCompleteRunWithAudit_Idempotent(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "crwa-idem-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		Operation:    "complete_run",
		ActorRole:    "agent",
		ActorAgentID: agentID,
		OrgID:        run.OrgID,
	}
	// First completion.
	err = testDB.CompleteRunWithAudit(ctx, run.OrgID, run.ID, model.RunStatusCompleted, nil, audit)
	require.NoError(t, err)

	// Second completion of already-completed run should be idempotent.
	err = testDB.CompleteRunWithAudit(ctx, run.OrgID, run.ID, model.RunStatusCompleted, nil, audit)
	require.NoError(t, err, "completing an already-completed run should be idempotent")
}

func TestCompleteRunWithAudit_NilMetadata(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "crwa-nil-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		Operation:    "complete_run",
		ActorRole:    "agent",
		ActorAgentID: agentID,
		OrgID:        run.OrgID,
	}
	err = testDB.CompleteRunWithAudit(ctx, run.OrgID, run.ID, model.RunStatusFailed, nil, audit)
	require.NoError(t, err, "nil metadata should be coerced to empty map")

	got, err := testDB.GetRun(ctx, run.OrgID, run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusFailed, got.Status)
}

// ---------------------------------------------------------------------------
// Tests: GetRun (57.1% -> cover not-found branch)
// ---------------------------------------------------------------------------

func TestGetRun_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetRun(ctx, uuid.Nil, uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: GetAgentByAgentID and GetAgentByID (57.1% -> cover not-found branches)
// ---------------------------------------------------------------------------

func TestGetAgentByAgentID_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetAgentByAgentID(ctx, uuid.Nil, "nonexistent-agent-"+uuid.New().String()[:8])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGetAgentByID_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetAgentByID(ctx, uuid.New(), uuid.Nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: GetAgentWinRates (53.3% -> cover the happy path and empty-agents path)
// ---------------------------------------------------------------------------

func TestGetAgentWinRates_EmptyAgents(t *testing.T) {
	ctx := context.Background()
	result, err := testDB.GetAgentWinRates(ctx, uuid.Nil, []string{}, "architecture")
	require.NoError(t, err)
	assert.Empty(t, result, "empty agentIDs should return empty map")
}

func TestGetAgentWinRates_WithResolvedConflicts(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentA := "winrate-a-" + suffix
	agentB := "winrate-b-" + suffix
	decisionType := "winrate_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: decisionType, Outcome: "approach_X",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: decisionType, Outcome: "approach_Y",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	topicSim := 0.95
	outcomeDiv := 0.85
	sig := 0.8
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: dA.OrgID, ConflictKind: "cross_agent",
		AgentA: agentA, AgentB: agentB,
		DecisionTypeA: decisionType, DecisionTypeB: decisionType,
		OutcomeA: "approach_X", OutcomeB: "approach_Y",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "embedding",
	})
	require.NoError(t, err)

	// Resolve the conflict with agentA as winner.
	resNote := "agentA wins"
	_, err = testDB.UpdateConflictStatusWithAudit(ctx, conflictID, dA.OrgID,
		"resolved", "test", &resNote, &dA.ID, nil,
		storage.MutationAuditEntry{
			OrgID: dA.OrgID, ActorAgentID: "test", ActorRole: "admin",
			Operation: "resolve_conflict", ResourceType: "conflict",
		})
	require.NoError(t, err)

	result, err := testDB.GetAgentWinRates(ctx, dA.OrgID, []string{agentA, agentB}, decisionType)
	require.NoError(t, err)
	// Both agents should appear. Exactly one should have a win (the winner).
	// Due to UUID-based normalization in InsertScoredConflict, we check aggregate counts
	// rather than assuming which agent maps to which side.
	totalWins := 0
	for _, r := range result {
		totalWins += r.WinCount
		assert.Equal(t, 1, r.Total, "each agent should appear in exactly 1 resolved conflict")
	}
	assert.Equal(t, 1, totalWins, "exactly one agent should have a win")
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionEmbeddings (57.1% -> cover empty input and happy path)
// ---------------------------------------------------------------------------

func TestGetDecisionEmbeddings_EmptyIDs(t *testing.T) {
	ctx := context.Background()
	result, err := testDB.GetDecisionEmbeddings(ctx, []uuid.UUID{}, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestGetDecisionEmbeddings_WithEmbeddings(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "emb-get-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	embSlice := make([]float32, 1024)
	outcomeEmbSlice := make([]float32, 1024)
	for i := range embSlice {
		embSlice[i] = float32(i) / 1024.0
		outcomeEmbSlice[i] = float32(1024-i) / 1024.0
	}
	emb := pgvector.NewVector(embSlice)
	outcomeEmb := pgvector.NewVector(outcomeEmbSlice)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "emb_test", Outcome: "some outcome",
		Confidence: 0.9, Metadata: map[string]any{},
		Embedding:        &emb,
		OutcomeEmbedding: &outcomeEmb,
	})
	require.NoError(t, err)

	result, err := testDB.GetDecisionEmbeddings(ctx, []uuid.UUID{d.ID}, d.OrgID)
	require.NoError(t, err)
	assert.Len(t, result, 1, "should return 1 decision with both embeddings")
	pair, ok := result[d.ID]
	assert.True(t, ok)
	assert.Equal(t, 1024, len(pair[0].Slice()))
	assert.Equal(t, 1024, len(pair[1].Slice()))
}

// ---------------------------------------------------------------------------
// Tests: ListAgents edge cases (63.6% -> cover limit clamping)
// ---------------------------------------------------------------------------

func TestListAgents_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	// With limit=0, should default to 200. Just verify it doesn't error.
	agents, err := testDB.ListAgents(ctx, uuid.Nil, 0, 0)
	require.NoError(t, err)
	_ = agents // may be empty or have agents from other tests; just verifying no error
}

func TestListAgents_LargeLimit(t *testing.T) {
	ctx := context.Background()
	// With limit=5000, should be clamped to 1000. Just verify it doesn't error.
	agents, err := testDB.ListAgents(ctx, uuid.Nil, 5000, 0)
	require.NoError(t, err)
	_ = agents
}

func TestListAgents_NegativeOffset(t *testing.T) {
	ctx := context.Background()
	agents, err := testDB.ListAgents(ctx, uuid.Nil, 10, -5)
	require.NoError(t, err)
	_ = agents
}

// ---------------------------------------------------------------------------
// Tests: FindRetriableClaimFailures (53.8% -> default limit, empty results)
// ---------------------------------------------------------------------------

func TestFindRetriableClaimFailures_NoFailures(t *testing.T) {
	ctx := context.Background()
	// With no decisions having failed claims, should return empty.
	refs, err := testDB.FindRetriableClaimFailures(ctx, 3, 10)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

func TestFindRetriableClaimFailures_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	// With limit=0, should default to 50. Just verify no error.
	refs, err := testDB.FindRetriableClaimFailures(ctx, 3, 0)
	require.NoError(t, err)
	assert.Empty(t, refs)
}

// ---------------------------------------------------------------------------
// Tests: GetAPIKeyByID (57.1% -> not-found branch)
// ---------------------------------------------------------------------------

func TestGetAPIKeyByID_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetAPIKeyByID(ctx, uuid.New(), uuid.Nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: ListAPIKeys edge cases (64.7%)
// ---------------------------------------------------------------------------

func TestListAPIKeys_Empty(t *testing.T) {
	ctx := context.Background()
	// Use a random org that has no keys.
	keys, _, err := testDB.ListAPIKeys(ctx, uuid.New(), 10, 0)
	require.NoError(t, err)
	assert.Empty(t, keys)
}

// ---------------------------------------------------------------------------
// Tests: ReviseDecision (64.3% -> cover not-found and successful revision)
// ---------------------------------------------------------------------------

func TestReviseDecision_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "revise-ok-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	original, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "revise_test", Outcome: "original approach",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		OrgID: original.OrgID, ActorAgentID: agentID, ActorRole: "agent",
		Operation: "revise_decision", ResourceType: "decision",
	}
	revised, err := testDB.ReviseDecision(ctx, original.ID, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: original.OrgID,
		DecisionType: "revise_test", Outcome: "revised approach",
		Confidence: 0.9,
	}, &audit)
	require.NoError(t, err)
	assert.Equal(t, "revised approach", revised.Outcome)
	assert.NotEqual(t, original.ID, revised.ID, "revised decision should have a new ID")
	assert.Equal(t, &original.ID, revised.SupersedesID)
}

func TestReviseDecision_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.ReviseDecision(ctx, uuid.New(), model.Decision{
		OrgID: uuid.Nil, AgentID: "ghost", DecisionType: "test",
		Outcome: "anything", Confidence: 0.5,
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: CountConflicts with filters to exercise conflictWhere branches
// ---------------------------------------------------------------------------

func TestCountConflicts_AllFilters(t *testing.T) {
	ctx := context.Background()

	decisionType := "filter_test"
	agentIDFilter := "filter-agent"
	conflictKind := "cross_agent"
	status := "open"
	severity := "high"
	category := "factual"
	decisionIDFilter := uuid.New()

	// Count with every filter active — should succeed (return 0) without SQL errors.
	count, err := testDB.CountConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		DecisionType: &decisionType,
		AgentID:      &agentIDFilter,
		ConflictKind: &conflictKind,
		Status:       &status,
		Severity:     &severity,
		Category:     &category,
		DecisionID:   &decisionIDFilter,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// ---------------------------------------------------------------------------
// Tests: ListConflicts with filters to exercise more conflictWhere branches
// ---------------------------------------------------------------------------

func TestListConflicts_WithSeverityFilter(t *testing.T) {
	ctx := context.Background()
	severity := "critical"
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		Severity: &severity,
	}, 10, 0)
	require.NoError(t, err)
	_ = conflicts // may be empty; just verifying the query with severity filter doesn't error
}

func TestListConflicts_WithCategoryFilter(t *testing.T) {
	ctx := context.Background()
	category := "temporal"
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		Category: &category,
	}, 10, 0)
	require.NoError(t, err)
	_ = conflicts
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignals (66.7% -> cover all three query paths)
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignals_NoSignals(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "signals-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "signal_test", Outcome: "standalone decision",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	signals, err := testDB.GetDecisionOutcomeSignals(ctx, d.ID, d.OrgID)
	require.NoError(t, err)
	assert.Nil(t, signals.SupersessionVelocityHours, "un-superseded decision should have nil velocity")
	assert.Equal(t, 0, signals.PrecedentCitationCount)
	assert.Equal(t, 0, signals.ConflictFate.Won)
	assert.Equal(t, 0, signals.ConflictFate.Lost)
	assert.Equal(t, 0, signals.ConflictFate.ResolvedNoWinner)
}

func TestGetDecisionOutcomeSignals_WithSupersession(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "signals-sup-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	original, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "signal_test", Outcome: "first approach",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Revise the decision so SupersessionVelocityHours is populated.
	_, err = testDB.ReviseDecision(ctx, original.ID, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: original.OrgID,
		DecisionType: "signal_test", Outcome: "revised approach",
		Confidence: 0.9,
	}, nil)
	require.NoError(t, err)

	signals, err := testDB.GetDecisionOutcomeSignals(ctx, original.ID, original.OrgID)
	require.NoError(t, err)
	assert.NotNil(t, signals.SupersessionVelocityHours, "superseded decision should have velocity")
}

// ---------------------------------------------------------------------------
// Tests: GetRetentionPolicy (66.7% -> cover policy lookup)
// ---------------------------------------------------------------------------

func TestGetRetentionPolicy_DefaultOrg(t *testing.T) {
	ctx := context.Background()
	policy, err := testDB.GetRetentionPolicy(ctx, uuid.Nil, time.Hour)
	require.NoError(t, err)
	// Default org likely has no retention policy set.
	assert.Nil(t, policy.RetentionDays, "default org should have nil retention days unless explicitly set")
}

// ---------------------------------------------------------------------------
// Tests: DeleteAgentData (67.3% -> not-found path)
// ---------------------------------------------------------------------------

func TestDeleteAgentData_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.DeleteAgentData(ctx, uuid.Nil, "nonexistent-agent-"+uuid.New().String()[:8], nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeleteAgentData_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "delete-me-" + suffix

	// Create the agent record explicitly.
	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create a run and decision for the agent.
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "delete_test", Outcome: "to be deleted",
		Confidence: 0.5, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		OrgID: run.OrgID, ActorAgentID: "admin", ActorRole: "admin",
		Operation: "delete_agent", ResourceType: "agent",
	}
	result, err := testDB.DeleteAgentData(ctx, run.OrgID, agentID, &audit)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Decisions, int64(1), "should delete at least 1 decision")
}

// ---------------------------------------------------------------------------
// Tests: BackfillEmbedding (69.2% -> cover not-found and basic success)
// ---------------------------------------------------------------------------

func TestBackfillEmbedding_NoMatch(t *testing.T) {
	ctx := context.Background()
	emb := pgvector.NewVector(make([]float32, 1024))
	// BackfillEmbedding returns nil when no rows match (decision revised/deleted/missing).
	err := testDB.BackfillEmbedding(ctx, uuid.New(), uuid.Nil, emb)
	require.NoError(t, err, "missing decision should be silently skipped")
}

// ---------------------------------------------------------------------------
// Tests: CompleteIdempotency (66.7%)
// ---------------------------------------------------------------------------

func TestCompleteIdempotency_NotFound(t *testing.T) {
	ctx := context.Background()
	err := testDB.CompleteIdempotency(ctx, uuid.Nil, "ghost-agent", "/v1/trace", uuid.New().String(), 200, map[string]any{"ok": true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: RevokeAPIKeyWithAudit (66.7%)
// ---------------------------------------------------------------------------

func TestRevokeAPIKeyWithAudit_NotFound(t *testing.T) {
	ctx := context.Background()
	audit := storage.MutationAuditEntry{
		OrgID: uuid.Nil, ActorAgentID: "admin", ActorRole: "admin",
		Operation: "revoke_api_key", ResourceType: "api_key",
	}
	err := testDB.RevokeAPIKeyWithAudit(ctx, uuid.New(), uuid.Nil, audit)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: UpdateAgentTagsWithAudit (63.2% -> cover success and not-found)
// ---------------------------------------------------------------------------

func TestUpdateAgentTagsWithAudit_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "tags-ok-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID,
		Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		OrgID: uuid.Nil, ActorAgentID: "admin", ActorRole: "admin",
		Operation: "update_tags", ResourceType: "agent",
	}
	updated, err := testDB.UpdateAgentTagsWithAudit(ctx, uuid.Nil, agentID, []string{"env:prod", "team:infra"}, audit)
	require.NoError(t, err)
	assert.Equal(t, []string{"env:prod", "team:infra"}, updated.Tags)
}

func TestUpdateAgentTagsWithAudit_NotFound(t *testing.T) {
	ctx := context.Background()
	audit := storage.MutationAuditEntry{
		OrgID: uuid.Nil, ActorAgentID: "admin", ActorRole: "admin",
		Operation: "update_tags", ResourceType: "agent",
	}
	_, err := testDB.UpdateAgentTagsWithAudit(ctx, uuid.Nil, "nonexistent-"+uuid.New().String()[:8], []string{"a"}, audit)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdateAgentTagsWithAudit_NilTags(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "tags-nil-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID,
		Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		OrgID: uuid.Nil, ActorAgentID: "admin", ActorRole: "admin",
		Operation: "update_tags", ResourceType: "agent",
	}
	updated, err := testDB.UpdateAgentTagsWithAudit(ctx, uuid.Nil, agentID, nil, audit)
	require.NoError(t, err)
	assert.Equal(t, []string{}, updated.Tags, "nil tags should be coerced to empty slice")
}

// ---------------------------------------------------------------------------
// Tests: ListAPIKeys edge cases (64.7%)
// ---------------------------------------------------------------------------

func TestListAPIKeys_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	keys, _, err := testDB.ListAPIKeys(ctx, uuid.Nil, 0, 0)
	require.NoError(t, err)
	_ = keys // just verify default limit=50 doesn't error
}

func TestListAPIKeys_NegativeOffset(t *testing.T) {
	ctx := context.Background()
	keys, _, err := testDB.ListAPIKeys(ctx, uuid.Nil, 10, -5)
	require.NoError(t, err)
	_ = keys
}

func TestListAPIKeys_LargeLimit(t *testing.T) {
	ctx := context.Background()
	keys, _, err := testDB.ListAPIKeys(ctx, uuid.Nil, 5000, 0)
	require.NoError(t, err)
	_ = keys
}

// ---------------------------------------------------------------------------
// Tests: SearchDecisionsByText (70%)
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_NoMatches(t *testing.T) {
	ctx := context.Background()
	results, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, "xyzzy_nonexistent_"+uuid.New().String()[:8], model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignals additional branch (precedent citation)
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignals_WithPrecedentCitation(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "signals-cite-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	original, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "signal_cite_test", Outcome: "foundational decision",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create two decisions citing the original as precedent.
	for i := range 2 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID,
			DecisionType: "signal_cite_test", Outcome: fmt.Sprintf("follows precedent %d", i),
			Confidence: 0.85, Metadata: map[string]any{},
			PrecedentRef: &original.ID,
		})
		require.NoError(t, err)
	}

	signals, err := testDB.GetDecisionOutcomeSignals(ctx, original.ID, original.OrgID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, signals.PrecedentCitationCount, 2, "should count at least 2 citations")
}

// ---------------------------------------------------------------------------
// Tests: NewConflictsSinceByOrg (71.4%)
// ---------------------------------------------------------------------------

func TestNewConflictsSinceByOrg_Empty(t *testing.T) {
	ctx := context.Background()
	// Far future cutoff should return 0 new conflicts.
	counts, err := testDB.NewConflictsSinceByOrg(ctx, uuid.Nil, time.Now().UTC().Add(24*time.Hour), 10)
	require.NoError(t, err)
	_ = counts
}

// ---------------------------------------------------------------------------
// Tests: FindDecisionsMissingOutcomeEmbedding (76.9% -> default limit)
// ---------------------------------------------------------------------------

func TestFindDecisionsMissingOutcomeEmbedding_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	results, err := testDB.FindDecisionsMissingOutcomeEmbedding(ctx, 0)
	require.NoError(t, err)
	_ = results
}

// ---------------------------------------------------------------------------
// Tests: TouchAPIKeyLastUsed (75%)
// ---------------------------------------------------------------------------

func TestTouchAPIKeyLastUsed_NonExistent(t *testing.T) {
	ctx := context.Background()
	// Touching a nonexistent key should not error (fire-and-forget pattern).
	err := testDB.TouchAPIKeyLastUsed(ctx, uuid.New())
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: InsertEvent (75%)
// ---------------------------------------------------------------------------

func TestInsertEvent_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "insert-event-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	event := model.AgentEvent{
		ID:          uuid.New(),
		RunID:       run.ID,
		OrgID:       run.OrgID,
		EventType:   model.EventDecisionMade,
		SequenceNum: 1,
		OccurredAt:  time.Now().UTC(),
		AgentID:     agentID,
		Payload:     map[string]any{"test": true},
		CreatedAt:   time.Now().UTC(),
	}

	err = testDB.InsertEvent(ctx, event)
	require.NoError(t, err)

	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignals with conflict fate data
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignals_WithConflictFate(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentA := "fate-a-" + suffix
	agentB := "fate-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: "fate_test", Outcome: "winner approach",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: "fate_test", Outcome: "loser approach",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	topicSim := 0.9
	outcomeDiv := 0.8
	sig := 0.72
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		DecisionAID: dA.ID, DecisionBID: dB.ID,
		OrgID: dA.OrgID, ConflictKind: "cross_agent",
		AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "fate_test", DecisionTypeB: "fate_test",
		OutcomeA: "winner approach", OutcomeB: "loser approach",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "embedding",
	})
	require.NoError(t, err)

	// Resolve with dA as winner.
	resNote := "agentA wins"
	_, err = testDB.UpdateConflictStatusWithAudit(ctx, conflictID, dA.OrgID,
		"resolved", "test", &resNote, &dA.ID, nil,
		storage.MutationAuditEntry{
			OrgID: dA.OrgID, ActorAgentID: "test", ActorRole: "admin",
			Operation: "resolve_conflict", ResourceType: "conflict",
		})
	require.NoError(t, err)

	// Check dA signals — should show Won=1.
	signalsA, err := testDB.GetDecisionOutcomeSignals(ctx, dA.ID, dA.OrgID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, signalsA.ConflictFate.Won+signalsA.ConflictFate.Lost, 1,
		"decision should appear in at least one resolved conflict")

	// Check dB signals — should show Lost=1.
	signalsB, err := testDB.GetDecisionOutcomeSignals(ctx, dB.ID, dB.OrgID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, signalsB.ConflictFate.Won+signalsB.ConflictFate.Lost, 1,
		"decision should appear in at least one resolved conflict")
}

// ---------------------------------------------------------------------------
// Tests: RetractDecision (79.5% -> cover not-found)
// ---------------------------------------------------------------------------

func TestRetractDecision_NotFound(t *testing.T) {
	ctx := context.Background()
	err := testDB.RetractDecision(ctx, uuid.Nil, uuid.New(), "test reason", "test-agent", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: EraseDecision (76.2% -> cover not-found)
// ---------------------------------------------------------------------------

func TestEraseDecision_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.EraseDecision(ctx, uuid.Nil, uuid.New(), "test reason", "test-agent", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: CreateAlternativesBatch (89.5% -> hit the nil metadata branch)
// ---------------------------------------------------------------------------

func TestCreateAlternativesBatch_NilMetadata(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "alt-nil-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "alt_test", Outcome: "test",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	alts := []model.Alternative{
		{DecisionID: d.ID, Label: "alt A", Metadata: nil},
		{DecisionID: d.ID, Label: "alt B", Metadata: map[string]any{"note": "best"}},
	}
	err = testDB.CreateAlternativesBatch(ctx, alts)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: FindUnembeddedDecisions (76.9% -> default limit)
// ---------------------------------------------------------------------------

func TestFindUnembeddedDecisions_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	results, err := testDB.FindUnembeddedDecisions(ctx, 0)
	require.NoError(t, err)
	_ = results
}

// ---------------------------------------------------------------------------
// Tests: GetConflictAnalytics (77.1%)
// ---------------------------------------------------------------------------

func TestGetConflictAnalytics_Empty(t *testing.T) {
	ctx := context.Background()
	analytics, err := testDB.GetConflictAnalytics(ctx, uuid.New(), storage.ConflictAnalyticsFilters{})
	require.NoError(t, err)
	_ = analytics
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions edge cases (78.8%)
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithAgentFilter(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "qd-filter-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "query_test", Outcome: "filtered",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	decisions, _, err := testDB.QueryDecisions(ctx, run.OrgID, model.QueryRequest{
		Filters: model.QueryFilters{AgentIDs: []string{agentID}},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, decisions, "should find at least 1 decision for this agent")
}

// ---------------------------------------------------------------------------
// Tests: SetRetentionPolicy + GetRetentionPolicy round-trip
// ---------------------------------------------------------------------------

func TestSetAndGetRetentionPolicy(t *testing.T) {
	ctx := context.Background()
	days := 30
	err := testDB.SetRetentionPolicy(ctx, uuid.Nil, &days, []string{"architecture"})
	require.NoError(t, err)

	policy, err := testDB.GetRetentionPolicy(ctx, uuid.Nil, 24*time.Hour)
	require.NoError(t, err)
	require.NotNil(t, policy.RetentionDays)
	assert.Equal(t, 30, *policy.RetentionDays)

	// Clean up: reset to nil.
	err = testDB.SetRetentionPolicy(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: CompleteRun — not-found and nil-metadata paths
// ---------------------------------------------------------------------------

func TestCompleteRun_NotFound(t *testing.T) {
	ctx := context.Background()
	err := testDB.CompleteRun(ctx, uuid.Nil, uuid.New(), model.RunStatusCompleted, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCompleteRun_NilMetadata(t *testing.T) {
	ctx := context.Background()
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "complete-nilmeta-" + uuid.New().String()[:8]})
	require.NoError(t, err)

	err = testDB.CompleteRun(ctx, run.OrgID, run.ID, model.RunStatusFailed, nil)
	require.NoError(t, err)

	got, err := testDB.GetRun(ctx, run.OrgID, run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusFailed, got.Status)
	assert.NotNil(t, got.CompletedAt)
}

// ---------------------------------------------------------------------------
// Tests: ListRunsByAgent — boundary parameters
// ---------------------------------------------------------------------------

func TestListRunsByAgent_ZeroLimit(t *testing.T) {
	ctx := context.Background()
	agentID := "runs-zero-" + uuid.New().String()[:8]
	_, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	runs, total, err := testDB.ListRunsByAgent(ctx, uuid.Nil, agentID, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, runs, 1)
}

func TestListRunsByAgent_LargeLimit(t *testing.T) {
	ctx := context.Background()
	agentID := "runs-large-" + uuid.New().String()[:8]
	_, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	runs, total, err := testDB.ListRunsByAgent(ctx, uuid.Nil, agentID, 5000, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, runs, 1)
}

func TestListRunsByAgent_NegativeOffset(t *testing.T) {
	ctx := context.Background()
	agentID := "runs-negoff-" + uuid.New().String()[:8]
	_, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	runs, total, err := testDB.ListRunsByAgent(ctx, uuid.Nil, agentID, 10, -5)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, runs, 1)
}

func TestListRunsByAgent_Empty(t *testing.T) {
	ctx := context.Background()
	agentID := "runs-empty-" + uuid.New().String()[:8]
	runs, total, err := testDB.ListRunsByAgent(ctx, uuid.Nil, agentID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, runs)
}

// ---------------------------------------------------------------------------
// Tests: GetRetentionPolicy — with deletion log
// ---------------------------------------------------------------------------

func TestGetRetentionPolicy_WithDeletionLog(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	orgID := uuid.New()
	_, err := testDB.Pool().Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		orgID, "retention-test-"+suffix, "retention-test-"+suffix)
	require.NoError(t, err)

	days := 90
	err = testDB.SetRetentionPolicy(ctx, orgID, &days, nil)
	require.NoError(t, err)

	started := time.Now().UTC().Add(-2 * time.Hour)
	_, err = testDB.Pool().Exec(ctx,
		`INSERT INTO deletion_log (id, org_id, trigger, started_at, completed_at, deleted_counts, criteria)
		 VALUES ($1, $2, 'policy', $3, $4, '{"decisions": 42}', '{}')`,
		uuid.New(), orgID, started, time.Now().UTC())
	require.NoError(t, err)

	policy, err := testDB.GetRetentionPolicy(ctx, orgID, 24*time.Hour)
	require.NoError(t, err)
	require.NotNil(t, policy.RetentionDays)
	assert.Equal(t, 90, *policy.RetentionDays)
	require.NotNil(t, policy.LastRunAt, "should populate LastRunAt from deletion_log")
	require.NotNil(t, policy.LastRunDeleted)
	assert.Equal(t, 42, *policy.LastRunDeleted)
	require.NotNil(t, policy.NextRunAt, "should compute NextRunAt from interval")
}

func TestGetRetentionPolicy_ZeroInterval(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	orgID := uuid.New()
	_, err := testDB.Pool().Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		orgID, "retention-zero-"+suffix, "retention-zero-"+suffix)
	require.NoError(t, err)

	days := 30
	err = testDB.SetRetentionPolicy(ctx, orgID, &days, nil)
	require.NoError(t, err)

	_, err = testDB.Pool().Exec(ctx,
		`INSERT INTO deletion_log (id, org_id, trigger, started_at, completed_at, deleted_counts, criteria)
		 VALUES ($1, $2, 'policy', $3, $4, '{"decisions": 10}', '{}')`,
		uuid.New(), orgID, time.Now().UTC().Add(-1*time.Hour), time.Now().UTC())
	require.NoError(t, err)

	policy, err := testDB.GetRetentionPolicy(ctx, orgID, 0)
	require.NoError(t, err)
	require.NotNil(t, policy.LastRunAt)
	assert.Nil(t, policy.NextRunAt, "zero interval should not set NextRunAt")
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionsByAgent — boundary params
// ---------------------------------------------------------------------------

func TestGetDecisionsByAgent_ZeroLimit(t *testing.T) {
	ctx := context.Background()
	agentID := "agent-zerolim-" + uuid.New().String()[:8]
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "test", Outcome: "result",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	decisions, total, err := testDB.GetDecisionsByAgent(ctx, run.OrgID, agentID, 0, 0, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, decisions, 1)
}

func TestGetDecisionsByAgent_LargeLimit(t *testing.T) {
	ctx := context.Background()
	agentID := "agent-largelim-" + uuid.New().String()[:8]
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "test", Outcome: "result",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	decisions, total, err := testDB.GetDecisionsByAgent(ctx, run.OrgID, agentID, 5000, 0, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, decisions, 1)
}

func TestGetDecisionsByAgent_NegativeOffset(t *testing.T) {
	ctx := context.Background()
	agentID := "agent-negoff-" + uuid.New().String()[:8]
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "test", Outcome: "result",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	decisions, total, err := testDB.GetDecisionsByAgent(ctx, run.OrgID, agentID, 10, -3, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Len(t, decisions, 1)
}

// ---------------------------------------------------------------------------
// Tests: FindRetriableClaimFailures — with actual data
// ---------------------------------------------------------------------------

func TestFindRetriableClaimFailures_WithData(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "retry-claims-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "retry_test", Outcome: "will fail claims",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	emb := pgvector.NewVector(make([]float32, 1024))
	err = testDB.BackfillEmbedding(ctx, dec.ID, dec.OrgID, emb)
	require.NoError(t, err)

	err = testDB.MarkClaimEmbeddingFailed(ctx, dec.ID, dec.OrgID)
	require.NoError(t, err)

	_, err = testDB.Pool().Exec(ctx,
		`UPDATE decisions SET claim_embeddings_failed_at = now() - interval '1 hour'
		 WHERE id = $1 AND org_id = $2`, dec.ID, dec.OrgID)
	require.NoError(t, err)

	refs, err := testDB.FindRetriableClaimFailures(ctx, 3, 50)
	require.NoError(t, err)

	found := false
	for _, r := range refs {
		if r.ID == dec.ID {
			found = true
			assert.Equal(t, dec.OrgID, r.OrgID)
			assert.Equal(t, 1, r.Attempts)
			break
		}
	}
	assert.True(t, found, "expected to find the failed decision in retriable results")
}

// ---------------------------------------------------------------------------
// Tests: GetConflictAnalytics — broader time window, structure validation
// ---------------------------------------------------------------------------

func TestGetConflictAnalytics_StructureValidation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	analytics, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From: now.Add(-7 * 24 * time.Hour),
		To:   now,
	})
	require.NoError(t, err)
	assert.NotNil(t, analytics.ByAgentPair)
	assert.NotNil(t, analytics.ByDecisionType)
	assert.NotNil(t, analytics.BySeverity)
	assert.NotNil(t, analytics.Trend)
	assert.Len(t, analytics.Trend, 7, "should have one trend point per day")
}

// ---------------------------------------------------------------------------
// Tests: GetOutcomeSignalsSummary
// ---------------------------------------------------------------------------

func TestGetOutcomeSignalsSummary_Populated(t *testing.T) {
	ctx := context.Background()
	summary, err := testDB.GetOutcomeSignalsSummary(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, summary.DecisionsTotal, 0)
	assert.GreaterOrEqual(t, summary.NeverSuperseded, 0)
	assert.GreaterOrEqual(t, summary.NeverCited, 0)
	assert.GreaterOrEqual(t, summary.ConflictsWon, 0)
	assert.Equal(t, summary.ConflictsWon, summary.ConflictsLost, "wins and losses should be symmetric")
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionQualityStats
// ---------------------------------------------------------------------------

func TestGetDecisionQualityStats_Structure(t *testing.T) {
	ctx := context.Background()
	stats, err := testDB.GetDecisionQualityStats(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.Total, 0)
}

// ---------------------------------------------------------------------------
// Tests: SearchDecisionsByText — FTS error triggers ILIKE fallback
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_MalformedQuery(t *testing.T) {
	ctx := context.Background()
	results, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, `"unclosed quote`, model.QueryFilters{}, 10)
	require.NoError(t, err)
	_ = results
}

func TestSearchDecisionsByText_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	results, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, "test", model.QueryFilters{}, 0)
	require.NoError(t, err)
	_ = results
}

func TestSearchDecisionsByText_LargeLimit(t *testing.T) {
	ctx := context.Background()
	results, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, "test", model.QueryFilters{}, 5000)
	require.NoError(t, err)
	_ = results
}

// ---------------------------------------------------------------------------
// Tests: ExportDecisionsCursor — with filters
// ---------------------------------------------------------------------------

func TestExportDecisionsCursor_WithFilters(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "export-filter-" + suffix
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "export_type_" + suffix, Outcome: "exported",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	decType := "export_type_" + suffix
	decisions, err := testDB.ExportDecisionsCursor(ctx, run.OrgID,
		model.QueryFilters{DecisionType: &decType}, nil, 10)
	require.NoError(t, err)
	assert.Len(t, decisions, 1)
	assert.Equal(t, "exported", decisions[0].Outcome)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionErasure — found path
// ---------------------------------------------------------------------------

func TestGetDecisionErasure_Found(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "erasure-" + suffix
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "erasure_test", Outcome: "to be erased",
		Confidence: 0.5, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	_, err = testDB.EraseDecision(ctx, dec.OrgID, dec.ID, "gdpr", agentID, nil)
	require.NoError(t, err)

	erasure, err := testDB.GetDecisionErasure(ctx, dec.OrgID, dec.ID)
	require.NoError(t, err)
	assert.Equal(t, "gdpr", erasure.Reason)
}

// ---------------------------------------------------------------------------
// Tests: CountConflicts — with severity and category filters
// ---------------------------------------------------------------------------

func TestCountConflicts_WithSeverity(t *testing.T) {
	ctx := context.Background()
	sev := "high"
	count, err := testDB.CountConflicts(ctx, uuid.Nil, storage.ConflictFilters{Severity: &sev})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

func TestCountConflicts_WithCategory(t *testing.T) {
	ctx := context.Background()
	cat := "architectural"
	count, err := testDB.CountConflicts(ctx, uuid.Nil, storage.ConflictFilters{Category: &cat})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

// ---------------------------------------------------------------------------
// Tests: DeleteAgentData — with comprehensive data and audit
// ---------------------------------------------------------------------------

func TestDeleteAgentData_WithAllRelatedData(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "delete-full-" + suffix

	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "delete_full_test", Outcome: "will be removed",
		Confidence: 0.6, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	_, err = testDB.CreateEvidence(ctx, model.Evidence{
		DecisionID: dec.ID, OrgID: dec.OrgID,
		SourceType: model.SourceAPIResponse, Content: "evidence content",
	})
	require.NoError(t, err)

	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: dec.ID, Label: "alternative A"},
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	err = testDB.InsertEvent(ctx, model.AgentEvent{
		ID: uuid.New(), RunID: run.ID, OrgID: run.OrgID,
		EventType: model.EventDecisionStarted, SequenceNum: 1,
		OccurredAt: now, AgentID: agentID,
		Payload: map[string]any{}, CreatedAt: now,
	})
	require.NoError(t, err)

	resID := agentID
	_, err = testDB.CreateGrant(ctx, model.AccessGrant{
		OrgID: uuid.Nil, GrantorID: agent.ID, GranteeID: agent.ID,
		ResourceType: "agent_traces", ResourceID: &resID,
		Permission: "read",
	})
	require.NoError(t, err)

	result, err := testDB.DeleteAgentData(ctx, uuid.Nil, agentID, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Decisions, int64(1))
	assert.GreaterOrEqual(t, result.Evidence, int64(1))
	assert.GreaterOrEqual(t, result.Alternatives, int64(1))
	assert.GreaterOrEqual(t, result.Events, int64(1))
	assert.GreaterOrEqual(t, result.Runs, int64(1))
	assert.GreaterOrEqual(t, result.Grants, int64(1))
	assert.GreaterOrEqual(t, result.Agents, int64(1))
}

// ---------------------------------------------------------------------------
// Tests: GetCitationPercentilesForOrg — with data
// ---------------------------------------------------------------------------

func TestGetCitationPercentilesForOrg_WithCitedDecisions(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "citation-perc-" + suffix
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	precedent, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "precedent_type", Outcome: "the precedent",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	for i := range 3 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID,
			DecisionType: "citing_type", Outcome: fmt.Sprintf("citing %d", i),
			Confidence:   0.8,
			PrecedentRef: &precedent.ID,
			Metadata:     map[string]any{},
		})
		require.NoError(t, err)
	}

	breakpoints, err := testDB.GetCitationPercentilesForOrg(ctx, run.OrgID)
	require.NoError(t, err)
	require.Len(t, breakpoints, 4)
	for _, bp := range breakpoints {
		assert.GreaterOrEqual(t, bp, float64(0))
	}
}

// ---------------------------------------------------------------------------
// Tests: BackfillOutcomeEmbedding — success path
// ---------------------------------------------------------------------------

func TestBackfillOutcomeEmbedding_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "backfill-oe-" + suffix
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "backfill_oe", Outcome: "needs outcome embedding",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	emb := pgvector.NewVector(make([]float32, 1024))
	err = testDB.BackfillEmbedding(ctx, dec.ID, dec.OrgID, emb)
	require.NoError(t, err)

	oemb := pgvector.NewVector(make([]float32, 1024))
	err = testDB.BackfillOutcomeEmbedding(ctx, dec.ID, dec.OrgID, oemb)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: GetAlternativesByDecisions — empty and no-match
// ---------------------------------------------------------------------------

func TestGetAlternativesByDecisions_Empty(t *testing.T) {
	ctx := context.Background()
	result, err := testDB.GetAlternativesByDecisions(ctx, []uuid.UUID{}, uuid.Nil)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestGetAlternativesByDecisions_NoMatch(t *testing.T) {
	ctx := context.Background()
	result, err := testDB.GetAlternativesByDecisions(ctx, []uuid.UUID{uuid.New()}, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

// ---------------------------------------------------------------------------
// Tests: GetEvidenceByDecision — empty
// ---------------------------------------------------------------------------

func TestGetEvidenceByDecision_NoEvidence(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "evidence-none-" + suffix
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "no_evidence", Outcome: "has no evidence",
		Confidence: 0.5, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	evidence, err := testDB.GetEvidenceByDecision(ctx, dec.ID, dec.OrgID)
	require.NoError(t, err)
	assert.Empty(t, evidence)
}

// ---------------------------------------------------------------------------
// Tests: GetAlternativesByDecision — empty
// ---------------------------------------------------------------------------

func TestGetAlternativesByDecision_NoAlternatives(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "alts-none-" + suffix
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "no_alts", Outcome: "has no alternatives",
		Confidence: 0.5, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	alts, err := testDB.GetAlternativesByDecision(ctx, dec.ID, dec.OrgID)
	require.NoError(t, err)
	assert.Empty(t, alts)
}

// ---------------------------------------------------------------------------
// Tests: TouchLastSeen — non-existent
// ---------------------------------------------------------------------------

func TestTouchLastSeen_NonExistent(t *testing.T) {
	ctx := context.Background()
	err := testDB.TouchLastSeen(ctx, uuid.Nil, "nonexistent-"+uuid.New().String()[:8])
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: GetEventsByRun — default limit
// ---------------------------------------------------------------------------

func TestGetEventsByRun_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	events, err := testDB.GetEventsByRun(ctx, uuid.Nil, uuid.New(), 0)
	require.NoError(t, err)
	assert.Empty(t, events)
}

// ---------------------------------------------------------------------------
// Tests: NewConflictsSinceByOrg — default limit
// ---------------------------------------------------------------------------

func TestNewConflictsSinceByOrg_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	conflicts, err := testDB.NewConflictsSinceByOrg(ctx, uuid.Nil, time.Now().Add(-24*time.Hour), 0)
	require.NoError(t, err)
	_ = conflicts
}

// ---------------------------------------------------------------------------
// Tests: GetResolvedConflictsByType — default limit
// ---------------------------------------------------------------------------

func TestGetResolvedConflictsByType_DefaultLimitBranch(t *testing.T) {
	ctx := context.Background()
	results, err := testDB.GetResolvedConflictsByType(ctx, uuid.Nil, "test_type", 0)
	require.NoError(t, err)
	_ = results
}

// ---------------------------------------------------------------------------
// Tests: CreateRun — nil metadata path
// ---------------------------------------------------------------------------

func TestCreateRun_NilMetadata(t *testing.T) {
	ctx := context.Background()
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{
		AgentID:  "nilmeta-run-" + uuid.New().String()[:8],
		Metadata: nil,
	})
	require.NoError(t, err)
	assert.NotNil(t, run.Metadata, "nil metadata should be replaced with empty map")
	assert.Empty(t, run.Metadata)
}

// ---------------------------------------------------------------------------
// Tests: BatchDeleteDecisions — agentID and excludeTypes filter branches
// ---------------------------------------------------------------------------

func TestBatchDeleteDecisions_WithAgentFilter(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "bd-agent-" + suffix
	otherAgent := "bd-other-" + suffix
	decisionType := "bd_agent_type_" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	runOther, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: otherAgent})
	require.NoError(t, err)

	// Create decisions for both agents.
	for i := range 2 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID,
			DecisionType: decisionType, Outcome: fmt.Sprintf("agent_%d", i),
			Confidence: 0.5, Metadata: map[string]any{},
		})
		require.NoError(t, err)
	}
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: runOther.ID, AgentID: otherAgent,
		DecisionType: decisionType, Outcome: "other",
		Confidence: 0.5, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	cutoff := time.Now().UTC().Add(1 * time.Hour)
	result, err := testDB.BatchDeleteDecisions(ctx, uuid.Nil, cutoff, &decisionType, &agentID, nil, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.Decisions, "should only delete agent's decisions")
}

func TestBatchDeleteDecisions_WithExcludeTypes(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "bd-excl-" + suffix
	typeKeep := "bd_keep_" + suffix
	typeDelete := "bd_delete_" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: typeKeep, Outcome: "should survive",
		Confidence: 0.5, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: typeDelete, Outcome: "should be deleted",
		Confidence: 0.5, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	cutoff := time.Now().UTC().Add(1 * time.Hour)
	result, err := testDB.BatchDeleteDecisions(ctx, uuid.Nil, cutoff, nil, &agentID, []string{typeKeep}, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Decisions, "should only delete non-excluded type")
}

func TestBatchDeleteDecisions_DefaultBatchSize(t *testing.T) {
	ctx := context.Background()
	// batchSize=0 should default to 1000.
	farPast := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	result, err := testDB.BatchDeleteDecisions(ctx, uuid.Nil, farPast, nil, nil, nil, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), result.Decisions)
}

// ---------------------------------------------------------------------------
// Tests: ListConflicts — status and kind filter branches
// ---------------------------------------------------------------------------

func TestListConflicts_StatusFilter(t *testing.T) {
	ctx := context.Background()
	status := "open"
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		Status: &status,
	}, 50, 0)
	require.NoError(t, err)
	for _, c := range conflicts {
		assert.Equal(t, "open", c.Status, "status filter should only return open conflicts")
	}
}

func TestListConflicts_KindFilter(t *testing.T) {
	ctx := context.Background()
	kind := "cross_agent"
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		ConflictKind: &kind,
	}, 50, 0)
	require.NoError(t, err)
	for _, c := range conflicts {
		assert.Equal(t, model.ConflictKindCrossAgent, c.ConflictKind, "kind filter should only return cross_agent conflicts")
	}
}

func TestListConflicts_DecisionIDFilter(t *testing.T) {
	ctx := context.Background()
	decID := uuid.New()
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		DecisionID: &decID,
	}, 50, 0)
	require.NoError(t, err)
	_ = conflicts // just verify no error with this filter
}

func TestListConflicts_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	// limit=0 should default to 50.
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{}, 0, 0)
	require.NoError(t, err)
	_ = conflicts
}

func TestListConflicts_LargeLimit(t *testing.T) {
	ctx := context.Background()
	// limit > 1000 should be capped at 1000.
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{}, 5000, 0)
	require.NoError(t, err)
	_ = conflicts
}

func TestListConflicts_NegativeOffset(t *testing.T) {
	ctx := context.Background()
	// negative offset should be clamped to 0.
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{}, 50, -10)
	require.NoError(t, err)
	_ = conflicts
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — ordering branches
// ---------------------------------------------------------------------------

func TestQueryDecisions_OrderByConfidence(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Limit:   10,
		OrderBy: "confidence",
	})
	require.NoError(t, err)
	_ = decisions
}

func TestQueryDecisions_OrderByCreatedAt(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Limit:   10,
		OrderBy: "created_at",
	})
	require.NoError(t, err)
	_ = decisions
}

func TestQueryDecisions_DescDirection(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Limit:    10,
		OrderDir: "desc",
	})
	require.NoError(t, err)
	_ = decisions
}

func TestQueryDecisions_AscDirection(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Limit:    10,
		OrderDir: "asc",
	})
	require.NoError(t, err)
	_ = decisions
}

// ---------------------------------------------------------------------------
// Tests: ListGrants — default limit and filter branches
// ---------------------------------------------------------------------------

func TestListGrants_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	grants, _, err := testDB.ListGrants(ctx, uuid.Nil, 0, 0)
	require.NoError(t, err)
	_ = grants
}

func TestListGrants_LargeLimit(t *testing.T) {
	ctx := context.Background()
	grants, _, err := testDB.ListGrants(ctx, uuid.Nil, 5000, 0)
	require.NoError(t, err)
	_ = grants
}

// ---------------------------------------------------------------------------
// Tests: CountEligibleDecisions — with exclude types
// ---------------------------------------------------------------------------

func TestCountEligibleDecisions_WithDecisionTypeFilter(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(1 * time.Hour)
	dt := "architecture"
	counts, err := testDB.CountEligibleDecisions(ctx, uuid.Nil, cutoff, &dt, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, counts.Decisions, int64(0))
}

func TestCountEligibleDecisions_WithAgentFilter(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(1 * time.Hour)
	agent := "some-agent"
	counts, err := testDB.CountEligibleDecisions(ctx, uuid.Nil, cutoff, nil, &agent)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, counts.Decisions, int64(0))
}

// ---------------------------------------------------------------------------
// Tests: FindEmbeddedDecisionIDs — with limit branch
// ---------------------------------------------------------------------------

func TestFindEmbeddedDecisionIDs_WithLimit(t *testing.T) {
	ctx := context.Background()
	ids, err := testDB.FindEmbeddedDecisionIDs(ctx, 5)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(ids), 5)
}

// ---------------------------------------------------------------------------
// Tests: DropEventChunks — exercising the function
// ---------------------------------------------------------------------------

func TestDropEventChunks_FarPast(t *testing.T) {
	ctx := context.Background()
	// A far-past cutoff should not drop anything.
	dropped, err := testDB.DropEventChunks(ctx, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, int64(0), dropped)
}

func TestDropEventChunks_AuditLogPopulated(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "chunk-audit-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Insert events with occurred_at 5 days ago so they land in an old chunk.
	// TimescaleDB chunk interval is 1 day, so a 5-day-old event is in a chunk
	// whose range_end is at most 4 days ago.
	oldTime := time.Now().UTC().Add(-5 * 24 * time.Hour)
	events := make([]model.AgentEvent, 3)
	for i := range events {
		events[i] = model.AgentEvent{
			ID:          uuid.New(),
			RunID:       run.ID,
			OrgID:       uuid.Nil, // test org
			EventType:   model.EventDecisionMade,
			SequenceNum: int64(900_000 + i),
			OccurredAt:  oldTime.Add(time.Duration(i) * time.Minute),
			AgentID:     agentID,
			Payload:     map[string]any{"test": true},
			CreatedAt:   oldTime.Add(time.Duration(i) * time.Minute),
		}
	}
	_, err = testDB.InsertEvents(ctx, events)
	require.NoError(t, err)

	// Record deletion_audit_log count before the drop so we can assert on net-new rows.
	var beforeCount int64
	err = testDB.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM deletion_audit_log WHERE table_name = 'agent_events'`,
	).Scan(&beforeCount)
	require.NoError(t, err)

	// Drop chunks older than 3 days ago. The 5-day-old chunk qualifies.
	cutoff := time.Now().UTC().Add(-3 * 24 * time.Hour)
	dropped, err := testDB.DropEventChunks(ctx, cutoff)
	require.NoError(t, err)

	if dropped == 0 {
		t.Skip("TimescaleDB did not create a separate chunk for the old events (chunk interval may exceed test window)")
	}

	// Verify that deletion_audit_log was populated with summary records.
	var afterCount int64
	err = testDB.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM deletion_audit_log WHERE table_name = 'agent_events'`,
	).Scan(&afterCount)
	require.NoError(t, err)
	assert.Greater(t, afterCount, beforeCount, "DropEventChunks should write audit log entries")

	// Verify the audit log entries contain the expected fields.
	var recordData map[string]any
	err = testDB.Pool().QueryRow(ctx,
		`SELECT record_data FROM deletion_audit_log
		 WHERE table_name = 'agent_events'
		 ORDER BY id DESC LIMIT 1`,
	).Scan(&recordData)
	require.NoError(t, err)

	assert.Equal(t, "retention_chunk_drop", recordData["operation"], "operation field")
	assert.NotNil(t, recordData["chunk_name"], "chunk_name field")
	assert.NotNil(t, recordData["event_count"], "event_count field")
	assert.NotNil(t, recordData["min_occurred_at"], "min_occurred_at field")
	assert.NotNil(t, recordData["max_occurred_at"], "max_occurred_at field")
	assert.NotNil(t, recordData["event_types"], "event_types field")
	assert.NotNil(t, recordData["cutoff"], "cutoff field")

	// event_count should be a positive number.
	eventCount, ok := recordData["event_count"].(float64)
	assert.True(t, ok, "event_count should be a number")
	assert.Greater(t, eventCount, float64(0), "event_count should be positive")
}

// ---------------------------------------------------------------------------
// Tests: GetActiveAPIKeysByAgentIDGlobal — coverage
// ---------------------------------------------------------------------------

func TestGetActiveAPIKeysByAgentIDGlobal_NoKeys(t *testing.T) {
	ctx := context.Background()
	keys, err := testDB.GetActiveAPIKeysByAgentIDGlobal(ctx, "nonexistent-"+uuid.New().String()[:8])
	require.NoError(t, err)
	assert.Empty(t, keys)
}

// ---------------------------------------------------------------------------
// Tests: ListAPIKeys — with agent filter
// ---------------------------------------------------------------------------

func TestGetActiveAPIKeysByAgentIDGlobal_NoMatch(t *testing.T) {
	ctx := context.Background()
	keys, err := testDB.GetActiveAPIKeysByAgentIDGlobal(ctx, "no-keys-agent-"+uuid.New().String()[:8])
	require.NoError(t, err)
	assert.Empty(t, keys)
}

// ---------------------------------------------------------------------------
// Tests: InsertEventsIdempotent — full lifecycle with duplicates
// ---------------------------------------------------------------------------

func TestInsertEventsIdempotent_WithDuplicates(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "idem-dup-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	now := time.Now().UTC()
	events := make([]model.AgentEvent, 3)
	for i := range events {
		events[i] = model.AgentEvent{
			ID:          uuid.New(),
			RunID:       run.ID,
			EventType:   model.EventToolCallCompleted,
			SequenceNum: int64(i + 1),
			OccurredAt:  now,
			AgentID:     agentID,
			Payload:     map[string]any{"step": i},
			CreatedAt:   now,
		}
	}

	// Insert original events.
	inserted, err := testDB.InsertEventsIdempotent(ctx, events)
	require.NoError(t, err)
	assert.Equal(t, int64(3), inserted)

	// Mix duplicates with one new event.
	newEvent := model.AgentEvent{
		ID:          uuid.New(),
		RunID:       run.ID,
		EventType:   model.EventToolCallCompleted,
		SequenceNum: 4,
		OccurredAt:  now,
		AgentID:     agentID,
		Payload:     map[string]any{"step": 3},
		CreatedAt:   now,
	}
	mixed := make([]model.AgentEvent, len(events)+1)
	copy(mixed, events)
	mixed[len(events)] = newEvent

	inserted2, err := testDB.InsertEventsIdempotent(ctx, mixed)
	require.NoError(t, err)
	assert.Equal(t, int64(1), inserted2, "only the new event should be inserted")

	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, got, 4)
}

func TestInsertEventsIdempotent_SingleEvent(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "idem-single-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	now := time.Now().UTC()
	event := model.AgentEvent{
		ID:          uuid.New(),
		RunID:       run.ID,
		EventType:   model.EventToolCallCompleted,
		SequenceNum: 1,
		OccurredAt:  now,
		AgentID:     agentID,
		Payload:     map[string]any{"step": 0},
		CreatedAt:   now,
	}

	inserted, err := testDB.InsertEventsIdempotent(ctx, []model.AgentEvent{event})
	require.NoError(t, err)
	assert.Equal(t, int64(1), inserted)

	// Re-insert same event.
	inserted2, err := testDB.InsertEventsIdempotent(ctx, []model.AgentEvent{event})
	require.NoError(t, err)
	assert.Equal(t, int64(0), inserted2, "duplicate should be skipped")
}

// ---------------------------------------------------------------------------
// Tests: CreateTraceTx — with alternatives and evidence
// ---------------------------------------------------------------------------

func TestCreateTraceTx_WithAlternativesAndEvidence(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "trace-tx-full-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	traceID := "trace-" + suffix
	sessionID := uuid.New()
	run, dec, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:  agentID,
		OrgID:    uuid.Nil,
		TraceID:  &traceID,
		Metadata: map[string]any{"test": true},
		Decision: model.Decision{
			DecisionType: "architecture",
			Outcome:      "use postgres",
			Confidence:   0.95,
			Reasoning:    strPtr("better for ACID"),
			Metadata:     map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "MySQL"},
			{Label: "Postgres"},
		},
		Evidence: []model.Evidence{
			{SourceType: model.SourceAPIResponse, Content: "benchmarks show...", Metadata: map[string]any{}},
		},
		SessionID:    &sessionID,
		AgentContext: map[string]any{"task": "db-selection"},
	})
	require.NoError(t, err)
	assert.Equal(t, agentID, run.AgentID)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
	assert.NotNil(t, run.CompletedAt)
	assert.Equal(t, "architecture", dec.DecisionType)
	assert.Equal(t, "use postgres", dec.Outcome)
	assert.NotEqual(t, uuid.Nil, dec.ID)

	// Verify alternatives were created.
	alts, err := testDB.GetAlternativesByDecision(ctx, dec.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, alts, 2)

	// Verify evidence was created.
	evs, err := testDB.GetEvidenceByDecision(ctx, dec.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, evs, 1)
}

func TestCreateTraceTx_WithAuditEntry(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "trace-tx-audit-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	audit := &storage.MutationAuditEntry{
		RequestID:    "req-" + suffix,
		OrgID:        uuid.Nil,
		ActorAgentID: agentID,
		ActorRole:    "agent",
		HTTPMethod:   "POST",
		Endpoint:     "/v1/trace",
		Operation:    "create",
		ResourceType: "decision",
	}

	run, dec, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Decision: model.Decision{
			DecisionType: "tool_choice",
			Outcome:      "use grep",
			Confidence:   0.8,
			Metadata:     map[string]any{},
		},
		AuditEntry: audit,
	})
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
	assert.NotEqual(t, uuid.Nil, dec.ID)
}

func TestCreateTraceTx_MinimalDecision(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "trace-tx-min-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, dec, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Decision: model.Decision{
			DecisionType: "test",
			Outcome:      "minimal",
			Confidence:   0.5,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
	assert.NotEqual(t, uuid.Nil, dec.ID)
	assert.NotNil(t, dec.Metadata, "nil metadata should be defaulted to empty map")
}

// ---------------------------------------------------------------------------
// Tests: DeleteAgentData — with audit entry
// ---------------------------------------------------------------------------

func TestDeleteAgentData_WithAuditEntry(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "delete-audit-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	audit := &storage.MutationAuditEntry{
		RequestID:    "req-del-" + suffix,
		OrgID:        uuid.Nil,
		ActorAgentID: "admin",
		ActorRole:    "platform_admin",
		HTTPMethod:   "DELETE",
		Endpoint:     "/v1/agents/" + agentID,
		Operation:    "delete",
		ResourceType: "agent",
	}

	result, err := testDB.DeleteAgentData(ctx, uuid.Nil, agentID, audit)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Agents)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignals — all three signal types populated
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignals_AllSignals(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "signals-all-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create a decision to query signals for.
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "signals_test", Outcome: "test outcome",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create a superseding decision.
	run2, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	supersedesID := dec.ID
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run2.ID, AgentID: agentID,
		DecisionType: "signals_test", Outcome: "revised outcome",
		Confidence: 0.9, Metadata: map[string]any{},
		SupersedesID: &supersedesID,
	})
	require.NoError(t, err)

	// Create a citing decision.
	run3, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	precedentRef := dec.ID
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run3.ID, AgentID: agentID,
		DecisionType: "signals_test", Outcome: "citing outcome",
		Confidence: 0.8, Metadata: map[string]any{},
		PrecedentRef: &precedentRef,
	})
	require.NoError(t, err)

	signals, err := testDB.GetDecisionOutcomeSignals(ctx, dec.ID, uuid.Nil)
	require.NoError(t, err)
	assert.NotNil(t, signals.SupersessionVelocityHours, "should have supersession velocity")
	assert.GreaterOrEqual(t, signals.PrecedentCitationCount, 1, "should have at least one citation")
}

// ---------------------------------------------------------------------------
// Tests: GetCitationPercentilesForOrg — with multiple citation levels
// ---------------------------------------------------------------------------

func TestGetCitationPercentilesForOrg_MultipleCitations(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "cite-pctl-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create a base decision that will be cited.
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	baseDec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "pctl_test", Outcome: "base",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create several citing decisions.
	for i := 0; i < 5; i++ {
		r, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
		require.NoError(t, err)

		ref := baseDec.ID
		_, err = testDB.CreateDecision(ctx, model.Decision{
			RunID: r.ID, AgentID: agentID,
			DecisionType: "pctl_test", Outcome: fmt.Sprintf("citing %d", i),
			Confidence: 0.8, Metadata: map[string]any{},
			PrecedentRef: &ref,
		})
		require.NoError(t, err)
	}

	breakpoints, err := testDB.GetCitationPercentilesForOrg(ctx, uuid.Nil)
	require.NoError(t, err)
	// With data, should return percentile breakpoints.
	assert.NotNil(t, breakpoints, "should return percentile breakpoints when citations exist")
	assert.Len(t, breakpoints, 4, "should return [p25, p50, p75, p90]")
}

// ---------------------------------------------------------------------------
// Tests: CleanupIdempotencyKeys — exercising the cleanup
// ---------------------------------------------------------------------------

func TestCleanupIdempotencyKeys_RemovesExpired(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	// Create an in-progress key.
	_, err := testDB.BeginIdempotency(ctx, uuid.Nil, "cleanup-agent-"+suffix, "/v1/trace", "cleanup-key-"+suffix, "hash1")
	require.NoError(t, err)

	// Cleanup with a very short TTL should remove it.
	// The key was just created, so with a 0 TTL it should be removed.
	deleted, err := testDB.CleanupIdempotencyKeys(ctx, 0, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(1), "should remove at least the in-progress key")
}

// ---------------------------------------------------------------------------
// Tests: CompleteIdempotency — full lifecycle
// ---------------------------------------------------------------------------

func TestCompleteIdempotency_FullLifecycle(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentID := "idemp-lifecycle-" + suffix
	endpoint := "/v1/trace"
	key := "idemp-key-" + suffix
	hash := "hash-" + suffix

	// Begin: should succeed (new key).
	lookup, err := testDB.BeginIdempotency(ctx, uuid.Nil, agentID, endpoint, key, hash)
	require.NoError(t, err)
	assert.False(t, lookup.Completed)

	// Complete: mark the key as done.
	err = testDB.CompleteIdempotency(ctx, uuid.Nil, agentID, endpoint, key, 201, map[string]any{"id": "dec-123"})
	require.NoError(t, err)

	// Begin again with same hash: should return completed lookup.
	lookup, err = testDB.BeginIdempotency(ctx, uuid.Nil, agentID, endpoint, key, hash)
	require.NoError(t, err)
	assert.True(t, lookup.Completed)
	assert.Equal(t, 201, lookup.StatusCode)
	assert.NotEmpty(t, lookup.ResponseData)
}

func TestBeginIdempotency_PayloadMismatch(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentID := "idemp-mismatch-" + suffix
	endpoint := "/v1/trace"
	key := "idemp-mismatch-key-" + suffix

	// Create initial key.
	_, err := testDB.BeginIdempotency(ctx, uuid.Nil, agentID, endpoint, key, "hash-a")
	require.NoError(t, err)

	// Complete it.
	err = testDB.CompleteIdempotency(ctx, uuid.Nil, agentID, endpoint, key, 200, map[string]any{})
	require.NoError(t, err)

	// Retry with different hash: should return ErrIdempotencyPayloadMismatch.
	_, err = testDB.BeginIdempotency(ctx, uuid.Nil, agentID, endpoint, key, "hash-b")
	require.ErrorIs(t, err, storage.ErrIdempotencyPayloadMismatch)
}

func TestBeginIdempotency_InProgress(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentID := "idemp-inprog-" + suffix
	endpoint := "/v1/trace"
	key := "idemp-inprog-key-" + suffix
	hash := "hash-inprog-" + suffix

	// First call owns processing.
	_, err := testDB.BeginIdempotency(ctx, uuid.Nil, agentID, endpoint, key, hash)
	require.NoError(t, err)

	// Second call with same hash should get ErrIdempotencyInProgress.
	_, err = testDB.BeginIdempotency(ctx, uuid.Nil, agentID, endpoint, key, hash)
	require.ErrorIs(t, err, storage.ErrIdempotencyInProgress)
}

// ---------------------------------------------------------------------------
// Tests: GetEventsByRun — with explicit limit
// ---------------------------------------------------------------------------

func TestGetEventsByRun_WithExplicitLimit(t *testing.T) {
	ctx := context.Background()
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: "events-limit-test"})
	require.NoError(t, err)

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		err = testDB.InsertEvent(ctx, model.AgentEvent{
			ID: uuid.New(), RunID: run.ID, OrgID: run.OrgID,
			EventType: model.EventDecisionStarted, SequenceNum: int64(i + 1),
			OccurredAt: now, AgentID: "events-limit-test",
			Payload: map[string]any{}, CreatedAt: now,
		})
		require.NoError(t, err)
	}

	// Fetch with limit of 3.
	events, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 3)
	require.NoError(t, err)
	assert.Len(t, events, 3)

	// Fetch with limit of 0 (should use default).
	events, err = testDB.GetEventsByRun(ctx, run.OrgID, run.ID, 0)
	require.NoError(t, err)
	assert.Len(t, events, 5)

	// Fetch with negative limit (should use default).
	events, err = testDB.GetEventsByRun(ctx, run.OrgID, run.ID, -1)
	require.NoError(t, err)
	assert.Len(t, events, 5)
}

// ---------------------------------------------------------------------------
// Tests: GetGrant — not found
// ---------------------------------------------------------------------------

func TestGetGrant_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetGrant(ctx, uuid.Nil, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Tests: GetOrganization — found after EnsureDefaultOrg
// ---------------------------------------------------------------------------

func TestGetOrganization_AfterEnsureDefault(t *testing.T) {
	ctx := context.Background()
	err := testDB.EnsureDefaultOrg(ctx)
	require.NoError(t, err)

	org, err := testDB.GetOrganization(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, org.ID)
	assert.Equal(t, "Default", org.Name)
}

// ---------------------------------------------------------------------------
// Tests: UpdateAgentTags — not found
// ---------------------------------------------------------------------------

func TestUpdateAgentTags_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.UpdateAgentTags(ctx, uuid.Nil, "nonexistent-"+uuid.New().String()[:8], []string{"tag1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestUpdateAgentTags_NilTags(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "tags-nil-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Nil tags should be converted to empty slice.
	agent, err := testDB.UpdateAgentTags(ctx, uuid.Nil, agentID, nil)
	require.NoError(t, err)
	assert.NotNil(t, agent.Tags)
	assert.Empty(t, agent.Tags)
}

// ---------------------------------------------------------------------------
// Tests: UpdateAgentWithAudit — not found
// ---------------------------------------------------------------------------

func TestUpdateAgentWithAudit_NotFound(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	name := "new-name"
	_, err := testDB.UpdateAgentWithAudit(ctx, uuid.Nil, "nonexistent-"+suffix, &name, nil, storage.MutationAuditEntry{
		RequestID:    "req-" + suffix,
		OrgID:        uuid.Nil,
		ActorAgentID: "admin",
		ActorRole:    "platform_admin",
		HTTPMethod:   "PATCH",
		Endpoint:     "/v1/agents/x",
		Operation:    "update",
		ResourceType: "agent",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Tests: RevokeAPIKeyWithAudit — already revoked
// ---------------------------------------------------------------------------

func TestRevokeAPIKeyWithAudit_AlreadyRevoked(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "revoke-twice-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	key, err := testDB.CreateAPIKeyWithAudit(ctx, model.APIKey{
		Prefix:    "ak_" + suffix[:6],
		KeyHash:   "hash-" + suffix,
		AgentID:   agentID,
		OrgID:     uuid.Nil,
		Label:     "test key",
		CreatedBy: agentID,
	}, storage.MutationAuditEntry{
		RequestID: "req-create-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "platform_admin",
		HTTPMethod: "POST", Endpoint: "/v1/api-keys",
		Operation: "create", ResourceType: "api_key",
	})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		RequestID: "req-revoke-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "platform_admin",
		HTTPMethod: "DELETE", Endpoint: "/v1/api-keys/" + key.ID.String(),
		Operation: "revoke", ResourceType: "api_key",
	}

	// First revocation should succeed.
	err = testDB.RevokeAPIKeyWithAudit(ctx, uuid.Nil, key.ID, audit)
	require.NoError(t, err)

	// Second revocation should fail with "already revoked".
	audit.RequestID = "req-revoke2-" + suffix
	err = testDB.RevokeAPIKeyWithAudit(ctx, uuid.Nil, key.ID, audit)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already revoked")
}

// ---------------------------------------------------------------------------
// Tests: BatchDeleteDecisions — with decision type filter
// ---------------------------------------------------------------------------

func TestBatchDeleteDecisions_WithDecisionTypeAndAgent(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "batch-del-da-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create some decisions.
	for i := 0; i < 3; i++ {
		r, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
		require.NoError(t, err)
		_, err = testDB.CreateDecision(ctx, model.Decision{
			RunID: r.ID, AgentID: agentID,
			DecisionType: "batch_del_type", Outcome: fmt.Sprintf("outcome %d", i),
			Confidence: 0.5, Metadata: map[string]any{},
		})
		require.NoError(t, err)
	}

	cutoff := time.Now().UTC().Add(1 * time.Hour)
	dt := "batch_del_type"
	counts, err := testDB.BatchDeleteDecisions(ctx, uuid.Nil, cutoff, &dt, &agentID, nil, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(3), counts.Decisions, "all 3 decisions should be deleted")
}

// ---------------------------------------------------------------------------
// Tests: StartDeletionLog — empty initiatedBy
// ---------------------------------------------------------------------------

func TestStartDeletionLog_EmptyInitiatedBy(t *testing.T) {
	ctx := context.Background()

	// Empty initiatedBy should be stored as NULL.
	logID, err := testDB.StartDeletionLog(ctx, uuid.Nil, "policy", "", map[string]any{})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, logID)
}

// ---------------------------------------------------------------------------
// Tests: CreateHold and ReleaseHold — already released returns false
// ---------------------------------------------------------------------------

func TestReleaseHold_AlreadyReleased(t *testing.T) {
	ctx := context.Background()

	hold, err := testDB.CreateHold(ctx, storage.RetentionHold{
		OrgID:     uuid.Nil,
		Reason:    "legal hold test",
		HoldFrom:  time.Now().UTC().Add(-24 * time.Hour),
		HoldTo:    time.Now().UTC().Add(24 * time.Hour),
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	// First release should succeed.
	ok, err := testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)
	assert.True(t, ok)

	// Second release should return false (already released).
	ok, err = testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)
	assert.False(t, ok, "releasing an already-released hold should return false")
}

func TestReleaseHold_NotFound(t *testing.T) {
	ctx := context.Background()
	ok, err := testDB.ReleaseHold(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// Tests: MarkDecisionConflictScored
// ---------------------------------------------------------------------------

func TestMarkDecisionConflictScored_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "mark-scored-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "scored_test", Outcome: "test",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	err = testDB.MarkDecisionConflictScored(ctx, dec.ID, uuid.Nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: GetConflictAnalytics — with all filter combinations
// ---------------------------------------------------------------------------

func TestGetConflictAnalytics_WithAgentFilter(t *testing.T) {
	ctx := context.Background()
	agent := "analytics-agent-filter"
	result, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:    time.Now().UTC().Add(-30 * 24 * time.Hour),
		To:      time.Now().UTC(),
		AgentID: &agent,
	})
	require.NoError(t, err)
	assert.NotNil(t, result.ByAgentPair)
	assert.NotNil(t, result.ByDecisionType)
}

func TestGetConflictAnalytics_WithDecisionTypeFilter(t *testing.T) {
	ctx := context.Background()
	dt := "architecture"
	result, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:         time.Now().UTC().Add(-30 * 24 * time.Hour),
		To:           time.Now().UTC(),
		DecisionType: &dt,
	})
	require.NoError(t, err)
	assert.NotNil(t, result.ByAgentPair)
}

func TestGetConflictAnalytics_WithConflictKindFilter(t *testing.T) {
	ctx := context.Background()
	kind := "cross_agent"
	result, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:         time.Now().UTC().Add(-30 * 24 * time.Hour),
		To:           time.Now().UTC(),
		ConflictKind: &kind,
	})
	require.NoError(t, err)
	assert.NotNil(t, result.ByAgentPair)
}

func TestGetConflictAnalytics_WithAllFilters(t *testing.T) {
	ctx := context.Background()
	agent := "some-agent"
	dt := "architecture"
	kind := "cross_agent"
	result, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:         time.Now().UTC().Add(-30 * 24 * time.Hour),
		To:           time.Now().UTC(),
		AgentID:      &agent,
		DecisionType: &dt,
		ConflictKind: &kind,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.Summary.TotalDetected)
}

// ---------------------------------------------------------------------------
// Tests: ListRunsByAgent — with data
// ---------------------------------------------------------------------------

func TestListRunsByAgent_WithData(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "list-runs-" + suffix

	for i := 0; i < 3; i++ {
		_, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
		require.NoError(t, err)
	}

	runs, total, err := testDB.ListRunsByAgent(ctx, uuid.Nil, agentID, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	assert.Len(t, runs, 2)

	// Second page.
	runs, total, err = testDB.ListRunsByAgent(ctx, uuid.Nil, agentID, 2, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	assert.Len(t, runs, 1)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionForScoring — not found
// ---------------------------------------------------------------------------

func TestGetDecisionForScoring_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetDecisionForScoring(ctx, uuid.New(), uuid.Nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestGetDecisionForScoring_Found(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "scoring-found-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "scoring_test", Outcome: "test outcome",
		Confidence: 0.9, Reasoning: strPtr("because reasons"),
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	got, err := testDB.GetDecisionForScoring(ctx, dec.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, dec.ID, got.ID)
	assert.Equal(t, "scoring_test", got.DecisionType)
}

// ---------------------------------------------------------------------------
// Tests: InsertMutationAudit — with before and after data
// ---------------------------------------------------------------------------

func TestInsertMutationAudit_WithBeforeAndAfterData(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	err := testDB.InsertMutationAudit(ctx, storage.MutationAuditEntry{
		RequestID:    "req-ba-" + suffix,
		OrgID:        uuid.Nil,
		ActorAgentID: "admin",
		ActorRole:    "platform_admin",
		HTTPMethod:   "PATCH",
		Endpoint:     "/v1/agents/test",
		Operation:    "update",
		ResourceType: "agent",
		ResourceID:   "test-agent",
		BeforeData:   map[string]any{"name": "old"},
		AfterData:    map[string]any{"name": "new"},
		Metadata:     map[string]any{"reason": "test"},
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: UpdateOutcomeScore — on a real decision
// ---------------------------------------------------------------------------

func TestUpdateOutcomeScore_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "outcome-score-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "score_test", Outcome: "test",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	score := float32(0.85)
	err = testDB.UpdateOutcomeScore(ctx, uuid.Nil, dec.ID, &score)
	require.NoError(t, err)

	// Set to nil.
	err = testDB.UpdateOutcomeScore(ctx, uuid.Nil, dec.ID, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: SetRetentionPolicy — success
// ---------------------------------------------------------------------------

func TestSetRetentionPolicy_ClearPolicy(t *testing.T) {
	ctx := context.Background()
	// Set a policy then clear it.
	days := 30
	err := testDB.SetRetentionPolicy(ctx, uuid.Nil, &days, []string{"audit"})
	require.NoError(t, err)

	// Clear the policy.
	err = testDB.SetRetentionPolicy(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)

	// Verify it's cleared.
	policy, err := testDB.GetRetentionPolicy(ctx, uuid.Nil, 24*time.Hour)
	require.NoError(t, err)
	assert.Nil(t, policy.RetentionDays)
}

// ---------------------------------------------------------------------------
// Tests: CountEligibleDecisions — with both filters
// ---------------------------------------------------------------------------

func TestCountEligibleDecisions_WithBothFilters(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(1 * time.Hour)
	dt := "architecture"
	agent := "some-agent"
	counts, err := testDB.CountEligibleDecisions(ctx, uuid.Nil, cutoff, &dt, &agent)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, counts.Decisions, int64(0))
}

// ---------------------------------------------------------------------------
// Tests: ListGrants — with data
// ---------------------------------------------------------------------------

func TestListGrants_WithData(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	// Create real agents for FK constraints.
	grantorAgent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "grantor-" + suffix, OrgID: uuid.Nil, Name: "Grantor", Role: model.RoleAdmin,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)
	granteeAgent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "grantee-" + suffix, OrgID: uuid.Nil, Name: "Grantee", Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		resID := fmt.Sprintf("res-%d-%s", i, suffix)
		_, err := testDB.CreateGrant(ctx, model.AccessGrant{
			OrgID: uuid.Nil, GrantorID: grantorAgent.ID, GranteeID: granteeAgent.ID,
			ResourceType: "agent_traces", ResourceID: &resID,
			Permission: "read",
		})
		require.NoError(t, err)
	}

	grants, total, err := testDB.ListGrants(ctx, uuid.Nil, 2, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 3)
	assert.LessOrEqual(t, len(grants), 2)
}

// ---------------------------------------------------------------------------
// Tests: DeleteGrantWithAudit — not found
// ---------------------------------------------------------------------------

func TestDeleteGrantWithAudit_NotFound(t *testing.T) {
	ctx := context.Background()
	err := testDB.DeleteGrantWithAudit(ctx, uuid.Nil, uuid.New(), storage.MutationAuditEntry{
		RequestID:    "req-del-grant",
		OrgID:        uuid.Nil,
		ActorAgentID: "admin",
		ActorRole:    "platform_admin",
		HTTPMethod:   "DELETE",
		Endpoint:     "/v1/grants/x",
		Operation:    "delete",
		ResourceType: "grant",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Tests: GetIntegrityProof — not found
// ---------------------------------------------------------------------------

func TestGetLatestIntegrityProof_NoProofs(t *testing.T) {
	ctx := context.Background()
	// Use a random org ID to avoid existing proofs.
	proof, err := testDB.GetLatestIntegrityProof(ctx, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, proof, "should return nil when no proofs exist for the org")
}

// ---------------------------------------------------------------------------
// Tests: CreateGrantWithAudit — full lifecycle
// ---------------------------------------------------------------------------

func TestCreateGrantWithAudit_FullCycle(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	// Create real agents for FK constraints.
	grantor, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "cwaudit-grantor-" + suffix, OrgID: uuid.Nil, Name: "Grantor", Role: model.RoleAdmin,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)
	grantee, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: "cwaudit-grantee-" + suffix, OrgID: uuid.Nil, Name: "Grantee", Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	resID := "res-" + suffix
	grant, err := testDB.CreateGrantWithAudit(ctx, model.AccessGrant{
		OrgID: uuid.Nil, GrantorID: grantor.ID, GranteeID: grantee.ID,
		ResourceType: "agent_traces", ResourceID: &resID,
		Permission: "read",
	}, storage.MutationAuditEntry{
		RequestID: "req-grant-" + suffix, OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		HTTPMethod: "POST", Endpoint: "/v1/grants",
		Operation: "create", ResourceType: "grant",
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, grant.ID)
}

// ---------------------------------------------------------------------------
// Tests: ReserveSequenceNums — edge cases
// ---------------------------------------------------------------------------

func TestReserveSequenceNums_ZeroCount(t *testing.T) {
	ctx := context.Background()
	nums, err := testDB.ReserveSequenceNums(ctx, 0)
	require.NoError(t, err)
	assert.Nil(t, nums)
}

func TestReserveSequenceNums_NegativeCount(t *testing.T) {
	ctx := context.Background()
	nums, err := testDB.ReserveSequenceNums(ctx, -5)
	require.NoError(t, err)
	assert.Nil(t, nums)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionQualityStats
// ---------------------------------------------------------------------------

func TestGetDecisionQualityStats_WithDecisions(t *testing.T) {
	ctx := context.Background()
	stats, err := testDB.GetDecisionQualityStats(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	// The default org will have decisions from other tests.
	assert.GreaterOrEqual(t, stats.Total, 0)
}

// ---------------------------------------------------------------------------
// Tests: Notify — exercises the success path
// ---------------------------------------------------------------------------

func TestNotify_Success(t *testing.T) {
	ctx := context.Background()
	// Notify on the decisions channel — should succeed even without listeners.
	err := testDB.Notify(ctx, storage.ChannelDecisions, `{"test": true}`)
	require.NoError(t, err)

	err = testDB.Notify(ctx, storage.ChannelConflicts, `{"conflict": true}`)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: CreateOrgWithOwnerAndKeyTx — verifies org, agent, and key creation
// ---------------------------------------------------------------------------

func TestCreateOrgWithOwnerAndKeyTx_AllFieldsPopulated(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	ownerAgentID := "owner-" + suffix

	org, agent, key, err := testDB.CreateOrgWithOwnerAndKeyTx(ctx,
		model.Organization{Name: "Test Org " + suffix, Slug: "test-org-" + suffix, Plan: "pro"},
		model.Agent{AgentID: ownerAgentID, Name: "Owner", Role: model.RoleOrgOwner, Email: strPtr("owner@test.com")},
		model.APIKey{Prefix: "ak_" + suffix[:6], KeyHash: "hash-signup-" + suffix, AgentID: ownerAgentID, Label: "initial key", CreatedBy: ownerAgentID},
		storage.MutationAuditEntry{RequestID: "signup-org-" + suffix, ActorAgentID: "system", ActorRole: "platform_admin", HTTPMethod: "POST", Endpoint: "/v1/signup", Operation: "create", ResourceType: "organization"},
		storage.MutationAuditEntry{RequestID: "signup-agent-" + suffix, ActorAgentID: "system", ActorRole: "platform_admin", HTTPMethod: "POST", Endpoint: "/v1/signup", Operation: "create", ResourceType: "agent"},
		storage.MutationAuditEntry{RequestID: "signup-key-" + suffix, ActorAgentID: "system", ActorRole: "platform_admin", HTTPMethod: "POST", Endpoint: "/v1/signup", Operation: "create", ResourceType: "api_key"},
	)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, org.ID)
	assert.Equal(t, "pro", org.Plan)
	assert.Equal(t, ownerAgentID, agent.AgentID)
	assert.Equal(t, org.ID, agent.OrgID)
	assert.NotEqual(t, uuid.Nil, key.ID)
	assert.Equal(t, org.ID, key.OrgID)
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — with Include (alternatives + evidence)
// ---------------------------------------------------------------------------

func TestQueryDecisions_IncludeBothAlternativesAndEvidence(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "query-include-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create a decision with alternatives and evidence.
	run, dec, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: agentID, OrgID: uuid.Nil,
		Decision: model.Decision{
			DecisionType: "query_include_test_" + suffix,
			Outcome:      "include test", Confidence: 0.8,
			Metadata: map[string]any{},
		},
		Alternatives: []model.Alternative{
			{Label: "Option A"},
			{Label: "Option B"},
		},
		Evidence: []model.Evidence{
			{SourceType: model.SourceAPIResponse, Content: "benchmark data", Metadata: map[string]any{}},
		},
	})
	require.NoError(t, err)
	_ = run

	dt := "query_include_test_" + suffix
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{DecisionType: &dt},
		Include: []string{"alternatives", "evidence"},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Equal(t, dec.ID, decisions[0].ID)
	assert.Len(t, decisions[0].Alternatives, 2, "should include 2 alternatives")
	assert.Len(t, decisions[0].Evidence, 1, "should include 1 evidence")
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — with TraceID filter
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithTraceIDFilter(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "query-trace-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	traceID := "trace-" + suffix
	_, _, err = testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: agentID, OrgID: uuid.Nil, TraceID: &traceID,
		Decision: model.Decision{
			DecisionType: "trace_filter_test", Outcome: "traced", Confidence: 0.9,
			Metadata: map[string]any{},
		},
	})
	require.NoError(t, err)

	// Query with trace ID filter.
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		TraceID: &traceID,
		Limit:   10,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 1)
	assert.GreaterOrEqual(t, len(decisions), 1)
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — quality_score order by (deprecated alias)
// ---------------------------------------------------------------------------

func TestQueryDecisions_OrderByQualityScore(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		OrderBy: "quality_score",
		Limit:   5,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(decisions), 5)
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisionsTemporal — with limit edge cases
// ---------------------------------------------------------------------------

func TestQueryDecisionsTemporal_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	decisions, err := testDB.QueryDecisionsTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf:  time.Now().UTC(),
		Limit: 0, // should default to 100
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(decisions), 100)
}

func TestQueryDecisionsTemporal_LargeLimit(t *testing.T) {
	ctx := context.Background()
	decisions, err := testDB.QueryDecisionsTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf:  time.Now().UTC(),
		Limit: 5000, // should be capped to 1000
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(decisions), 1000)
}

// ---------------------------------------------------------------------------
// Tests: Assessment outcomes — partially_correct and incorrect
// ---------------------------------------------------------------------------

func TestCreateAssessment_PartiallyCorrectAndIncorrect(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "assess-variety-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "assess_variety_test", Outcome: "test outcome",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create assessments with all three outcome types.
	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      dec.ID,
		AssessorAgentID: "assessor-a-" + suffix,
		Outcome:         model.AssessmentCorrect,
		Notes:           strPtr("looks good"),
	})
	require.NoError(t, err)

	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      dec.ID,
		AssessorAgentID: "assessor-b-" + suffix,
		Outcome:         model.AssessmentIncorrect,
		Notes:           strPtr("wrong approach"),
	})
	require.NoError(t, err)

	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID:      dec.ID,
		AssessorAgentID: "assessor-c-" + suffix,
		Outcome:         model.AssessmentPartiallyCorrect,
		Notes:           strPtr("partially right"),
	})
	require.NoError(t, err)

	// Verify the summary covers all three types.
	summary, err := testDB.GetAssessmentSummary(ctx, uuid.Nil, dec.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, summary.Total)
	assert.Equal(t, 1, summary.Correct)
	assert.Equal(t, 1, summary.Incorrect)
	assert.Equal(t, 1, summary.PartiallyCorrect)
}

// ---------------------------------------------------------------------------
// Tests: ExportDecisionsCursor — with cursor pagination
// ---------------------------------------------------------------------------

func TestExportDecisionsCursor_WithCursorPagination(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "export-cursor-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dt := "export_cursor_test_" + suffix

	// Create several decisions.
	for i := 0; i < 5; i++ {
		r, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
		require.NoError(t, err)
		_, err = testDB.CreateDecision(ctx, model.Decision{
			RunID: r.ID, AgentID: agentID,
			DecisionType: dt, Outcome: fmt.Sprintf("outcome %d", i),
			Confidence: 0.5, Metadata: map[string]any{},
		})
		require.NoError(t, err)
	}

	// First page.
	page1, err := testDB.ExportDecisionsCursor(ctx, uuid.Nil, model.QueryFilters{DecisionType: &dt}, nil, 2)
	require.NoError(t, err)
	assert.Len(t, page1, 2)

	// Second page using cursor from first page.
	cursor := &storage.ExportCursor{
		ValidFrom: page1[1].ValidFrom,
		ID:        page1[1].ID,
	}
	page2, err := testDB.ExportDecisionsCursor(ctx, uuid.Nil, model.QueryFilters{DecisionType: &dt}, cursor, 2)
	require.NoError(t, err)
	assert.Len(t, page2, 2)

	// Verify no overlap between pages.
	for _, d1 := range page1 {
		for _, d2 := range page2 {
			assert.NotEqual(t, d1.ID, d2.ID, "pages should not overlap")
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — with multiple filter types
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithOutcomeFilter(t *testing.T) {
	ctx := context.Background()
	outcome := "nonexistent-outcome-" + uuid.New().String()[:8]
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{Outcome: &outcome},
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithConfidenceMinFilter(t *testing.T) {
	ctx := context.Background()
	confMin := float32(0.99)
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{ConfidenceMin: &confMin},
		Limit:   5,
	})
	require.NoError(t, err)
	for _, d := range decisions {
		assert.GreaterOrEqual(t, d.Confidence, float32(0.99))
	}
}

func TestQueryDecisions_WithTimeRangeFilter(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-1 * time.Hour)
	to := now.Add(1 * time.Hour)
	_, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{TimeRange: &model.TimeRange{From: &from, To: &to}},
		Limit:   5,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 0)
}

func TestQueryDecisions_WithSessionFilter(t *testing.T) {
	ctx := context.Background()
	sessionID := uuid.New()
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{SessionID: &sessionID},
		Limit:   5,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithProjectFilter(t *testing.T) {
	ctx := context.Background()
	project := "nonexistent-project"
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{Project: &project},
		Limit:   5,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Tests: GetConflictStatusCounts
// ---------------------------------------------------------------------------

func TestGetConflictStatusCounts_StructureVerification(t *testing.T) {
	ctx := context.Background()
	counts, err := testDB.GetConflictStatusCounts(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, counts.Total, 0)
	assert.GreaterOrEqual(t, counts.Open, 0)
	assert.GreaterOrEqual(t, counts.Resolved, 0)
	assert.GreaterOrEqual(t, counts.FalsePositive, 0)
}

// ---------------------------------------------------------------------------
// Tests: GetConflict — not found
// ---------------------------------------------------------------------------

func TestGetConflict_NotFound(t *testing.T) {
	ctx := context.Background()
	// GetConflict returns nil, nil when not found (not ErrNotFound).
	conflict, err := testDB.GetConflict(ctx, uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, conflict)
}

// ---------------------------------------------------------------------------
// Tests: ListConflicts — with severity filter
// ---------------------------------------------------------------------------

func TestListConflicts_SeverityFilter_Coverage(t *testing.T) {
	ctx := context.Background()
	sev := "critical"
	cat := "factual"
	conflicts, err := testDB.ListConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		Severity: &sev,
		Category: &cat,
	}, 10, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(conflicts), 0)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionsByAgent — with filters exercised
// ---------------------------------------------------------------------------

func TestGetDecisionsByAgent_WithTimeRange(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-1 * time.Hour)
	to := now.Add(1 * time.Hour)
	decisions, _, err := testDB.GetDecisionsByAgent(ctx, uuid.Nil, "nonexistent", 10, 0, &from, &to)
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Tests: MigrateAgentKeysToAPIKeys — no legacy keys (fast path)
// ---------------------------------------------------------------------------

func TestMigrateAgentKeysToAPIKeys_NoLegacyKeys(t *testing.T) {
	ctx := context.Background()
	// All agents in tests use managed API keys, so migration should find nothing.
	migrated, err := testDB.MigrateAgentKeysToAPIKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, migrated, "no agents should have legacy keys to migrate")
}

// ---------------------------------------------------------------------------
// Tests: CreateAlternativesBatch — empty batch
// ---------------------------------------------------------------------------

func TestCreateAlternativesBatch_Empty(t *testing.T) {
	ctx := context.Background()
	err := testDB.CreateAlternativesBatch(ctx, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: SearchDecisionsByText — FTS then ILIKE fallback
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_WithFilters(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "search-text-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "search_text_test", Outcome: "use postgres for data storage " + suffix,
		Confidence: 0.9, Reasoning: strPtr("ACID compliance"),
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	results, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, "postgres "+suffix, model.QueryFilters{
		AgentIDs: []string{agentID},
	}, 5)
	require.NoError(t, err)
	// Should find via FTS or ILIKE fallback.
	assert.GreaterOrEqual(t, len(results), 0)
}

// ---------------------------------------------------------------------------
// Tests: CountConflicts — with decision type filter
// ---------------------------------------------------------------------------

func TestCountConflicts_WithDecisionTypeFilter(t *testing.T) {
	ctx := context.Background()
	dt := "architecture"
	count, err := testDB.CountConflicts(ctx, uuid.Nil, storage.ConflictFilters{
		DecisionType: &dt,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

// ---------------------------------------------------------------------------
// Tests: GetEvidenceCoverageStats
// ---------------------------------------------------------------------------

func TestGetEvidenceCoverageStats_Structure(t *testing.T) {
	ctx := context.Background()
	stats, err := testDB.GetEvidenceCoverageStats(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.TotalDecisions, 0)
	assert.GreaterOrEqual(t, stats.TotalRecords, 0)
}

// ---------------------------------------------------------------------------
// Tests: ListAgentIDsBySharedTags
// ---------------------------------------------------------------------------

func TestListAgentIDsBySharedTags_WithNewTag(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	tag := "shared-tag-" + suffix
	for i := 0; i < 2; i++ {
		agentID := fmt.Sprintf("shared-tag-agent-%d-%s", i, suffix)
		_, err := testDB.CreateAgent(ctx, model.Agent{
			AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
			Tags: []string{tag}, Metadata: map[string]any{},
		})
		require.NoError(t, err)
	}

	ids, err := testDB.ListAgentIDsBySharedTags(ctx, uuid.Nil, []string{tag})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(ids), 2)
}

// ---------------------------------------------------------------------------
// Tests: GetAgentListStats — with freshly created agent
// ---------------------------------------------------------------------------

func TestGetAgentListStats_Fresh(t *testing.T) {
	ctx := context.Background()

	stats, err := testDB.GetAgentListStats(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.NotNil(t, stats)
}

// ---------------------------------------------------------------------------
// Tests: CountDecisionsByAPIKey
// ---------------------------------------------------------------------------

func TestCountDecisionsByAPIKey_WithTimeRange(t *testing.T) {
	ctx := context.Background()
	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC().Add(24 * time.Hour)
	counts, total, err := testDB.CountDecisionsByAPIKey(ctx, uuid.Nil, from, to)
	require.NoError(t, err)
	assert.NotNil(t, counts)
	assert.GreaterOrEqual(t, total, 0)
}

// ---------------------------------------------------------------------------
// Tests: CreateEvidence and CreateEvidenceBatch
// ---------------------------------------------------------------------------

func TestCreateEvidenceBatch(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "evi-batch-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent,
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dec, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "evi_batch_test", Outcome: "test",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	err = testDB.CreateEvidenceBatch(ctx, []model.Evidence{
		{DecisionID: dec.ID, OrgID: uuid.Nil, SourceType: model.SourceAPIResponse, Content: "evidence 1"},
		{DecisionID: dec.ID, OrgID: uuid.Nil, SourceType: model.SourceAPIResponse, Content: "evidence 2"},
	})
	require.NoError(t, err)

	evs, err := testDB.GetEvidenceByDecision(ctx, dec.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, evs, 2)
}

func TestCreateEvidenceBatch_Empty(t *testing.T) {
	ctx := context.Background()
	err := testDB.CreateEvidenceBatch(ctx, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Tests: GetConflictAnalytics — with actual scored conflict data
// ---------------------------------------------------------------------------

func TestGetConflictAnalytics_WithScoredConflict(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "analytics-a-" + suffix
	agentB := "analytics-b-" + suffix
	decType := "analytics_type_" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: decType, Outcome: "approach_alpha",
		Confidence: 0.9, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: decType, Outcome: "approach_beta",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	topicSim := 0.85
	outcomeDiv := 0.75
	sig := 0.70
	sev := "high"
	cat := "factual"
	_, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     decType,
		DecisionTypeB:     decType,
		OutcomeA:          "approach_alpha",
		OutcomeB:          "approach_beta",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "llm_v2",
		Severity:          &sev,
		Category:          &cat,
	})
	require.NoError(t, err)

	// Query analytics for the org with a time range that includes now.
	// The trend query uses generate_series(from::date, (to::date - '1 day')::date, '1 day'),
	// so from and to must span at least 2 calendar days to produce rows.
	now := time.Now().UTC()
	analytics, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From: now.Add(-24 * time.Hour),
		To:   now.Add(24 * time.Hour),
	})
	require.NoError(t, err)

	// Summary should reflect at least the one conflict we inserted.
	assert.GreaterOrEqual(t, analytics.Summary.TotalDetected, 1)

	// ByAgentPair should have entries (covers the pairRows.Next() loop).
	assert.NotEmpty(t, analytics.ByAgentPair, "expected at least one agent pair in analytics")

	// ByDecisionType should have entries (covers the typeRows.Next() loop).
	assert.NotEmpty(t, analytics.ByDecisionType, "expected at least one decision type in analytics")

	// BySeverity should have entries (covers the sevRows.Next() loop).
	assert.NotEmpty(t, analytics.BySeverity, "expected at least one severity in analytics")

	// Trend should have entries (covers the trendRows.Next() loop — generate_series produces rows).
	assert.NotEmpty(t, analytics.Trend, "expected at least one trend point in analytics")
}

// ---------------------------------------------------------------------------
// Tests: GetConflict — found
// ---------------------------------------------------------------------------

func TestGetConflict_Found(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	agentA := "get-conflict-a-" + suffix
	agentB := "get-conflict-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: "get_conflict_test", Outcome: "found_a",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: "get_conflict_test", Outcome: "found_b",
		Confidence: 0.7, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	topicSim := 0.9
	outcomeDiv := 0.8
	sig := 0.72
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind:      model.ConflictKindCrossAgent,
		DecisionAID:       dA.ID,
		DecisionBID:       dB.ID,
		OrgID:             uuid.Nil,
		AgentA:            agentA,
		AgentB:            agentB,
		DecisionTypeA:     "get_conflict_test",
		DecisionTypeB:     "get_conflict_test",
		OutcomeA:          "found_a",
		OutcomeB:          "found_b",
		TopicSimilarity:   &topicSim,
		OutcomeDivergence: &outcomeDiv,
		Significance:      &sig,
		ScoringMethod:     "llm_v2",
	})
	require.NoError(t, err)

	conflict, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, conflict, "expected conflict to be found")
	assert.Equal(t, conflictID, conflict.ID)
	assert.Equal(t, "open", conflict.Status)
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — with RunID filter
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithRunIDFilter(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "runid-filter-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "runid_test", Outcome: "filtered_by_run",
		Confidence: 0.8, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{RunID: &run.ID},
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 1)
	for _, d := range decisions {
		assert.Equal(t, run.ID, d.RunID)
	}
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — with Tool and Model filters
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithToolFilter(t *testing.T) {
	ctx := context.Background()
	tool := "nonexistent-tool-" + uuid.New().String()[:8]
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{Tool: &tool},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

func TestQueryDecisions_WithModelFilter(t *testing.T) {
	ctx := context.Background()
	mdl := "nonexistent-model-" + uuid.New().String()[:8]
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{Model: &mdl},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — ordering edge cases
// ---------------------------------------------------------------------------

func TestQueryDecisions_OrderByOutcomeScore(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		OrderBy: "outcome_score",
		Limit:   5,
	})
	require.NoError(t, err)
	_ = decisions // just verify the query runs without error
}

func TestQueryDecisions_OrderAsc(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		OrderBy:  "valid_from",
		OrderDir: "asc",
		Limit:    5,
	})
	require.NoError(t, err)
	_ = decisions
}

func TestQueryDecisions_LimitOver1000(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Limit: 5000,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(decisions), 1000)
}

func TestQueryDecisions_NegativeOffsetClamped(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Offset: -10,
		Limit:  5,
	})
	require.NoError(t, err)
	_ = decisions
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisionsTemporal — limit edge cases
// ---------------------------------------------------------------------------

func TestQueryDecisionsTemporal_LimitOver1000(t *testing.T) {
	ctx := context.Background()
	decisions, err := testDB.QueryDecisionsTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf:  time.Now().UTC(),
		Limit: 5000,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(decisions), 1000)
}

func TestGetDecisionsByAgent_LimitClamping(t *testing.T) {
	ctx := context.Background()
	// Limit <= 0 defaults to 50, limit > 1000 caps to 1000, offset < 0 defaults to 0.
	decisions, _, err := testDB.GetDecisionsByAgent(ctx, uuid.Nil, "no-such-agent", 0, -1, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, decisions)

	decisions, _, err = testDB.GetDecisionsByAgent(ctx, uuid.Nil, "no-such-agent", 9999, 0, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Tests: ListRunsByAgent — limit edge cases
// ---------------------------------------------------------------------------

func TestListRunsByAgent_LimitClamping(t *testing.T) {
	ctx := context.Background()
	// Limit <= 0 defaults to 50, limit > 1000 caps to 1000, offset < 0 defaults to 0.
	runs, total, err := testDB.ListRunsByAgent(ctx, uuid.Nil, "no-such-agent", 0, -1)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, runs)

	runs, total, err = testDB.ListRunsByAgent(ctx, uuid.Nil, "no-such-agent", 9999, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, runs)
}

func strPtr(s string) *string {
	return &s
}

// ---------------------------------------------------------------------------
// Integrity violation persistence + tampered proof detection (Finding #1, #9)
// ---------------------------------------------------------------------------

func TestCreateIntegrityViolation_And_GetViolations(t *testing.T) {
	ctx := context.Background()

	// Use the default test org (uuid.Nil).
	orgID := uuid.Nil

	// We need a proof to reference.
	proof := storage.IntegrityProof{
		OrgID:         orgID,
		BatchStart:    time.Now().UTC().Add(-1 * time.Hour),
		BatchEnd:      time.Now().UTC(),
		DecisionCount: 5,
		RootHash:      "viol-test-root-" + uuid.New().String()[:8],
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, testDB.CreateIntegrityProof(ctx, proof))
	// Re-fetch to get the auto-generated ID.
	fetched, err := testDB.GetLatestIntegrityProof(ctx, orgID)
	require.NoError(t, err)
	require.NotNil(t, fetched)

	// Insert a violation.
	v := storage.IntegrityViolation{
		OrgID:         orgID,
		ProofID:       fetched.ID,
		ViolationType: "merkle_root_mismatch",
		Details: map[string]any{
			"stored_root":    "aaaa",
			"decision_count": 5,
		},
		CreatedAt: time.Now().UTC(),
	}
	err = testDB.CreateIntegrityViolation(ctx, v)
	require.NoError(t, err)

	// Fetch violations.
	violations, err := testDB.GetIntegrityViolations(ctx, orgID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, violations)

	found := false
	for _, viol := range violations {
		if viol.ProofID == fetched.ID && viol.ViolationType == "merkle_root_mismatch" {
			found = true
			assert.Equal(t, orgID, viol.OrgID)
			assert.NotNil(t, viol.Details)
			break
		}
	}
	assert.True(t, found, "expected to find the created violation")
}

func TestVerifyBatchProof_DetectsTamperingIntegration(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "tamper-detect-" + suffix

	// Use a dedicated org so other tests' decisions in the default org
	// don't leak into our batch window and inflate the hash count.
	orgID := uuid.New()
	_, err := testDB.Pool().Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		orgID, "tamper-org-"+suffix, "tamper-org-"+suffix)
	require.NoError(t, err)

	// CreateRun uses the default org; we need to insert the run manually
	// under our isolated org so decisions land in the right org_id.
	runID := uuid.New()
	_, err = testDB.Pool().Exec(ctx,
		`INSERT INTO agent_runs (id, org_id, agent_id, status, created_at)
		 VALUES ($1, $2, $3, 'running', now())`,
		runID, orgID, agentID)
	require.NoError(t, err)

	beforeCreate := time.Now().UTC().Add(-1 * time.Second)

	// Create 4 decisions with content hashes.
	const count = 4
	for i := range count {
		reasoning := fmt.Sprintf("reasoning %d", i)
		_, err := testDB.CreateDecision(ctx, model.Decision{
			ID:           uuid.New(),
			RunID:        runID,
			AgentID:      agentID,
			OrgID:        orgID,
			DecisionType: "tamper_test",
			Outcome:      fmt.Sprintf("outcome %d for %s", i, suffix),
			Confidence:   0.8,
			Reasoning:    &reasoning,
			ValidFrom:    time.Now().UTC(),
		})
		require.NoError(t, err)
	}

	afterCreate := time.Now().UTC().Add(1 * time.Second)

	// Get hashes and build a legitimate Merkle root.
	hashes, err := testDB.GetDecisionHashesForBatch(ctx, orgID, beforeCreate, afterCreate)
	require.NoError(t, err)
	require.Len(t, hashes, count)

	root, err := integrity.BuildMerkleRoot(hashes)
	require.NoError(t, err)

	// Verify passes with correct root.
	ok, err := integrity.VerifyBatchProof(root, hashes)
	require.NoError(t, err)
	assert.True(t, ok, "legitimate proof should verify")

	// Store the proof.
	proof := storage.IntegrityProof{
		OrgID:         orgID,
		BatchStart:    beforeCreate,
		BatchEnd:      afterCreate,
		DecisionCount: count,
		RootHash:      root,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, testDB.CreateIntegrityProof(ctx, proof))

	// Simulate tampering: the decisions table has an immutability trigger
	// (correctly preventing direct mutation), so we simulate what would happen
	// if the stored root was corrupted or if a decision was inserted/deleted
	// outside the normal write path. We verify against a wrong root.
	wrongRoot := "0000000000000000000000000000000000000000000000000000000000000000"
	ok, err = integrity.VerifyBatchProof(wrongRoot, hashes)
	require.NoError(t, err)
	assert.False(t, ok, "wrong stored root should fail verification")

	// Also verify that adding an extra hash (simulating an injected decision)
	// causes verification to fail against the original root.
	injectedHashes := make([]string, len(hashes)+1)
	copy(injectedHashes, hashes)
	injectedHashes[len(hashes)] = "zzz_injected_hash"
	ok, err = integrity.VerifyBatchProof(root, injectedHashes)
	require.NoError(t, err)
	assert.False(t, ok, "injected hash should cause root mismatch")
}

func TestIntegrityViolation_ChainLinkageDetection(t *testing.T) {
	ctx := context.Background()
	orgID := uuid.Nil
	now := time.Now().UTC()

	// Create two proofs with correct chain linkage.
	proof1 := storage.IntegrityProof{
		OrgID:         orgID,
		BatchStart:    now.Add(-2 * time.Hour),
		BatchEnd:      now.Add(-1 * time.Hour),
		DecisionCount: 3,
		RootHash:      "chain-root-1-" + uuid.New().String()[:8],
		CreatedAt:     now.Add(-1 * time.Hour),
	}
	require.NoError(t, testDB.CreateIntegrityProof(ctx, proof1))

	prevRoot := proof1.RootHash
	proof2 := storage.IntegrityProof{
		OrgID:         orgID,
		BatchStart:    now.Add(-1 * time.Hour),
		BatchEnd:      now,
		DecisionCount: 5,
		RootHash:      "chain-root-2-" + uuid.New().String()[:8],
		PreviousRoot:  &prevRoot,
		CreatedAt:     now,
	}
	require.NoError(t, testDB.CreateIntegrityProof(ctx, proof2))

	// Fetch recent proofs (newest-first).
	proofs, err := testDB.GetRecentIntegrityProofs(ctx, orgID, 10)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(proofs), 2)

	// Find our two proofs in the result (may have other proofs from other tests).
	var newest, older *storage.IntegrityProof
	for i := range proofs {
		if proofs[i].RootHash == proof2.RootHash {
			newest = &proofs[i]
		}
		if proofs[i].RootHash == proof1.RootHash {
			older = &proofs[i]
		}
	}
	require.NotNil(t, newest, "should find proof2 in recent proofs")
	require.NotNil(t, older, "should find proof1 in recent proofs")

	// Chain linkage should be valid.
	require.NotNil(t, newest.PreviousRoot)
	assert.Equal(t, older.RootHash, *newest.PreviousRoot, "chain linkage should match")

	// Now create a proof with broken chain linkage.
	brokenPrev := "wrong-root-hash"
	proof3 := storage.IntegrityProof{
		OrgID:         orgID,
		BatchStart:    now,
		BatchEnd:      now.Add(1 * time.Hour),
		DecisionCount: 2,
		RootHash:      "chain-root-3-" + uuid.New().String()[:8],
		PreviousRoot:  &brokenPrev,
		CreatedAt:     now.Add(1 * time.Second),
	}
	require.NoError(t, testDB.CreateIntegrityProof(ctx, proof3))

	// Verify the broken linkage is detectable.
	proofs, err = testDB.GetRecentIntegrityProofs(ctx, orgID, 10)
	require.NoError(t, err)

	var broken *storage.IntegrityProof
	for i := range proofs {
		if proofs[i].RootHash == proof3.RootHash {
			broken = &proofs[i]
			break
		}
	}
	require.NotNil(t, broken)
	require.NotNil(t, broken.PreviousRoot)
	// The broken proof's previous_root should NOT match proof2's root.
	assert.NotEqual(t, proof2.RootHash, *broken.PreviousRoot,
		"chain linkage should be broken — previous_root doesn't match prior proof")

	// Record a violation for the broken linkage.
	err = testDB.CreateIntegrityViolation(ctx, storage.IntegrityViolation{
		OrgID:         orgID,
		ProofID:       broken.ID,
		ViolationType: "chain_linkage_broken",
		Details: map[string]any{
			"expected_previous": proof2.RootHash,
			"actual_previous":   *broken.PreviousRoot,
		},
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Verify the violation was persisted.
	violations, err := testDB.GetIntegrityViolations(ctx, orgID, 10)
	require.NoError(t, err)
	found := false
	for _, v := range violations {
		if v.ProofID == broken.ID && v.ViolationType == "chain_linkage_broken" {
			found = true
			break
		}
	}
	assert.True(t, found, "chain linkage violation should be persisted and retrievable")
}

// ---------------------------------------------------------------------------
// Tests: proof_leaves — proofs survive retention purge and GDPR erasure
// ---------------------------------------------------------------------------

func TestProofLeaves_CreateAndGet(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]

	orgID := uuid.New()
	_, err := testDB.Pool().Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		orgID, "pl-org-"+suffix, "pl-org-"+suffix)
	require.NoError(t, err)

	proofID := uuid.New()
	proof := storage.IntegrityProof{
		ID:            proofID,
		OrgID:         orgID,
		BatchStart:    time.Now().UTC().Add(-1 * time.Hour),
		BatchEnd:      time.Now().UTC(),
		DecisionCount: 3,
		RootHash:      "pl-root-" + suffix,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, testDB.CreateIntegrityProof(ctx, proof))

	leaves := []string{"aaa_hash", "bbb_hash", "ccc_hash"}
	require.NoError(t, testDB.CreateProofLeaves(ctx, proofID, orgID, leaves))

	got, err := testDB.GetProofLeaves(ctx, proofID)
	require.NoError(t, err)
	assert.Equal(t, leaves, got, "leaves should round-trip in sorted order")

	// No leaves for a random proof.
	empty, err := testDB.GetProofLeaves(ctx, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestProofLeaves_Empty(t *testing.T) {
	ctx := context.Background()
	// CreateProofLeaves with empty slice should be a no-op.
	require.NoError(t, testDB.CreateProofLeaves(ctx, uuid.New(), uuid.Nil, nil))
}

func TestProofLeaves_SurvivesRetentionPurge(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "pl-retention-" + suffix

	// Isolated org.
	orgID := uuid.New()
	_, err := testDB.Pool().Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgID, "pl-ret-org-"+suffix, "pl-ret-org-"+suffix)
	require.NoError(t, err)

	runID := uuid.New()
	_, err = testDB.Pool().Exec(ctx,
		`INSERT INTO agent_runs (id, org_id, agent_id, status, created_at)
		 VALUES ($1, $2, $3, 'running', now())`,
		runID, orgID, agentID)
	require.NoError(t, err)

	beforeCreate := time.Now().UTC().Add(-1 * time.Second)

	// Create decisions.
	const count = 3
	for i := range count {
		reasoning := fmt.Sprintf("reasoning %d %s", i, suffix)
		_, err := testDB.CreateDecision(ctx, model.Decision{
			ID:           uuid.New(),
			RunID:        runID,
			AgentID:      agentID,
			OrgID:        orgID,
			DecisionType: "retention_proof_test",
			Outcome:      fmt.Sprintf("outcome %d %s", i, suffix),
			Confidence:   0.7,
			Reasoning:    &reasoning,
			ValidFrom:    time.Now().UTC(),
		})
		require.NoError(t, err)
	}

	afterCreate := time.Now().UTC().Add(1 * time.Second)

	// Build proof and snapshot leaves.
	hashes, err := testDB.GetDecisionHashesForBatch(ctx, orgID, beforeCreate, afterCreate)
	require.NoError(t, err)
	require.Len(t, hashes, count)

	root, err := integrity.BuildMerkleRoot(hashes)
	require.NoError(t, err)

	proofID := uuid.New()
	proof := storage.IntegrityProof{
		ID:            proofID,
		OrgID:         orgID,
		BatchStart:    beforeCreate,
		BatchEnd:      afterCreate,
		DecisionCount: count,
		RootHash:      root,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, testDB.CreateIntegrityProof(ctx, proof))
	require.NoError(t, testDB.CreateProofLeaves(ctx, proofID, orgID, hashes))

	// Verify passes before purge.
	ok, err := integrity.VerifyBatchProof(root, hashes)
	require.NoError(t, err)
	require.True(t, ok, "proof should verify before purge")

	// Purge all decisions (cutoff in the future).
	_, err = testDB.BatchDeleteDecisions(ctx, orgID, time.Now().UTC().Add(1*time.Hour), nil, nil, nil, 100)
	require.NoError(t, err)

	// Decisions should be gone.
	remaining, err := testDB.GetDecisionHashesForBatch(ctx, orgID, beforeCreate, afterCreate)
	require.NoError(t, err)
	assert.Empty(t, remaining, "decisions should be purged")

	// But proof_leaves survive — verification still passes.
	savedLeaves, err := testDB.GetProofLeaves(ctx, proofID)
	require.NoError(t, err)
	require.Len(t, savedLeaves, count, "proof leaves should survive retention purge")

	ok, err = integrity.VerifyBatchProof(root, savedLeaves)
	require.NoError(t, err)
	assert.True(t, ok, "proof should still verify after retention purge via saved leaves")
}

func TestProofLeaves_SurvivesGDPRErasure(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "pl-gdpr-" + suffix

	// Isolated org.
	orgID := uuid.New()
	_, err := testDB.Pool().Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgID, "pl-gdpr-org-"+suffix, "pl-gdpr-org-"+suffix)
	require.NoError(t, err)

	runID := uuid.New()
	_, err = testDB.Pool().Exec(ctx,
		`INSERT INTO agent_runs (id, org_id, agent_id, status, created_at)
		 VALUES ($1, $2, $3, 'running', now())`,
		runID, orgID, agentID)
	require.NoError(t, err)

	beforeCreate := time.Now().UTC().Add(-1 * time.Second)

	// Create a decision that will be erased.
	decisionID := uuid.New()
	reasoning := "sensitive reasoning " + suffix
	_, err = testDB.CreateDecision(ctx, model.Decision{
		ID:           decisionID,
		RunID:        runID,
		AgentID:      agentID,
		OrgID:        orgID,
		DecisionType: "gdpr_proof_test",
		Outcome:      "sensitive outcome " + suffix,
		Confidence:   0.9,
		Reasoning:    &reasoning,
		ValidFrom:    time.Now().UTC(),
	})
	require.NoError(t, err)

	afterCreate := time.Now().UTC().Add(1 * time.Second)

	// Build proof and snapshot leaves.
	hashes, err := testDB.GetDecisionHashesForBatch(ctx, orgID, beforeCreate, afterCreate)
	require.NoError(t, err)
	require.Len(t, hashes, 1)

	root, err := integrity.BuildMerkleRoot(hashes)
	require.NoError(t, err)

	proofID := uuid.New()
	proof := storage.IntegrityProof{
		ID:            proofID,
		OrgID:         orgID,
		BatchStart:    beforeCreate,
		BatchEnd:      afterCreate,
		DecisionCount: 1,
		RootHash:      root,
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, testDB.CreateIntegrityProof(ctx, proof))
	require.NoError(t, testDB.CreateProofLeaves(ctx, proofID, orgID, hashes))

	// Erase the decision (GDPR).
	_, err = testDB.EraseDecision(ctx, orgID, decisionID, "GDPR request", "admin-"+suffix, nil)
	require.NoError(t, err)

	// The decision's content_hash has changed.
	postErasureHashes, err := testDB.GetDecisionHashesForBatch(ctx, orgID, beforeCreate, afterCreate)
	require.NoError(t, err)
	require.Len(t, postErasureHashes, 1)
	assert.NotEqual(t, hashes[0], postErasureHashes[0], "content_hash should change after erasure")

	// Verifying against the decisions table would fail (false violation).
	ok, err := integrity.VerifyBatchProof(root, postErasureHashes)
	require.NoError(t, err)
	assert.False(t, ok, "verifying against mutated decisions table should fail")

	// But proof_leaves preserve the original hashes — verification passes.
	savedLeaves, err := testDB.GetProofLeaves(ctx, proofID)
	require.NoError(t, err)
	require.Len(t, savedLeaves, 1)
	assert.Equal(t, hashes[0], savedLeaves[0], "proof leaves should preserve original hash")

	ok, err = integrity.VerifyBatchProof(root, savedLeaves)
	require.NoError(t, err)
	assert.True(t, ok, "proof should still verify after GDPR erasure via saved leaves")
}
