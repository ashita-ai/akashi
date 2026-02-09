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

// loadGrantedSet returns the set of agent_ids the caller can access.
// For admin+ this returns nil (meaning all access). For others it loads
// granted agent_ids in a single query.
func loadGrantedSet(ctx context.Context, db *storage.DB, claims *auth.Claims) (map[string]bool, error) {
	if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		return nil, nil // nil means unrestricted
	}

	callerUUID, err := uuid.Parse(claims.Subject)
	if err != nil {
		slog.Warn("authz: malformed JWT subject, denying all access",
			"error", err,
			"agent_id", claims.AgentID,
			"role", claims.Role)
		return map[string]bool{}, nil // empty set = no access
	}

	return db.ListGrantedAgentIDs(ctx, claims.OrgID, callerUUID, claims.AgentID)
}

// filterDecisionsByAccess removes decisions the caller is not authorized to see.
// Admin+ sees everything; agent sees own + granted; reader sees only granted.
func filterDecisionsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, decisions []model.Decision) ([]model.Decision, error) {
	granted, err := loadGrantedSet(ctx, db, claims)
	if err != nil {
		return nil, err
	}
	if granted == nil {
		return decisions, nil // admin: unrestricted
	}

	allowed := make([]model.Decision, 0, len(decisions))
	for _, d := range decisions {
		if granted[d.AgentID] {
			allowed = append(allowed, d)
		}
	}
	return allowed, nil
}

// filterSearchResultsByAccess is like filterDecisionsByAccess but for search results.
func filterSearchResultsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, results []model.SearchResult) ([]model.SearchResult, error) {
	granted, err := loadGrantedSet(ctx, db, claims)
	if err != nil {
		return nil, err
	}
	if granted == nil {
		return results, nil
	}

	allowed := make([]model.SearchResult, 0, len(results))
	for _, r := range results {
		if granted[r.Decision.AgentID] {
			allowed = append(allowed, r)
		}
	}
	return allowed, nil
}

// filterConflictsByAccess removes conflicts the caller cannot see.
// A caller must have access to BOTH agents involved in a conflict to see it.
func filterConflictsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, conflicts []model.DecisionConflict) ([]model.DecisionConflict, error) {
	granted, err := loadGrantedSet(ctx, db, claims)
	if err != nil {
		return nil, err
	}
	if granted == nil {
		return conflicts, nil
	}

	allowed := make([]model.DecisionConflict, 0, len(conflicts))
	for _, c := range conflicts {
		if granted[c.AgentA] && granted[c.AgentB] {
			allowed = append(allowed, c)
		}
	}
	return allowed, nil
}
