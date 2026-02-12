package server

import (
	"context"

	"github.com/ashita-ai/akashi/internal/auth"
	"github.com/ashita-ai/akashi/internal/authz"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// canAccessAgent delegates to the shared authz package.
func canAccessAgent(ctx context.Context, db *storage.DB, claims *auth.Claims, targetAgentID string) (bool, error) {
	return authz.CanAccessAgent(ctx, db, claims, targetAgentID)
}

// filterDecisionsByAccess delegates to the shared authz package.
func filterDecisionsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, decisions []model.Decision, cache *authz.GrantCache) ([]model.Decision, error) {
	return authz.FilterDecisions(ctx, db, claims, decisions, cache)
}

// filterSearchResultsByAccess delegates to the shared authz package.
func filterSearchResultsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, results []model.SearchResult, cache *authz.GrantCache) ([]model.SearchResult, error) {
	return authz.FilterSearchResults(ctx, db, claims, results, cache)
}

// filterConflictsByAccess delegates to the shared authz package.
func filterConflictsByAccess(ctx context.Context, db *storage.DB, claims *auth.Claims, conflicts []model.DecisionConflict, cache *authz.GrantCache) ([]model.DecisionConflict, error) {
	return authz.FilterConflicts(ctx, db, claims, conflicts, cache)
}
