package authz_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/model"
)

// ---------------------------------------------------------------------------
// Grant expiry
// ---------------------------------------------------------------------------

func TestCanAccessAgent_ExpiredGrantDeniesAccess(t *testing.T) {
	suffix := uuid.New().String()[:8]
	caller := createTestAgent(t, "exp-caller-"+suffix, model.RoleAgent, nil)
	target := createTestAgent(t, "exp-target-"+suffix, model.RoleAgent, nil)

	// Create a grant that already expired.
	pastTime := time.Now().Add(-1 * time.Hour)
	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    target.ID,
		GranteeID:    caller.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &target.AgentID,
		Permission:   string(model.PermissionRead),
		ExpiresAt:    &pastTime,
	})
	require.NoError(t, err)

	claims := makeClaims(caller.AgentID, caller.ID, model.RoleAgent)
	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, target.AgentID)
	require.NoError(t, err)
	assert.False(t, ok, "expired grant should not confer access")
}

func TestLoadGrantedSet_ExpiredGrantExcluded(t *testing.T) {
	suffix := uuid.New().String()[:8]
	grantee := createTestAgent(t, "exp-grantee-"+suffix, model.RoleAgent, nil)
	target := createTestAgent(t, "exp-target2-"+suffix, model.RoleAgent, nil)

	pastTime := time.Now().Add(-1 * time.Hour)
	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    target.ID,
		GranteeID:    grantee.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &target.AgentID,
		Permission:   string(model.PermissionRead),
		ExpiresAt:    &pastTime,
	})
	require.NoError(t, err)

	claims := makeClaims(grantee.AgentID, grantee.ID, model.RoleAgent)
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted)
	assert.True(t, granted[grantee.AgentID], "should still include self")
	assert.False(t, granted[target.AgentID], "expired grant should not appear in granted set")
}

func TestCanAccessAgent_FutureExpiryGrants(t *testing.T) {
	suffix := uuid.New().String()[:8]
	caller := createTestAgent(t, "fut-caller-"+suffix, model.RoleAgent, nil)
	target := createTestAgent(t, "fut-target-"+suffix, model.RoleAgent, nil)

	futureTime := time.Now().Add(24 * time.Hour)
	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    target.ID,
		GranteeID:    caller.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &target.AgentID,
		Permission:   string(model.PermissionRead),
		ExpiresAt:    &futureTime,
	})
	require.NoError(t, err)

	claims := makeClaims(caller.AgentID, caller.ID, model.RoleAgent)
	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, target.AgentID)
	require.NoError(t, err)
	assert.True(t, ok, "non-expired grant should confer access")
}

// ---------------------------------------------------------------------------
// Grant revocation
// ---------------------------------------------------------------------------

func TestCanAccessAgent_RevokedGrantDeniesAccess(t *testing.T) {
	suffix := uuid.New().String()[:8]
	caller := createTestAgent(t, "rev-caller-"+suffix, model.RoleAgent, nil)
	target := createTestAgent(t, "rev-target-"+suffix, model.RoleAgent, nil)

	grant, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    target.ID,
		GranteeID:    caller.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &target.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	claims := makeClaims(caller.AgentID, caller.ID, model.RoleAgent)

	// Access should work before revocation.
	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, target.AgentID)
	require.NoError(t, err)
	assert.True(t, ok, "grant should confer access before revocation")

	// Revoke the grant.
	err = testDB.DeleteGrant(context.Background(), uuid.Nil, grant.ID)
	require.NoError(t, err)

	// Access should be denied after revocation.
	ok, err = authz.CanAccessAgent(context.Background(), testDB, claims, target.AgentID)
	require.NoError(t, err)
	assert.False(t, ok, "revoked grant should not confer access")
}

func TestLoadGrantedSet_CacheInvalidationAfterRevocation(t *testing.T) {
	suffix := uuid.New().String()[:8]
	grantee := createTestAgent(t, "cache-rev-grantee-"+suffix, model.RoleAgent, nil)
	target := createTestAgent(t, "cache-rev-target-"+suffix, model.RoleAgent, nil)

	grant, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    target.ID,
		GranteeID:    grantee.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &target.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	claims := makeClaims(grantee.AgentID, grantee.ID, model.RoleAgent)
	cache := authz.NewGrantCache(5 * time.Second)
	defer cache.Close()

	// Populate cache.
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, cache)
	require.NoError(t, err)
	assert.True(t, granted[target.AgentID], "should see target before revocation")

	// Revoke.
	err = testDB.DeleteGrant(context.Background(), uuid.Nil, grant.ID)
	require.NoError(t, err)

	// Without invalidation, cache still returns stale data.
	stale, err := authz.LoadGrantedSet(context.Background(), testDB, claims, cache)
	require.NoError(t, err)
	assert.True(t, stale[target.AgentID], "stale cache should still show target")

	// After invalidation, fresh load should exclude revoked grant.
	cacheKey := uuid.Nil.String() + ":" + grantee.ID.String()
	cache.Invalidate(cacheKey)

	fresh, err := authz.LoadGrantedSet(context.Background(), testDB, claims, cache)
	require.NoError(t, err)
	assert.False(t, fresh[target.AgentID], "after invalidation, revoked grant should disappear")
}

