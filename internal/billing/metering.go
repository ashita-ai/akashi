package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CurrentPeriod returns the current billing period string (YYYY-MM).
func CurrentPeriod() string {
	return time.Now().UTC().Format("2006-01")
}

// CheckDecisionQuota checks if an org has exceeded its monthly decision limit.
// Returns nil if allowed, ErrQuotaExceeded if the limit is reached,
// or a wrapped error on storage failure.
func (s *Service) CheckDecisionQuota(ctx context.Context, orgID uuid.UUID) error {
	if !s.enabled {
		return nil
	}

	org, err := s.db.GetOrganization(ctx, orgID)
	if err != nil {
		return fmt.Errorf("billing: get org for quota check: %w", err)
	}

	// Unlimited orgs (enterprise or decision_limit=0) skip the check.
	if org.DecisionLimit == 0 {
		return nil
	}

	usage, err := s.db.GetUsage(ctx, orgID, CurrentPeriod())
	if err != nil {
		return fmt.Errorf("billing: get usage for quota check: %w", err)
	}

	if usage.DecisionCount >= org.DecisionLimit {
		return fmt.Errorf("%w: %d/%d decisions this period", ErrQuotaExceeded, usage.DecisionCount, org.DecisionLimit)
	}
	return nil
}

// CheckAgentQuota checks if an org has exceeded its agent limit.
// Returns nil if allowed, ErrAgentLimitExceeded if the limit is reached.
func (s *Service) CheckAgentQuota(ctx context.Context, orgID uuid.UUID) error {
	if !s.enabled {
		return nil
	}

	org, err := s.db.GetOrganization(ctx, orgID)
	if err != nil {
		return fmt.Errorf("billing: get org for agent quota: %w", err)
	}

	// Unlimited orgs skip the check.
	if org.AgentLimit == 0 {
		return nil
	}

	count, err := s.db.CountAgents(ctx, orgID)
	if err != nil {
		return fmt.Errorf("billing: count agents: %w", err)
	}

	if count >= org.AgentLimit {
		return fmt.Errorf("%w: %d/%d agents", ErrAgentLimitExceeded, count, org.AgentLimit)
	}
	return nil
}

// IncrementDecisionCount atomically increments the usage counter after a successful trace.
//
// Deprecated: prefer passing QuotaLimit/BillingPeriod to CreateTraceTx for atomic enforcement.
func (s *Service) IncrementDecisionCount(ctx context.Context, orgID uuid.UUID) error {
	_, err := s.db.IncrementUsage(ctx, orgID, CurrentPeriod())
	return err
}

// DecisionLimit returns the org's decision limit for quota enforcement.
// Returns 0 if billing is disabled or the org has unlimited decisions.
func (s *Service) DecisionLimit(ctx context.Context, orgID uuid.UUID) int {
	if !s.enabled {
		return 0
	}
	org, err := s.db.GetOrganization(ctx, orgID)
	if err != nil {
		return 0 // On error, treat as unlimited (the pre-check already handles errors).
	}
	return org.DecisionLimit
}
