package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateAgent inserts a new agent.
func (db *DB) CreateAgent(ctx context.Context, agent model.Agent) (model.Agent, error) {
	if agent.ID == uuid.Nil {
		agent.ID = uuid.New()
	}
	now := time.Now().UTC()
	if agent.CreatedAt.IsZero() {
		agent.CreatedAt = now
	}
	agent.UpdatedAt = now
	if agent.Metadata == nil {
		agent.Metadata = map[string]any{}
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO agents (id, agent_id, org_id, name, role, api_key_hash, metadata, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		agent.ID, agent.AgentID, agent.OrgID, agent.Name, string(agent.Role),
		agent.APIKeyHash, agent.Metadata, agent.CreatedAt, agent.UpdatedAt,
	)
	if err != nil {
		return model.Agent{}, fmt.Errorf("storage: create agent: %w", err)
	}
	return agent, nil
}

// GetAgentByAgentIDGlobal retrieves an agent by agent_id across all orgs.
// Used ONLY for authentication (token issuance) where org_id isn't known yet.
func (db *DB) GetAgentByAgentIDGlobal(ctx context.Context, agentID string) (model.Agent, error) {
	var a model.Agent
	err := db.pool.QueryRow(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, metadata, created_at, updated_at
		 FROM agents WHERE agent_id = $1`, agentID,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Metadata, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Agent{}, fmt.Errorf("storage: agent not found: %s", agentID)
		}
		return model.Agent{}, fmt.Errorf("storage: get agent: %w", err)
	}
	return a, nil
}

// GetAgentByAgentID retrieves an agent by agent_id within an org.
func (db *DB) GetAgentByAgentID(ctx context.Context, orgID uuid.UUID, agentID string) (model.Agent, error) {
	var a model.Agent
	err := db.pool.QueryRow(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, metadata, created_at, updated_at
		 FROM agents WHERE org_id = $1 AND agent_id = $2`, orgID, agentID,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Metadata, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Agent{}, fmt.Errorf("storage: agent not found: %s", agentID)
		}
		return model.Agent{}, fmt.Errorf("storage: get agent: %w", err)
	}
	return a, nil
}

// GetAgentByID retrieves an agent by its internal UUID.
func (db *DB) GetAgentByID(ctx context.Context, id uuid.UUID) (model.Agent, error) {
	var a model.Agent
	err := db.pool.QueryRow(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, metadata, created_at, updated_at
		 FROM agents WHERE id = $1`, id,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Metadata, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.Agent{}, fmt.Errorf("storage: agent not found: %s", id)
		}
		return model.Agent{}, fmt.Errorf("storage: get agent by id: %w", err)
	}
	return a, nil
}

// ListAgents returns all agents within an org.
func (db *DB) ListAgents(ctx context.Context, orgID uuid.UUID) ([]model.Agent, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, metadata, created_at, updated_at
		 FROM agents WHERE org_id = $1 ORDER BY created_at ASC`, orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list agents: %w", err)
	}
	defer rows.Close()

	var agents []model.Agent
	for rows.Next() {
		var a model.Agent
		if err := rows.Scan(
			&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
			&a.Metadata, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan agent: %w", err)
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// CountAgents returns the number of registered agents in an org.
func (db *DB) CountAgents(ctx context.Context, orgID uuid.UUID) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents WHERE org_id = $1`, orgID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: count agents: %w", err)
	}
	return count, nil
}
