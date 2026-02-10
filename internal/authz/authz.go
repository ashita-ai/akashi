// Package authz provides authorization helpers for filtering data by access grants.
//
// This package exists to share access-control logic between the HTTP server
// and the MCP server without creating a circular dependency (both import this
// package; neither imports the other).
package authz

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// CanAccessAgent checks whether the authenticated caller may read data belonging
// to targetAgentID. The rules are:
//   - admin+: always allowed
//   - agent: allowed for own data (claims.AgentID == targetAgentID)
//   - tag overlap: allowed if caller and target share at least one tag
//   - grant: allowed if an explicit access grant exists
//   - reader: requires tag overlap or explicit access grant
func CanAccessAgent(ctx context.Context, db *storage.DB, claims *auth.Claims, targetAgentID string) (bool, error) {
	if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
		return true, nil
	}

	if claims.AgentID == targetAgentID {
		return true, nil
	}

	callerUUID, err := uuid.Parse(claims.Subject)
	if err != nil {
		slog.Warn("authz: malformed JWT subject, denying access",
			"error", err,
			"agent_id", claims.AgentID,
			"role", claims.Role)
		return false, nil
	}

	// Check tag-based access: if caller has tags, check if target shares any.
	caller, err := db.GetAgentByAgentID(ctx, claims.OrgID, claims.AgentID)
	if err == nil && len(caller.Tags) > 0 {
		target, err := db.GetAgentByAgentID(ctx, claims.OrgID, targetAgentID)
		if err == nil && tagsOverlap(caller.Tags, target.Tags) {
			return true, nil
		}
	}

	// Fall back to grant-based access.
	return db.HasAccess(ctx, claims.OrgID, callerUUID, string(model.ResourceAgentTraces), targetAgentID, string(model.PermissionRead))
}

// tagsOverlap returns true if the two slices share at least one element.
func tagsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, t := range a {
		set[t] = struct{}{}
	}
	for _, t := range b {
		if _, ok := set[t]; ok {
			return true
		}
	}
	return false
}

// LoadGrantedSet returns the set of agent_ids the caller can access.
// For admin+ this returns nil (meaning unrestricted). For others it merges
// tag-based matches (agents sharing at least one tag) with per-agent grants.
func LoadGrantedSet(ctx context.Context, db *storage.DB, claims *auth.Claims) (map[string]bool, error) {
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

	// Start with self.
	granted := map[string]bool{claims.AgentID: true}

	// Tag-based access: find agents sharing tags with caller.
	caller, err := db.GetAgentByAgentID(ctx, claims.OrgID, claims.AgentID)
	if err == nil && len(caller.Tags) > 0 {
		tagMatches, err := db.ListAgentIDsBySharedTags(ctx, claims.OrgID, caller.Tags)
		if err == nil {
			for _, id := range tagMatches {
				granted[id] = true
			}
		}
	}

	// Grant-based access: existing per-agent grants.
	grantMatches, err := db.ListGrantedAgentIDs(ctx, claims.OrgID, callerUUID, claims.AgentID)
	if err != nil {
		return nil, err
	}
	for id := range grantMatches {
		granted[id] = true
	}

	return granted, nil
}

// FilterDecisions removes decisions the caller is not authorized to see.
func FilterDecisions(ctx context.Context, db *storage.DB, claims *auth.Claims, decisions []model.Decision) ([]model.Decision, error) {
	granted, err := LoadGrantedSet(ctx, db, claims)
	if err != nil {
		return nil, err
	}
	if granted == nil {
		return decisions, nil
	}

	allowed := make([]model.Decision, 0, len(decisions))
	for _, d := range decisions {
		if granted[d.AgentID] {
			allowed = append(allowed, d)
		}
	}
	return allowed, nil
}

// FilterSearchResults removes search results the caller is not authorized to see.
func FilterSearchResults(ctx context.Context, db *storage.DB, claims *auth.Claims, results []model.SearchResult) ([]model.SearchResult, error) {
	granted, err := LoadGrantedSet(ctx, db, claims)
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

// FilterConflicts removes conflicts the caller cannot see.
// A caller must have access to BOTH agents involved in a conflict to see it.
func FilterConflicts(ctx context.Context, db *storage.DB, claims *auth.Claims, conflicts []model.DecisionConflict) ([]model.DecisionConflict, error) {
	granted, err := LoadGrantedSet(ctx, db, claims)
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
