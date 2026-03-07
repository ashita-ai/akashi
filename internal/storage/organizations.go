package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// EnsureDefaultOrg idempotently creates the default organization (uuid.Nil).
// Used by SeedAdmin to guarantee the FK target exists on a fresh database
// before inserting the admin agent.
func (db *DB) EnsureDefaultOrg(ctx context.Context) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug, plan, created_at, updated_at)
		 VALUES ($1, 'Default', 'default', 'oss', NOW(), NOW())
		 ON CONFLICT (id) DO NOTHING`,
		uuid.Nil,
	)
	if err != nil {
		return fmt.Errorf("storage: ensure default org: %w", err)
	}
	return nil
}

// GetOrganization retrieves an org by ID.
func (db *DB) GetOrganization(ctx context.Context, id uuid.UUID) (model.Organization, error) {
	var org model.Organization
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, slug, plan, created_at, updated_at
		 FROM organizations WHERE id = $1`, id,
	).Scan(
		&org.ID, &org.Name, &org.Slug, &org.Plan, &org.CreatedAt, &org.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Organization{}, fmt.Errorf("storage: organization not found: %s", id)
		}
		return model.Organization{}, fmt.Errorf("storage: get organization: %w", err)
	}
	return org, nil
}

// CreateOrgWithOwnerAndKeyTx atomically creates an organization, its owner
// agent (org_owner role), and an initial managed API key. Three audit entries
// are written: one each for the org, agent, and key. If any step fails the
// entire transaction is rolled back and nothing is persisted.
func (db *DB) CreateOrgWithOwnerAndKeyTx(
	ctx context.Context,
	org model.Organization,
	agent model.Agent,
	key model.APIKey,
	orgAudit, agentAudit, keyAudit MutationAuditEntry,
) (model.Organization, model.Agent, model.APIKey, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.Organization{}, model.Agent{}, model.APIKey{},
			fmt.Errorf("storage: begin signup tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()

	// --- 1. Create organization ---
	if org.ID == uuid.Nil {
		org.ID = uuid.New()
	}
	org.CreatedAt = now
	org.UpdatedAt = now

	if _, err := tx.Exec(ctx,
		`INSERT INTO organizations (id, name, slug, plan, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		org.ID, org.Name, org.Slug, org.Plan, org.CreatedAt, org.UpdatedAt,
	); err != nil {
		return model.Organization{}, model.Agent{}, model.APIKey{},
			fmt.Errorf("storage: create org in signup tx: %w", err)
	}

	orgAudit.ResourceID = org.ID.String()
	orgAudit.AfterData = org
	if err := InsertMutationAuditTx(ctx, tx, orgAudit); err != nil {
		return model.Organization{}, model.Agent{}, model.APIKey{},
			fmt.Errorf("storage: audit org in signup tx: %w", err)
	}

	// --- 2. Create owner agent ---
	if agent.ID == uuid.Nil {
		agent.ID = uuid.New()
	}
	agent.OrgID = org.ID
	agent.CreatedAt = now
	agent.UpdatedAt = now
	agent.APIKeyHash = nil // credentials live in api_keys only
	if agent.Metadata == nil {
		agent.Metadata = map[string]any{}
	}
	if agent.Tags == nil {
		agent.Tags = []string{}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO agents (id, agent_id, org_id, name, role, api_key_hash, email, tags, metadata, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		agent.ID, agent.AgentID, agent.OrgID, agent.Name, string(agent.Role),
		agent.APIKeyHash, agent.Email, agent.Tags, agent.Metadata, agent.CreatedAt, agent.UpdatedAt,
	); err != nil {
		return model.Organization{}, model.Agent{}, model.APIKey{},
			fmt.Errorf("storage: create agent in signup tx: %w", err)
	}

	agentAudit.ResourceID = agent.AgentID
	agentAudit.AfterData = agent
	if err := InsertMutationAuditTx(ctx, tx, agentAudit); err != nil {
		return model.Organization{}, model.Agent{}, model.APIKey{},
			fmt.Errorf("storage: audit agent in signup tx: %w", err)
	}

	// --- 3. Create API key ---
	if key.ID == uuid.Nil {
		key.ID = uuid.New()
	}
	key.OrgID = org.ID
	key.CreatedAt = now

	if _, err := tx.Exec(ctx,
		`INSERT INTO api_keys (id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		key.ID, key.Prefix, key.KeyHash, key.AgentID, key.OrgID,
		key.Label, key.CreatedBy, key.CreatedAt, key.ExpiresAt,
	); err != nil {
		return model.Organization{}, model.Agent{}, model.APIKey{},
			fmt.Errorf("storage: create api key in signup tx: %w", err)
	}

	keyAudit.ResourceID = key.ID.String()
	keyAudit.AfterData = key
	if err := InsertMutationAuditTx(ctx, tx, keyAudit); err != nil {
		return model.Organization{}, model.Agent{}, model.APIKey{},
			fmt.Errorf("storage: audit api key in signup tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Organization{}, model.Agent{}, model.APIKey{},
			fmt.Errorf("storage: commit signup tx: %w", err)
	}
	return org, agent, key, nil
}