// ---------------------------------------------------------------------------
// Reader role: minimal privilege
// ---------------------------------------------------------------------------

func TestLoadGrantedSet_ReaderGetsSelfOnly(t *testing.T) {
	suffix := uuid.New().String()[:8]
	reader := createTestAgent(t, "reader-self-"+suffix, model.RoleReader, nil)
	claims := makeClaims(reader.AgentID, reader.ID, model.RoleReader)

	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted, "reader should get non-nil (restricted) set")
	assert.Len(t, granted, 1, "reader with no grants/tags should only see self")
	assert.True(t, granted[reader.AgentID])
}

func TestCanAccessAgent_ReaderDeniedWithoutGrant(t *testing.T) {
	suffix := uuid.New().String()[:8]
	reader := createTestAgent(t, "reader-no-"+suffix, model.RoleReader, nil)
	_ = createTestAgent(t, "other-agent-"+suffix, model.RoleAgent, nil)

	claims := makeClaims(reader.AgentID, reader.ID, model.RoleReader)
	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, "other-agent-"+suffix)
	require.NoError(t, err)
	assert.False(t, ok, "reader without grant or tag overlap should be denied")
}

func TestCanAccessAgent_ReaderAllowedWithGrant(t *testing.T) {
	suffix := uuid.New().String()[:8]
	reader := createTestAgent(t, "reader-yes-"+suffix, model.RoleReader, nil)
	target := createTestAgent(t, "target-yes-"+suffix, model.RoleAgent, nil)

	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    target.ID,
		GranteeID:    reader.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &target.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	claims := makeClaims(reader.AgentID, reader.ID, model.RoleReader)
	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, target.AgentID)
	require.NoError(t, err)
	assert.True(t, ok, "reader with explicit grant should be allowed")
}

func TestCanAccessAgent_ReaderAllowedByTagOverlap(t *testing.T) {
	suffix := uuid.New().String()[:8]
	tag := "shared-team-" + suffix
	reader := createTestAgent(t, "reader-tag-"+suffix, model.RoleReader, []string{tag})
	target := createTestAgent(t, "target-tag-"+suffix, model.RoleAgent, []string{tag})

	claims := makeClaims(reader.AgentID, reader.ID, model.RoleReader)
	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, target.AgentID)
	require.NoError(t, err)
	assert.True(t, ok, "reader sharing a tag with target should be allowed")
}

// ---------------------------------------------------------------------------
// Role hierarchy: every role at or above admin bypasses grants
// ---------------------------------------------------------------------------

func TestLoadGrantedSet_OrgOwnerReturnsNil(t *testing.T) {
	claims := makeClaims("org-owner-test", uuid.New(), model.RoleOrgOwner)
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	assert.Nil(t, granted, "org_owner should get nil (unrestricted)")
}

func TestLoadGrantedSet_PlatformAdminReturnsNil(t *testing.T) {
	claims := makeClaims("platform-admin-test", uuid.New(), model.RolePlatformAdmin)
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	assert.Nil(t, granted, "platform_admin should get nil (unrestricted)")
}

func TestCanAccessAgent_OrgOwnerBypass(t *testing.T) {
	claims := makeClaims("org-owner-bypass", uuid.New(), model.RoleOrgOwner)
	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, "any-agent-id")
	require.NoError(t, err)
	assert.True(t, ok, "org_owner should access any agent")
}

func TestCanAccessAgent_PlatformAdminBypass(t *testing.T) {
	claims := makeClaims("platform-admin-bypass", uuid.New(), model.RolePlatformAdmin)
	ok, err := authz.CanAccessAgent(context.Background(), testDB, claims, "any-agent-id")
	require.NoError(t, err)
	assert.True(t, ok, "platform_admin should access any agent")
}

// ---------------------------------------------------------------------------
// Combined tags + grants
// ---------------------------------------------------------------------------

