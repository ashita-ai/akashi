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
	if agent.Tags == nil {
		agent.Tags = []string{}
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO agents (id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		agent.ID, agent.AgentID, agent.OrgID, agent.Name, string(agent.Role),
		agent.APIKeyHash, agent.Tags, agent.Metadata, agent.CreatedAt, agent.UpdatedAt,
	)
	if err != nil {
		return model.Agent{}, fmt.Errorf("storage: create agent: %w", err)
	}
	return agent, nil
}

// CreateAgentWithAudit inserts a new agent and a mutation audit entry
// atomically within a single transaction.
func (db *DB) CreateAgentWithAudit(ctx context.Context, agent model.Agent, audit MutationAuditEntry) (model.Agent, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.Agent{}, fmt.Errorf("storage: begin create agent tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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
	if agent.Tags == nil {
		agent.Tags = []string{}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO agents (id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		agent.ID, agent.AgentID, agent.OrgID, agent.Name, string(agent.Role),
		agent.APIKeyHash, agent.Tags, agent.Metadata, agent.CreatedAt, agent.UpdatedAt,
	); err != nil {
		return model.Agent{}, fmt.Errorf("storage: create agent: %w", err)
	}

	audit.ResourceID = agent.AgentID
	audit.AfterData = agent
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return model.Agent{}, fmt.Errorf("storage: audit in create agent tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Agent{}, fmt.Errorf("storage: commit create agent tx: %w", err)
	}
	return agent, nil
}

// CreateAgentAndKeyTx inserts a new agent and mints its initial API key
// atomically within a single transaction. The agent's legacy api_key_hash
// column is always set to NULL — credentials live in api_keys only.
// Two audit entries are written: one for the agent and one for the key.
func (db *DB) CreateAgentAndKeyTx(
	ctx context.Context,
	agent model.Agent,
	key model.APIKey,
	agentAudit, keyAudit MutationAuditEntry,
) (model.Agent, model.APIKey, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.Agent{}, model.APIKey{}, fmt.Errorf("storage: begin create agent+key tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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
	if agent.Tags == nil {
		agent.Tags = []string{}
	}
	// Credentials live in api_keys — never write the legacy column for new agents.
	agent.APIKeyHash = nil

	if _, err := tx.Exec(ctx,
		`INSERT INTO agents (id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		agent.ID, agent.AgentID, agent.OrgID, agent.Name, string(agent.Role),
		agent.APIKeyHash, agent.Tags, agent.Metadata, agent.CreatedAt, agent.UpdatedAt,
	); err != nil {
		return model.Agent{}, model.APIKey{}, fmt.Errorf("storage: create agent: %w", err)
	}

	agentAudit.ResourceID = agent.AgentID
	agentAudit.AfterData = agent
	if err := InsertMutationAuditTx(ctx, tx, agentAudit); err != nil {
		return model.Agent{}, model.APIKey{}, fmt.Errorf("storage: audit in create agent+key tx: %w", err)
	}

	if key.ID == uuid.Nil {
		key.ID = uuid.New()
	}
	if key.CreatedAt.IsZero() {
		key.CreatedAt = now
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO api_keys (id, prefix, key_hash, agent_id, org_id, label, created_by, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		key.ID, key.Prefix, key.KeyHash, key.AgentID, key.OrgID,
		key.Label, key.CreatedBy, key.CreatedAt, key.ExpiresAt,
	); err != nil {
		return model.Agent{}, model.APIKey{}, fmt.Errorf("storage: create api key in agent+key tx: %w", err)
	}

	keyAudit.ResourceID = key.ID.String()
	keyAudit.AfterData = key
	if err := InsertMutationAuditTx(ctx, tx, keyAudit); err != nil {
		return model.Agent{}, model.APIKey{}, fmt.Errorf("storage: audit api key in create agent+key tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Agent{}, model.APIKey{}, fmt.Errorf("storage: commit create agent+key tx: %w", err)
	}
	return agent, key, nil
}

// GetAgentsByAgentIDGlobal returns all agents with the given agent_id across all orgs.
// Used ONLY for authentication (token issuance) where org_id isn't known yet.
// Returns all matches so the caller can verify credentials against each one,
// preventing cross-tenant confusion when agent_ids collide across orgs.
func (db *DB) GetAgentsByAgentIDGlobal(ctx context.Context, agentID string) ([]model.Agent, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at, last_seen
		 FROM agents WHERE agent_id = $1 ORDER BY created_at ASC`, agentID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get agents by agent_id: %w", err)
	}
	defer rows.Close()

	var agents []model.Agent
	for rows.Next() {
		var a model.Agent
		if err := rows.Scan(
			&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
			&a.Tags, &a.Metadata, &a.CreatedAt, &a.UpdatedAt, &a.LastSeen,
		); err != nil {
			return nil, fmt.Errorf("storage: scan agent: %w", err)
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: get agents by agent_id: %w", err)
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("storage: agent %s: %w", agentID, ErrNotFound)
	}
	return agents, nil
}

// GetAgentByAgentID retrieves an agent by agent_id within an org.
func (db *DB) GetAgentByAgentID(ctx context.Context, orgID uuid.UUID, agentID string) (model.Agent, error) {
	var a model.Agent
	err := db.pool.QueryRow(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at, last_seen
		 FROM agents WHERE org_id = $1 AND agent_id = $2`, orgID, agentID,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Tags, &a.Metadata, &a.CreatedAt, &a.UpdatedAt, &a.LastSeen,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Agent{}, fmt.Errorf("storage: agent %s: %w", agentID, ErrNotFound)
		}
		return model.Agent{}, fmt.Errorf("storage: get agent: %w", err)
	}
	return a, nil
}

// GetAgentByID retrieves an agent by its internal UUID, scoped to an org for
// defense-in-depth tenant isolation.
func (db *DB) GetAgentByID(ctx context.Context, id uuid.UUID, orgID uuid.UUID) (model.Agent, error) {
	var a model.Agent
	err := db.pool.QueryRow(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at, last_seen
		 FROM agents WHERE id = $1 AND org_id = $2`, id, orgID,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Tags, &a.Metadata, &a.CreatedAt, &a.UpdatedAt, &a.LastSeen,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Agent{}, fmt.Errorf("storage: agent %s: %w", id, ErrNotFound)
		}
		return model.Agent{}, fmt.Errorf("storage: get agent by id: %w", err)
	}
	return a, nil
}

// ListAgents returns agents within an org with pagination.
// limit is clamped to [1, 1000] with a default of 200; offset must be non-negative.
func (db *DB) ListAgents(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]model.Agent, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at, last_seen
		 FROM agents WHERE org_id = $1 ORDER BY created_at ASC LIMIT $2 OFFSET $3`,
		orgID, limit, offset,
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
			&a.Tags, &a.Metadata, &a.CreatedAt, &a.UpdatedAt, &a.LastSeen,
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

// CountAgentsGlobal returns the total number of agents across all organizations.
func (db *DB) CountAgentsGlobal(ctx context.Context) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agents`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: count agents global: %w", err)
	}
	return count, nil
}

// ListAgentIDsBySharedTags returns agent_ids within the org that share at least
// one tag with the provided set (array-overlap). The GIN index on tags makes
// this efficient even for large agent populations.
func (db *DB) ListAgentIDsBySharedTags(ctx context.Context, orgID uuid.UUID, tags []string) ([]string, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT agent_id FROM agents WHERE org_id = $1 AND tags && $2`,
		orgID, tags,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list agents by shared tags: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("storage: scan agent id by tag: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UpdateAgent performs a partial update of an agent's name and/or metadata.
// Only non-nil fields are applied (COALESCE pattern). Returns the updated agent.
func (db *DB) UpdateAgent(ctx context.Context, orgID uuid.UUID, agentID string, name *string, metadata map[string]any) (model.Agent, error) {
	var a model.Agent
	err := db.pool.QueryRow(ctx,
		`UPDATE agents
		 SET name = COALESCE($1, name),
		     metadata = CASE WHEN $2::jsonb IS NOT NULL THEN metadata || $2::jsonb ELSE metadata END,
		     updated_at = now()
		 WHERE org_id = $3 AND agent_id = $4
		 RETURNING id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at`,
		name, metadata, orgID, agentID,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Tags, &a.Metadata, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Agent{}, fmt.Errorf("storage: agent %s: %w", agentID, ErrNotFound)
		}
		return model.Agent{}, fmt.Errorf("storage: update agent: %w", err)
	}
	return a, nil
}

// UpdateAgentWithAudit performs a partial update and inserts a mutation audit
// entry atomically within a single transaction.
func (db *DB) UpdateAgentWithAudit(ctx context.Context, orgID uuid.UUID, agentID string, name *string, metadata map[string]any, audit MutationAuditEntry) (model.Agent, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.Agent{}, fmt.Errorf("storage: begin update agent tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var a model.Agent
	err = tx.QueryRow(ctx,
		`UPDATE agents
		 SET name = COALESCE($1, name),
		     metadata = CASE WHEN $2::jsonb IS NOT NULL THEN metadata || $2::jsonb ELSE metadata END,
		     updated_at = now()
		 WHERE org_id = $3 AND agent_id = $4
		 RETURNING id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at`,
		name, metadata, orgID, agentID,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Tags, &a.Metadata, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Agent{}, fmt.Errorf("storage: agent %s: %w", agentID, ErrNotFound)
		}
		return model.Agent{}, fmt.Errorf("storage: update agent: %w", err)
	}

	audit.ResourceID = agentID
	audit.AfterData = a
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return model.Agent{}, fmt.Errorf("storage: audit in update agent tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Agent{}, fmt.Errorf("storage: commit update agent tx: %w", err)
	}
	return a, nil
}

// AgentStats holds aggregate statistics for a single agent.
type AgentStats struct {
	DecisionCount int            `json:"decision_count"`
	AvgConfidence float64        `json:"avg_confidence"`
	FirstDecision *time.Time     `json:"first_decision,omitempty"`
	LastDecision  *time.Time     `json:"last_decision,omitempty"`
	LowQuality    int            `json:"low_quality_count"` // completeness_score < 0.5
	TypeBreakdown map[string]int `json:"decision_types"`
}

// GetAgentStats returns aggregate decision statistics for a specific agent.
func (db *DB) GetAgentStats(ctx context.Context, orgID uuid.UUID, agentID string) (AgentStats, error) {
	var s AgentStats
	err := db.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(avg(confidence), 0),
		       min(created_at), max(created_at),
		       count(*) FILTER (WHERE completeness_score < 0.5)
		FROM decisions
		WHERE org_id = $1 AND agent_id = $2 AND valid_to IS NULL`,
		orgID, agentID,
	).Scan(&s.DecisionCount, &s.AvgConfidence, &s.FirstDecision, &s.LastDecision, &s.LowQuality)
	if err != nil {
		return s, fmt.Errorf("storage: agent stats: %w", err)
	}

	// Decision type breakdown.
	rows, err := db.pool.Query(ctx, `
		SELECT decision_type, count(*)
		FROM decisions
		WHERE org_id = $1 AND agent_id = $2 AND valid_to IS NULL
		GROUP BY decision_type
		ORDER BY count(*) DESC`,
		orgID, agentID,
	)
	if err != nil {
		return s, fmt.Errorf("storage: agent stats type breakdown: %w", err)
	}
	defer rows.Close()

	s.TypeBreakdown = make(map[string]int)
	for rows.Next() {
		var dt string
		var c int
		if err := rows.Scan(&dt, &c); err != nil {
			return s, fmt.Errorf("storage: scan agent stats type: %w", err)
		}
		s.TypeBreakdown[dt] = c
	}
	return s, rows.Err()
}

