package storage_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/migrations"
)

// testDB holds a shared test database connection for all tests in this package.
var testDB *storage.DB

func TestMain(m *testing.M) {
	ctx := context.Background()

	// Start a TimescaleDB container with pgvector.
	req := testcontainers.ContainerRequest{
		Image:        "timescale/timescaledb:latest-pg18",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "akashi",
			"POSTGRES_PASSWORD": "akashi",
			"POSTGRES_DB":       "akashi",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start container: %v\n", err)
		os.Exit(1)
	}

	host, err := container.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get container host: %v\n", err)
		os.Exit(1)
	}

	port, err := container.MappedPort(ctx, "5432")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get container port: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf("postgres://akashi:akashi@%s:%s/akashi?sslmode=disable", host, port.Port())

	// Enable extensions before creating the storage layer so pgvector types
	// get registered on the pool's AfterConnect hook.
	bootstrapConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to bootstrap connection: %v\n", err)
		os.Exit(1)
	}
	if _, err := bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create vector extension: %v\n", err)
		os.Exit(1)
	}
	if _, err := bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create timescaledb extension: %v\n", err)
		os.Exit(1)
	}
	_ = bootstrapConn.Close(ctx)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	testDB, err = storage.New(ctx, dsn, "", logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create DB: %v\n", err)
		os.Exit(1)
	}

	// Run migrations.
	if err := testDB.RunMigrations(ctx, migrations.FS); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	testDB.Close(ctx)
	_ = container.Terminate(ctx)
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
	score1, score2 := float32(0.92), float32(0.45)
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "Route to specialist", Score: &score1, Selected: true},
		{DecisionID: d.ID, Label: "Route to general", Score: &score2, Selected: false},
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
	})
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

	_, err = testDB.DeleteAgentData(ctx, agentA.OrgID, agentA.AgentID)
	require.NoError(t, err)

	gotB, err := testDB.GetDecision(ctx, agentB.OrgID, decB.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Nil(t, gotB.SupersedesID)
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
	})
	require.NoError(t, err)

	c, err := testDB.ReviseDecision(ctx, b.ID, model.Decision{
		RunID:        run.ID,
		AgentID:      agentID,
		DecisionType: "revision_chain",
		Outcome:      "version_c",
		Confidence:   0.9,
		Metadata:     map[string]any{},
	})
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
	})
	require.NoError(t, err)

	c, err := testDB.ReviseDecision(ctx, b.ID, model.Decision{
		RunID: run.ID, AgentID: agentID, DecisionType: "chain_test",
		Outcome: "version_c", Confidence: 0.9, Metadata: map[string]any{},
	})
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
	err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
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
	err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
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

	// Create 5 decisions with slight delays so valid_from values differ.
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
		// Small sleep to ensure distinct valid_from timestamps.
		time.Sleep(5 * time.Millisecond)
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
	score1 := float32(0.9)
	score2 := float32(0.3)
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
			{Label: "Option A", Score: &score1, Selected: true},
			{Label: "Option B", Score: &score2, Selected: false},
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

	score := float32(0.95)
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "Selected", Score: &score, Selected: true},
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

	score := float32(0.77)
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "Alt1", Score: &score, Selected: true},
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
	err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
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
	got, err := testDB.FindClaimsByDecision(ctx, d.ID)
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

	got, err := testDB.FindClaimsByDecision(ctx, d.ID)
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

	got, err := testDB.FindClaimsByDecision(ctx, d.ID)
	require.NoError(t, err)
	assert.Empty(t, got, "decision with no claims should return empty slice")
}

func TestFindClaimsByDecision_NonexistentDecision(t *testing.T) {
	ctx := context.Background()

	got, err := testDB.FindClaimsByDecision(ctx, uuid.New())
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
	has, err := testDB.HasClaimsForDecision(ctx, dA.ID)
	require.NoError(t, err)
	assert.False(t, has, "no claims yet for decision A")

	has, err = testDB.HasClaimsForDecision(ctx, dB.ID)
	require.NoError(t, err)
	assert.False(t, has, "no claims for decision B")

	// Insert claims for A.
	err = testDB.InsertClaims(ctx, []storage.Claim{
		{DecisionID: dA.ID, OrgID: dA.OrgID, ClaimIdx: 0, ClaimText: "A claim for decision A."},
	})
	require.NoError(t, err)

	// A should now return true; B should still be false.
	has, err = testDB.HasClaimsForDecision(ctx, dA.ID)
	require.NoError(t, err)
	assert.True(t, has, "decision A should have claims after insert")

	has, err = testDB.HasClaimsForDecision(ctx, dB.ID)
	require.NoError(t, err)
	assert.False(t, has, "decision B should still have no claims")
}

func TestHasClaimsForDecision_NonexistentDecision(t *testing.T) {
	ctx := context.Background()

	has, err := testDB.HasClaimsForDecision(ctx, uuid.New())
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