func TestLoadGrantedSet_CombinesTagsAndGrants(t *testing.T) {
	suffix := uuid.New().String()[:8]
	tag := "combo-team-" + suffix

	caller := createTestAgent(t, "combo-caller-"+suffix, model.RoleAgent, []string{tag})
	tagPeer := createTestAgent(t, "combo-tagpeer-"+suffix, model.RoleAgent, []string{tag})
	grantedAgent := createTestAgent(t, "combo-granted-"+suffix, model.RoleAgent, nil)
	unrelated := createTestAgent(t, "combo-unrelated-"+suffix, model.RoleAgent, nil)

	// Grant caller access to grantedAgent.
	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    grantedAgent.ID,
		GranteeID:    caller.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &grantedAgent.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	claims := makeClaims(caller.AgentID, caller.ID, model.RoleAgent)
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted)

	assert.True(t, granted[caller.AgentID], "should include self")
	assert.True(t, granted[tagPeer.AgentID], "should include tag-matched agent")
	assert.True(t, granted[grantedAgent.AgentID], "should include explicitly granted agent")
	assert.False(t, granted[unrelated.AgentID], "should not include unrelated agent")
}

// ---------------------------------------------------------------------------
// Filter functions: scoped visibility for decisions, conflicts, search
// ---------------------------------------------------------------------------

func TestFilterDecisions_ReaderSeesOnlyGranted(t *testing.T) {
	suffix := uuid.New().String()[:8]
	reader := createTestAgent(t, "filter-reader-"+suffix, model.RoleReader, nil)
	allowed := createTestAgent(t, "filter-allowed-"+suffix, model.RoleAgent, nil)

	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    allowed.ID,
		GranteeID:    reader.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &allowed.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	claims := makeClaims(reader.AgentID, reader.ID, model.RoleReader)

	decisions := []model.Decision{
		{AgentID: reader.AgentID},        // own
		{AgentID: allowed.AgentID},       // granted
		{AgentID: "forbidden-" + suffix}, // no access
	}

	filtered, err := authz.FilterDecisions(context.Background(), testDB, claims, decisions, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 2, "reader should see own + granted decisions only")

	ids := make(map[string]bool)
	for _, d := range filtered {
		ids[d.AgentID] = true
	}
	assert.True(t, ids[reader.AgentID])
	assert.True(t, ids[allowed.AgentID])
}

func TestFilterConflicts_ReaderNeedsAccessToBothAgents(t *testing.T) {
	suffix := uuid.New().String()[:8]
	reader := createTestAgent(t, "conf-reader-"+suffix, model.RoleReader, nil)
	agentA := createTestAgent(t, "conf-a-"+suffix, model.RoleAgent, nil)
	agentB := createTestAgent(t, "conf-b-"+suffix, model.RoleAgent, nil)

	// Grant access only to agentA, not agentB.
	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    agentA.ID,
		GranteeID:    reader.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &agentA.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	claims := makeClaims(reader.AgentID, reader.ID, model.RoleReader)

	conflicts := []model.DecisionConflict{
		// Reader can see both sides (self + agentA).
		{ConflictKind: model.ConflictKindCrossAgent, AgentA: reader.AgentID, AgentB: agentA.AgentID},
		// Reader can see agentA but not agentB — should be filtered.
		{ConflictKind: model.ConflictKindCrossAgent, AgentA: agentA.AgentID, AgentB: agentB.AgentID},
		// Reader can see neither side.
		{ConflictKind: model.ConflictKindCrossAgent, AgentA: "unknown-1", AgentB: "unknown-2"},
	}

	filtered, err := authz.FilterConflicts(context.Background(), testDB, claims, conflicts, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 1, "only conflict where reader has access to both agents should be visible")
	assert.Equal(t, reader.AgentID, filtered[0].AgentA)
	assert.Equal(t, agentA.AgentID, filtered[0].AgentB)
}

func TestFilterConflicts_AdminSeesAll(t *testing.T) {
	claims := makeClaims("admin-conf", uuid.New(), model.RoleAdmin)

	conflicts := []model.DecisionConflict{
		{ConflictKind: model.ConflictKindCrossAgent, AgentA: "a1", AgentB: "b1"},
		{ConflictKind: model.ConflictKindCrossAgent, AgentA: "a2", AgentB: "b2"},
		{ConflictKind: model.ConflictKindSelfContradiction, AgentA: "a3", AgentB: "a3"},
	}

	filtered, err := authz.FilterConflicts(context.Background(), testDB, claims, conflicts, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 3, "admin should see all conflicts")
}

