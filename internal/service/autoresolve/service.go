// Package autoresolve implements the background auto-resolution of conflicts
// based on org-configurable policies.
package autoresolve

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Service runs one round of auto-resolution across all configured orgs.
type Service struct {
	db     *storage.DB
	logger *slog.Logger
}

// New creates an auto-resolution service.
func New(db *storage.DB, logger *slog.Logger) *Service {
	return &Service{db: db, logger: logger}
}

// RunOnce scans all orgs with auto-resolution policies and resolves eligible conflicts.
func (s *Service) RunOnce(ctx context.Context) error {
	orgs, err := s.db.GetOrgsWithAutoResolution(ctx)
	if err != nil {
		return fmt.Errorf("autoresolve: list orgs: %w", err)
	}
	if len(orgs) == 0 {
		return nil
	}

	for _, org := range orgs {
		if err := s.processOrg(ctx, org); err != nil {
			s.logger.Warn("autoresolve: org processing failed",
				"org_id", org.OrgID, "error", err)
		}
	}
	return nil
}

func (s *Service) processOrg(ctx context.Context, org storage.OrgAutoResolveConfig) error {
	policy := org.Policy
	neverSeverities := policy.NeverAutoResolveSeverities
	if neverSeverities == nil {
		neverSeverities = []string{}
	}

	eligible, err := s.db.AutoResolvableConflicts(ctx, org.OrgID,
		policy.AutoResolveMaxSeverity, neverSeverities, policy.AutoResolveAfterDays)
	if err != nil {
		return fmt.Errorf("fetch eligible: %w", err)
	}
	if len(eligible) == 0 {
		return nil
	}

	var resolved, skipped int
	for _, c := range eligible {
		if !conflicts.ShouldAutoResolve(c, policy) {
			skipped++
			continue
		}

		winner, note := s.resolveConflict(ctx, c, policy, org.OrgID)

		audit := storage.MutationAuditEntry{
			RequestID:    uuid.New().String(),
			OrgID:        org.OrgID,
			ActorAgentID: "system:auto_resolve",
			ActorRole:    "system",
			HTTPMethod:   "SYSTEM",
			Endpoint:     "auto_resolve_loop",
			Operation:    "conflict_auto_resolved",
			ResourceType: "conflict",
			ResourceID:   c.ID.String(),
			Metadata: map[string]any{
				"tier":                conflicts.ClassifyTier(c).String(),
				"policy_after_days":   policy.AutoResolveAfterDays,
				"policy_winner":       string(policy.AutoResolveWinner),
				"policy_max_severity": policy.AutoResolveMaxSeverity,
			},
		}

		_, err := s.db.UpdateConflictStatusWithAudit(ctx, c.ID, org.OrgID,
			"resolved", "system:auto_resolve", &note, winner, nil, audit)
		if err != nil {
			s.logger.Warn("autoresolve: resolve failed",
				"conflict_id", c.ID, "org_id", org.OrgID, "error", err)
			continue
		}
		resolved++
	}

	if resolved > 0 || skipped > 0 {
		s.logger.Info("autoresolve: org complete",
			"org_id", org.OrgID, "resolved", resolved, "skipped", skipped)
	}
	return nil
}

func (s *Service) resolveConflict(ctx context.Context, c model.DecisionConflict, policy model.ConflictResolutionPolicy, orgID uuid.UUID) (*uuid.UUID, string) {
	// Build recommendation input for winner determination.
	rec := s.buildRecommendation(ctx, c, orgID)
	winner := conflicts.DetermineWinner(c, policy, rec)

	tier := conflicts.ClassifyTier(c)
	var parts []string
	parts = append(parts, fmt.Sprintf("Auto-resolved (%s, review window %dd expired)",
		tier, policy.AutoResolveAfterDays))
	parts = append(parts, fmt.Sprintf("winner selected by %s strategy", policy.AutoResolveWinner))
	if winner != nil {
		side := "A"
		if *winner == c.DecisionBID {
			side = "B"
		}
		parts = append(parts, fmt.Sprintf("Decision %s prevails", side))
	}
	note := strings.Join(parts, "; ")
	return winner, note
}

func (s *Service) buildRecommendation(ctx context.Context, c model.DecisionConflict, orgID uuid.UUID) *model.Recommendation {
	input := conflicts.RecommendationInput{Conflict: c}

	// Fetch win rates for both agents.
	agentIDs := []string{c.AgentA}
	if c.AgentA != c.AgentB {
		agentIDs = append(agentIDs, c.AgentB)
	}
	winRates, err := s.db.GetAgentWinRates(ctx, orgID, agentIDs, c.DecisionType)
	if err == nil {
		if r, ok := winRates[c.AgentA]; ok {
			input.WinRateA = r.WinRate
			input.WinCountA = r.Total
		}
		if r, ok := winRates[c.AgentB]; ok {
			input.WinRateB = r.WinRate
			input.WinCountB = r.Total
		}
	}

	// Fetch revision depths.
	if depth, err := s.db.GetRevisionDepth(ctx, c.DecisionAID, orgID); err == nil {
		input.RevisionDepthA = depth
	}
	if depth, err := s.db.GetRevisionDepth(ctx, c.DecisionBID, orgID); err == nil {
		input.RevisionDepthB = depth
	}

	return conflicts.Recommend(input)
}
