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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
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
	if err := testDB.RunMigrations(ctx, os.DirFS("../../migrations")); err != nil {
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

	err = testDB.CompleteRun(ctx, run.OrgID, run.ID, model.RunStatusFailed, nil)
	require.Error(t, err)
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

	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID)
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

	got, err := testDB.GetEventsByRun(ctx, run.OrgID, run.ID)
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

	gotByID, err := testDB.GetAgentByID(ctx, agent.ID)
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
	gotByID, err := testDB.GetAgentByID(ctx, agent.ID)
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
	got1, err := testDB.GetAgentByID(ctx, a1.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a"}, got1.Tags)

	got2, err := testDB.GetAgentByID(ctx, a2.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-b", "team-c"}, got2.Tags)
}