func TestFilterSearchResults_ReaderSeesOnlyGranted(t *testing.T) {
	suffix := uuid.New().String()[:8]
	reader := createTestAgent(t, "sr-reader-"+suffix, model.RoleReader, nil)
	allowed := createTestAgent(t, "sr-allowed-"+suffix, model.RoleAgent, nil)

	_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    allowed.ID,
		GranteeID:    reader.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &allowed.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	claims := makeClaims(reader.AgentID, reader.ID, model.RoleReader)

	results := []model.SearchResult{
		{Decision: model.Decision{AgentID: reader.AgentID}, SimilarityScore: 0.95},
		{Decision: model.Decision{AgentID: allowed.AgentID}, SimilarityScore: 0.9},
		{Decision: model.Decision{AgentID: "denied-" + suffix}, SimilarityScore: 0.85},
	}

	filtered, err := authz.FilterSearchResults(context.Background(), testDB, claims, results, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 2, "reader should only see own + granted search results")
}

// ---------------------------------------------------------------------------
// Multiple grants: agent can accumulate access to several targets
// ---------------------------------------------------------------------------

func TestLoadGrantedSet_MultipleGrants(t *testing.T) {
	suffix := uuid.New().String()[:8]
	grantee := createTestAgent(t, "multi-grantee-"+suffix, model.RoleAgent, nil)

	targets := make([]model.Agent, 3)
	for i := range targets {
		targets[i] = createTestAgent(t, fmt.Sprintf("multi-target-%d-%s", i, suffix), model.RoleAgent, nil)
		_, err := testDB.CreateGrant(context.Background(), model.AccessGrant{
			OrgID:        uuid.Nil,
			GrantorID:    targets[i].ID,
			GranteeID:    grantee.ID,
			ResourceType: string(model.ResourceAgentTraces),
			ResourceID:   &targets[i].AgentID,
			Permission:   string(model.PermissionRead),
		})
		require.NoError(t, err)
	}

	claims := makeClaims(grantee.AgentID, grantee.ID, model.RoleAgent)
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted)

	// Self + 3 targets = 4.
	assert.Len(t, granted, 4, "should see self + 3 granted targets")
	assert.True(t, granted[grantee.AgentID])
	for _, tgt := range targets {
		assert.True(t, granted[tgt.AgentID], "should include granted target %s", tgt.AgentID)
	}
}

// ---------------------------------------------------------------------------
// Nil claims: deny everything
// ---------------------------------------------------------------------------

func TestFilterDecisions_NilClaimsReturnsEmpty(t *testing.T) {
	decisions := []model.Decision{
		{AgentID: "a"},
		{AgentID: "b"},
	}

	filtered, err := authz.FilterDecisions(context.Background(), testDB, nil, decisions, nil)
	require.NoError(t, err)
	assert.Empty(t, filtered, "nil claims should yield empty filtered results")
}

func TestFilterConflicts_NilClaimsReturnsEmpty(t *testing.T) {
	conflicts := []model.DecisionConflict{
		{ConflictKind: model.ConflictKindCrossAgent, AgentA: "a", AgentB: "b"},
	}

	filtered, err := authz.FilterConflicts(context.Background(), testDB, nil, conflicts, nil)
	require.NoError(t, err)
	assert.Empty(t, filtered, "nil claims should yield empty filtered conflicts")
}

func TestFilterSearchResults_NilClaimsReturnsEmpty(t *testing.T) {
	results := []model.SearchResult{
		{Decision: model.Decision{AgentID: "a"}, SimilarityScore: 0.9},
	}

	filtered, err := authz.FilterSearchResults(context.Background(), testDB, nil, results, nil)
	require.NoError(t, err)
	assert.Empty(t, filtered, "nil claims should yield empty filtered search results")
}

// ---------------------------------------------------------------------------
// Empty input slices: no crash, no allocation surprises
// ---------------------------------------------------------------------------

func TestFilterDecisions_EmptyInput(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "empty-dec-"+suffix, model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	filtered, err := authz.FilterDecisions(context.Background(), testDB, claims, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, filtered)

	filtered, err = authz.FilterDecisions(context.Background(), testDB, claims, []model.Decision{}, nil)
	require.NoError(t, err)
	assert.Empty(t, filtered)
}

// ---------------------------------------------------------------------------
// Self-contradiction conflicts: agent can always see own self-contradictions
// ---------------------------------------------------------------------------

