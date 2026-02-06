package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateOrganization inserts a new organization.
func (db *DB) CreateOrganization(ctx context.Context, org model.Organization) (model.Organization, error) {
	if org.ID == uuid.Nil {
		org.ID = uuid.New()
	}
	now := time.Now().UTC()
	if org.CreatedAt.IsZero() {
		org.CreatedAt = now
	}
	org.UpdatedAt = now

	_, err := db.pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug, plan, stripe_customer_id, stripe_subscription_id,
		 decision_limit, agent_limit, email, email_verified, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		org.ID, org.Name, org.Slug, org.Plan, org.StripeCustomerID, org.StripeSubscriptionID,
		org.DecisionLimit, org.AgentLimit, org.Email, org.EmailVerified, org.CreatedAt, org.UpdatedAt,
	)
	if err != nil {
		return model.Organization{}, fmt.Errorf("storage: create organization: %w", err)
	}
	return org, nil
}

// GetOrganization retrieves an org by ID.
func (db *DB) GetOrganization(ctx context.Context, id uuid.UUID) (model.Organization, error) {
	var org model.Organization
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, slug, plan, stripe_customer_id, stripe_subscription_id,
		 decision_limit, agent_limit, email, email_verified, created_at, updated_at
		 FROM organizations WHERE id = $1`, id,
	).Scan(
		&org.ID, &org.Name, &org.Slug, &org.Plan, &org.StripeCustomerID, &org.StripeSubscriptionID,
		&org.DecisionLimit, &org.AgentLimit, &org.Email, &org.EmailVerified, &org.CreatedAt, &org.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Organization{}, fmt.Errorf("storage: organization not found: %s", id)
		}
		return model.Organization{}, fmt.Errorf("storage: get organization: %w", err)
	}
	return org, nil
}

// GetOrganizationBySlug retrieves an org by slug.
func (db *DB) GetOrganizationBySlug(ctx context.Context, slug string) (model.Organization, error) {
	var org model.Organization
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, slug, plan, stripe_customer_id, stripe_subscription_id,
		 decision_limit, agent_limit, email, email_verified, created_at, updated_at
		 FROM organizations WHERE slug = $1`, slug,
	).Scan(
		&org.ID, &org.Name, &org.Slug, &org.Plan, &org.StripeCustomerID, &org.StripeSubscriptionID,
		&org.DecisionLimit, &org.AgentLimit, &org.Email, &org.EmailVerified, &org.CreatedAt, &org.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Organization{}, fmt.Errorf("storage: organization not found: %s", slug)
		}
		return model.Organization{}, fmt.Errorf("storage: get organization by slug: %w", err)
	}
	return org, nil
}

// UpdateOrganization updates org fields.
func (db *DB) UpdateOrganization(ctx context.Context, org model.Organization) error {
	org.UpdatedAt = time.Now().UTC()
	tag, err := db.pool.Exec(ctx,
		`UPDATE organizations SET name = $1, slug = $2, plan = $3, stripe_customer_id = $4,
		 stripe_subscription_id = $5, decision_limit = $6, agent_limit = $7, email = $8,
		 email_verified = $9, updated_at = $10 WHERE id = $11`,
		org.Name, org.Slug, org.Plan, org.StripeCustomerID, org.StripeSubscriptionID,
		org.DecisionLimit, org.AgentLimit, org.Email, org.EmailVerified, org.UpdatedAt, org.ID,
	)
	if err != nil {
		return fmt.Errorf("storage: update organization: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: organization not found: %s", org.ID)
	}
	return nil
}

// IncrementUsage atomically increments the decision count for an org's current period.
// Returns the new count.
func (db *DB) IncrementUsage(ctx context.Context, orgID uuid.UUID, period string) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`INSERT INTO org_usage (org_id, period, decision_count)
		 VALUES ($1, $2, 1)
		 ON CONFLICT (org_id, period) DO UPDATE SET decision_count = org_usage.decision_count + 1
		 RETURNING decision_count`,
		orgID, period,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: increment usage: %w", err)
	}
	return count, nil
}

// GetUsage returns the current period's usage for an org.
func (db *DB) GetUsage(ctx context.Context, orgID uuid.UUID, period string) (model.OrgUsage, error) {
	var usage model.OrgUsage
	err := db.pool.QueryRow(ctx,
		`SELECT org_id, period, decision_count FROM org_usage WHERE org_id = $1 AND period = $2`,
		orgID, period,
	).Scan(&usage.OrgID, &usage.Period, &usage.DecisionCount)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.OrgUsage{OrgID: orgID, Period: period, DecisionCount: 0}, nil
		}
		return model.OrgUsage{}, fmt.Errorf("storage: get usage: %w", err)
	}
	return usage, nil
}

// GetOrganizationByStripeCustomer retrieves an org by its Stripe customer ID.
func (db *DB) GetOrganizationByStripeCustomer(ctx context.Context, customerID string) (model.Organization, error) {
	var org model.Organization
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, slug, plan, stripe_customer_id, stripe_subscription_id,
		 decision_limit, agent_limit, email, email_verified, created_at, updated_at
		 FROM organizations WHERE stripe_customer_id = $1`, customerID,
	).Scan(
		&org.ID, &org.Name, &org.Slug, &org.Plan, &org.StripeCustomerID, &org.StripeSubscriptionID,
		&org.DecisionLimit, &org.AgentLimit, &org.Email, &org.EmailVerified, &org.CreatedAt, &org.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Organization{}, fmt.Errorf("storage: organization not found for customer: %s", customerID)
		}
		return model.Organization{}, fmt.Errorf("storage: get organization by stripe customer: %w", err)
	}
	return org, nil
}

// CreateEmailVerification inserts a verification token for an org.
func (db *DB) CreateEmailVerification(ctx context.Context, orgID uuid.UUID, token string, expiresAt time.Time) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO email_verifications (org_id, token, expires_at) VALUES ($1, $2, $3)`,
		orgID, token, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("storage: create email verification: %w", err)
	}
	return nil
}

// VerifyEmail marks a verification token as used and sets the org's email as verified.
// Returns an error if the token is invalid, expired, or already used.
func (db *DB) VerifyEmail(ctx context.Context, token string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin verify tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID uuid.UUID
	var expiresAt time.Time
	var usedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT org_id, expires_at, used_at FROM email_verifications WHERE token = $1`,
		token,
	).Scan(&orgID, &expiresAt, &usedAt)
	if err != nil {
		return fmt.Errorf("storage: verification token not found")
	}

	if usedAt != nil {
		return fmt.Errorf("storage: verification token already used")
	}
	if time.Now().After(expiresAt) {
		return fmt.Errorf("storage: verification token expired")
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx,
		`UPDATE email_verifications SET used_at = $1 WHERE token = $2`,
		now, token,
	); err != nil {
		return fmt.Errorf("storage: mark verification used: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE organizations SET email_verified = true, updated_at = $1 WHERE id = $2`,
		now, orgID,
	); err != nil {
		return fmt.Errorf("storage: verify org email: %w", err)
	}

	return tx.Commit(ctx)
}
