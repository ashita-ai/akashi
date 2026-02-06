package server

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// canAccessAgent checks whether the authenticated caller may read data belonging
// to targetAgentID. The rules are:
//   - admin+: always allowed
//   - agent: allowed for own data (claims.AgentID == targetAgentID), otherwise
//     requires an access grant with resource_type="agent_traces", permission="read"
//   - reader: always requires an explicit access grant
//
// Returns true if access is permitted.
func canAccessAgent(ctx context.Context, db *storage.DB, claims *auth.Claims, targetAgentID string) (bool, error) {
	if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		return true, nil
	}

	if claims.Role == model.RoleAgent && claims.AgentID == targetAgentID {
		return true, nil
	}

	// Need a grant. Parse the caller's UUID from the JWT subject.
	callerUUID, err := uuid.Parse(claims.Subject)
	if err != nil {
		slog.Warn("authz: malformed JWT subject, denying access",
			"error", err,
			"agent_id", claims.AgentID,
			"role", claims.Role)
		return false, nil
	}

	return db.HasAccess(ctx, claims.OrgID, callerUUID, string(model.ResourceAgentTraces), targetAgentID, string(model.PermissionRead))
}

// filterDecisionsByAccess removes decisions the caller is not authorized to see.
// Admin+ sees everything; agent sees own + granted; reader sees only granted.
func filterDecisionsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, decisions []model.Decision) ([]model.Decision, error) {
	if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		return decisions, nil
	}

	// Build a cache of agent access checks to avoid repeated DB queries.
	accessCache := make(map[string]bool)

	var allowed []model.Decision
	for _, d := range decisions {
		ok, cached := accessCache[d.AgentID]
		if !cached {
			var err error
			ok, err = canAccessAgent(ctx, db, claims, d.AgentID)
			if err != nil {
				return nil, err
			}
			accessCache[d.AgentID] = ok
		}
		if ok {
			allowed = append(allowed, d)
		}
	}
	return allowed, nil
}

// filterSearchResultsByAccess is like filterDecisionsByAccess but for search results.
func filterSearchResultsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, results []model.SearchResult) ([]model.SearchResult, error) {
	if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		return results, nil
	}

	accessCache := make(map[string]bool)

	var allowed []model.SearchResult
	for _, r := range results {
		ok, cached := accessCache[r.Decision.AgentID]
		if !cached {
			var err error
			ok, err = canAccessAgent(ctx, db, claims, r.Decision.AgentID)
			if err != nil {
				return nil, err
			}
			accessCache[r.Decision.AgentID] = ok
		}
		if ok {
			allowed = append(allowed, r)
		}
	}
	return allowed, nil
}

// filterConflictsByAccess removes conflicts the caller cannot see.
// A caller must have access to BOTH agents involved in a conflict to see it.
func filterConflictsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, conflicts []model.DecisionConflict) ([]model.DecisionConflict, error) {
	if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		return conflicts, nil
	}

	accessCache := make(map[string]bool)
	checkAccess := func(agentID string) (bool, error) {
		ok, cached := accessCache[agentID]
		if cached {
			return ok, nil
		}
		var err error
		ok, err = canAccessAgent(ctx, db, claims, agentID)
		if err != nil {
			return false, err
		}
		accessCache[agentID] = ok
		return ok, nil
	}

	var allowed []model.DecisionConflict
	for _, c := range conflicts {
		okA, err := checkAccess(c.AgentA)
		if err != nil {
			return nil, err
		}
		okB, err := checkAccess(c.AgentB)
		if err != nil {
			return nil, err
		}
		if okA && okB {
			allowed = append(allowed, c)
		}
	}
	return allowed, nil
}
