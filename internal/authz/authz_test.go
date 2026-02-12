package authz_test

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

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

var testDB *storage.DB

func TestMain(m *testing.M) {
	ctx := context.Background()

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

	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "5432")
	dsn := fmt.Sprintf("postgres://akashi:akashi@%s:%s/akashi?sslmode=disable", host, port.Port())

	bootstrapConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to bootstrap: %v\n", err)
		os.Exit(1)
	}
	_, _ = bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	_, _ = bootstrapConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb")
	_ = bootstrapConn.Close(ctx)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	testDB, err = storage.New(ctx, dsn, "", logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create DB: %v\n", err)
		os.Exit(1)
	}

	if err := testDB.RunMigrations(ctx, os.DirFS("../../migrations")); err != nil {
		fmt.Fprintf(os.Stderr, "migrations failed: %v\n", err)
		os.Exit(1)
	}

	// Ensure the default org exists.
	if err := testDB.EnsureDefaultOrg(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ensure default org: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	testDB.Close(ctx)
	_ = container.Terminate(ctx)
	os.Exit(code)
}

// makeClaims creates test claims with the given role and agent.
func makeClaims(agentID string, agentUUID uuid.UUID, role model.AgentRole) *auth.Claims {
	c := &auth.Claims{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Role:    role,
	}
	c.Subject = agentUUID.String()
	return c
}

func createTestAgent(t *testing.T, agentID string, role model.AgentRole, tags []string) model.Agent {
	t.Helper()
	a, err := testDB.CreateAgent(context.Background(), model.Agent{
		AgentID: agentID,
		OrgID:   uuid.Nil,
		Name:    agentID,
		Role:    role,
		Tags:    tags,
	})
	require.NoError(t, err)
	return a
}

func TestLoadGrantedSet_AdminReturnsNil(t *testing.T) {
	claims := makeClaims("admin-authz-test", uuid.New(), model.RoleAdmin)

	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	assert.Nil(t, granted, "admin should get nil (unrestricted)")
}

func TestLoadGrantedSet_AgentGetsSelfAccess(t *testing.T) {
	agent := createTestAgent(t, "self-access-"+uuid.New().String()[:8], model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted)
	assert.True(t, granted[agent.AgentID], "agent should see own decisions")
}

func TestLoadGrantedSet_MalformedSubjectReturnsEmpty(t *testing.T) {
	c := &auth.Claims{
		AgentID: "bad-sub-agent",
		OrgID:   uuid.Nil,
		Role:    model.RoleAgent,
	}
	c.Subject = "not-a-uuid"

	granted, err := authz.LoadGrantedSet(context.Background(), testDB, c, nil)
	require.NoError(t, err)
	assert.NotNil(t, granted, "should return non-nil (restricted)")
	assert.Empty(t, granted, "malformed subject should yield empty set (no access)")
}

func TestLoadGrantedSet_TagOverlap(t *testing.T) {
	suffix := uuid.New().String()[:8]
	tag := "team-" + suffix
	agent1 := createTestAgent(t, "tag1-"+suffix, model.RoleAgent, []string{tag})
	agent2 := createTestAgent(t, "tag2-"+suffix, model.RoleAgent, []string{tag})

	claims := makeClaims(agent1.AgentID, agent1.ID, model.RoleAgent)

	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted)
	assert.True(t, granted[agent1.AgentID], "should include self")
	assert.True(t, granted[agent2.AgentID], "should include tag-matched agent")
}

func TestLoadGrantedSet_ExplicitGrant(t *testing.T) {
	suffix := uuid.New().String()[:8]
	grantee := createTestAgent(t, "grantee-"+suffix, model.RoleAgent, nil)
	target := createTestAgent(t, "target-"+suffix, model.RoleAgent, nil)

	// Create an explicit access grant.
	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    target.ID,
		GranteeID:    grantee.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &target.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	claims := makeClaims(grantee.AgentID, grantee.ID, model.RoleAgent)

	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted)
	assert.True(t, granted[grantee.AgentID], "should include self")
	assert.True(t, granted[target.AgentID], "should include granted agent")
}

func TestLoadGrantedSet_WithCache(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "cached-"+suffix, model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	cache := authz.NewGrantCache(time.Second)
	defer cache.Close()

	// First call populates cache.
	granted1, err := authz.LoadGrantedSet(context.Background(), testDB, claims, cache)
	require.NoError(t, err)
	require.NotNil(t, granted1)
	assert.True(t, granted1[agent.AgentID])

	// Second call should return from cache (same pointer is not guaranteed,
	// but same content is).
	granted2, err := authz.LoadGrantedSet(context.Background(), testDB, claims, cache)
	require.NoError(t, err)
	assert.Equal(t, granted1, granted2)
}

func TestCanAccessAgent_AdminBypass(t *testing.T) {
	claims := makeClaims("admin-can-access", uuid.New(), model.RoleAdmin)

	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, "any-agent-id")
	require.NoError(t, err)
	assert.True(t, ok, "admin should access any agent")
}

func TestCanAccessAgent_SelfAccess(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "self-can-"+suffix, model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, agent.AgentID)
	require.NoError(t, err)
	assert.True(t, ok, "agent should access own data")
}

func TestCanAccessAgent_DeniedWithoutGrant(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "denied-"+suffix, model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, "other-agent-"+suffix)
	require.NoError(t, err)
	assert.False(t, ok, "agent without grant should be denied")
}

func TestFilterDecisions_AdminSeesAll(t *testing.T) {
	claims := makeClaims("admin-filter", uuid.New(), model.RoleAdmin)

	decisions := []model.Decision{
		{AgentID: "a"},
		{AgentID: "b"},
		{AgentID: "c"},
	}

	filtered, err := authz.FilterDecisions(context.Background(), testDB, claims, decisions, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 3, "admin should see all decisions")
}

func TestFilterDecisions_AgentSeesOnlyOwn(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "filter-"+suffix, model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	decisions := []model.Decision{
		{AgentID: agent.AgentID},
		{AgentID: "other-" + suffix},
	}

	filtered, err := authz.FilterDecisions(context.Background(), testDB, claims, decisions, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 1, "agent should only see own decisions")
	assert.Equal(t, agent.AgentID, filtered[0].AgentID)
}

func TestFilterConflicts_RequiresBothAgents(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "conflict-"+suffix, model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	conflicts := []model.DecisionConflict{
		{AgentA: agent.AgentID, AgentB: agent.AgentID},
		{AgentA: agent.AgentID, AgentB: "unknown"},
		{AgentA: "unknown", AgentB: agent.AgentID},
	}

	filtered, err := authz.FilterConflicts(context.Background(), testDB, claims, conflicts, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 1, "only conflicts where caller has access to both agents should be visible")
}

func TestFilterSearchResults_Filtered(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "search-"+suffix, model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	results := []model.SearchResult{
		{Decision: model.Decision{AgentID: agent.AgentID}, SimilarityScore: 0.9},
		{Decision: model.Decision{AgentID: "other-" + suffix}, SimilarityScore: 0.8},
	}

	filtered, err := authz.FilterSearchResults(context.Background(), testDB, claims, results, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 1)
	assert.Equal(t, agent.AgentID, filtered[0].Decision.AgentID)
}