func TestFilterConflicts_SelfContradictionVisible(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "selfconflict-"+suffix, model.RoleAgent, nil)
	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)

	conflicts := []model.DecisionConflict{
		{ConflictKind: model.ConflictKindSelfContradiction, AgentA: agent.AgentID, AgentB: agent.AgentID},
	}

	filtered, err := authz.FilterConflicts(context.Background(), testDB, claims, conflicts, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 1, "agent should see own self-contradictions")
}

// ---------------------------------------------------------------------------
// Cache: grant creation populates correctly on next load
// ---------------------------------------------------------------------------

func TestLoadGrantedSet_CacheUpdatesAfterNewGrant(t *testing.T) {
	suffix := uuid.New().String()[:8]
	grantee := createTestAgent(t, "cache-new-grantee-"+suffix, model.RoleAgent, nil)
	target := createTestAgent(t, "cache-new-target-"+suffix, model.RoleAgent, nil)

	claims := makeClaims(grantee.AgentID, grantee.ID, model.RoleAgent)
	cache := authz.NewGrantCache(5 * time.Second)
	defer cache.Close()

	// Populate cache: only self.
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, cache)
	require.NoError(t, err)
	assert.False(t, granted[target.AgentID], "target should not be visible before grant")

	// Create grant.
	_, err = testDB.CreateGrant(context.Background(), model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    target.ID,
		GranteeID:    grantee.ID,
		ResourceType: string(model.ResourceAgentTraces),
		ResourceID:   &target.AgentID,
		Permission:   string(model.PermissionRead),
	})
	require.NoError(t, err)

	// Invalidate and re-load.
	cacheKey := uuid.Nil.String() + ":" + grantee.ID.String()
	cache.Invalidate(cacheKey)

	granted, err = authz.LoadGrantedSet(context.Background(), testDB, claims, cache)
	require.NoError(t, err)
	assert.True(t, granted[target.AgentID], "target should be visible after grant + cache invalidation")
}

// ---------------------------------------------------------------------------
// Tag-only agents: no grants needed when tags overlap
// ---------------------------------------------------------------------------

func TestLoadGrantedSet_TagOverlapIncludesAllTagMates(t *testing.T) {
	suffix := uuid.New().String()[:8]
	tag := "team-x-" + suffix

	caller := createTestAgent(t, "tagmate-caller-"+suffix, model.RoleAgent, []string{tag})
	peer1 := createTestAgent(t, "tagmate-peer1-"+suffix, model.RoleAgent, []string{tag})
	peer2 := createTestAgent(t, "tagmate-peer2-"+suffix, model.RoleAgent, []string{tag, "other-tag"})
	outsider := createTestAgent(t, "tagmate-outsider-"+suffix, model.RoleAgent, []string{"different-tag"})

	claims := makeClaims(caller.AgentID, caller.ID, model.RoleAgent)
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted)

	assert.True(t, granted[caller.AgentID], "should include self")
	assert.True(t, granted[peer1.AgentID], "should include tag-matched peer1")
	assert.True(t, granted[peer2.AgentID], "should include tag-matched peer2 (shares at least one tag)")
	assert.False(t, granted[outsider.AgentID], "should not include agent with non-overlapping tags")
}

// ---------------------------------------------------------------------------
// Agent with no tags and no grants: isolated to self
// ---------------------------------------------------------------------------

func TestLoadGrantedSet_IsolatedAgent(t *testing.T) {
	suffix := uuid.New().String()[:8]
	agent := createTestAgent(t, "isolated-"+suffix, model.RoleAgent, nil)
	_ = createTestAgent(t, "other-isolated-"+suffix, model.RoleAgent, nil) // exists but not visible

	claims := makeClaims(agent.AgentID, agent.ID, model.RoleAgent)
	granted, err := authz.LoadGrantedSet(context.Background(), testDB, claims, nil)
	require.NoError(t, err)
	require.NotNil(t, granted)
	assert.Len(t, granted, 1, "isolated agent should only see self")
	assert.True(t, granted[agent.AgentID])
}

// ---------------------------------------------------------------------------
// Claims with empty subject: non-admin gets restricted empty set
// ---------------------------------------------------------------------------

func TestLoadGrantedSet_EmptySubject(t *testing.T) {
	c := &auth.Claims{
		AgentID: "empty-sub-agent",
		OrgID:   uuid.Nil,
		Role:    model.RoleAgent,
	}
	c.Subject = ""

	granted, err := authz.LoadGrantedSet(context.Background(), testDB, c, nil)
	require.NoError(t, err)
	assert.NotNil(t, granted, "should return non-nil (restricted)")
	assert.Empty(t, granted, "empty subject should yield empty set")
}