// AgentListStat holds per-agent decision counts for list enrichment.
type AgentListStat struct {
	DecisionCount  int        `json:"decision_count"`
	LastDecisionAt *time.Time `json:"last_decision_at,omitempty"`
}

// GetAgentListStats returns decision count and last decision time per agent in an org.
func (db *DB) GetAgentListStats(ctx context.Context, orgID uuid.UUID) (map[string]AgentListStat, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT agent_id, count(*), max(created_at)
		FROM decisions
		WHERE org_id = $1 AND valid_to IS NULL
		GROUP BY agent_id`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: agent list stats: %w", err)
	}
	defer rows.Close()

	result := make(map[string]AgentListStat)
	for rows.Next() {
		var agentID string
		var s AgentListStat
		if err := rows.Scan(&agentID, &s.DecisionCount, &s.LastDecisionAt); err != nil {
			return nil, fmt.Errorf("storage: scan agent list stat: %w", err)
		}
		result[agentID] = s
	}
	return result, rows.Err()
}

// UpdateAgentTags replaces the tags array for an agent. Admin-only operation.
func (db *DB) UpdateAgentTags(ctx context.Context, orgID uuid.UUID, agentID string, tags []string) (model.Agent, error) {
	if tags == nil {
		tags = []string{}
	}

	var a model.Agent
	err := db.pool.QueryRow(ctx,
		`UPDATE agents SET tags = $1, updated_at = now()
		 WHERE org_id = $2 AND agent_id = $3
		 RETURNING id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at, last_seen`,
		tags, orgID, agentID,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Tags, &a.Metadata, &a.CreatedAt, &a.UpdatedAt, &a.LastSeen,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Agent{}, fmt.Errorf("storage: agent %s: %w", agentID, ErrNotFound)
		}
		return model.Agent{}, fmt.Errorf("storage: update agent tags: %w", err)
	}
	return a, nil
}

// UpdateAgentTagsWithAudit replaces tags and inserts a mutation audit entry
// atomically within a single transaction.
func (db *DB) UpdateAgentTagsWithAudit(ctx context.Context, orgID uuid.UUID, agentID string, tags []string, audit MutationAuditEntry) (model.Agent, error) {
	if tags == nil {
		tags = []string{}
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.Agent{}, fmt.Errorf("storage: begin update tags tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var a model.Agent
	err = tx.QueryRow(ctx,
		`UPDATE agents SET tags = $1, updated_at = now()
		 WHERE org_id = $2 AND agent_id = $3
		 RETURNING id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at, last_seen`,
		tags, orgID, agentID,
	).Scan(
		&a.ID, &a.AgentID, &a.OrgID, &a.Name, &a.Role, &a.APIKeyHash,
		&a.Tags, &a.Metadata, &a.CreatedAt, &a.UpdatedAt, &a.LastSeen,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Agent{}, fmt.Errorf("storage: agent %s: %w", agentID, ErrNotFound)
		}
		return model.Agent{}, fmt.Errorf("storage: update agent tags: %w", err)
	}

	audit.ResourceID = agentID
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return model.Agent{}, fmt.Errorf("storage: audit in update tags tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Agent{}, fmt.Errorf("storage: commit update tags tx: %w", err)
	}
	return a, nil
}

// TouchLastSeen updates the last_seen timestamp for an agent to now().
// Called from the auth middleware on every successful authentication.
// Uses a fire-and-forget pattern — callers should not block on the result.
func (db *DB) TouchLastSeen(ctx context.Context, orgID uuid.UUID, agentID string) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE agents SET last_seen = now() WHERE org_id = $1 AND agent_id = $2`,
		orgID, agentID,
	)
	if err != nil {
		return fmt.Errorf("storage: touch last_seen: %w", err)
	}
	return nil
}
