package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// GetAgentByAgentID returns the agent with the given agent_id in the org.
func (l *LiteDB) GetAgentByAgentID(ctx context.Context, orgID uuid.UUID, agentID string) (model.Agent, error) {
	row := l.db.QueryRowContext(ctx,
		`SELECT id, agent_id, org_id, name, role, api_key_hash, tags, metadata,
		        created_at, updated_at, last_seen
		 FROM agents WHERE org_id = ? AND agent_id = ?`,
		uuidStr(orgID), agentID,
	)
	return scanAgent(row)
}

// CreateAgent inserts a new agent.
func (l *LiteDB) CreateAgent(ctx context.Context, agent model.Agent) (model.Agent, error) {
	if agent.ID == uuid.Nil {
		agent.ID = uuid.New()
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO agents (id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuidStr(agent.ID),
		agent.AgentID,
		uuidStr(agent.OrgID),
		agent.Name,
		string(agent.Role),
		agent.APIKeyHash,
		jsonStr(agent.Tags),
		jsonStr(agent.Metadata),
		timeStr(agent.CreatedAt),
		timeStr(agent.UpdatedAt),
	)
	if err != nil {
		return model.Agent{}, fmt.Errorf("sqlite: create agent: %w", err)
	}
	return agent, nil
}

// CreateAgentWithAudit inserts an agent and a mutation audit entry atomically.
func (l *LiteDB) CreateAgentWithAudit(ctx context.Context, agent model.Agent, audit storage.MutationAuditEntry) (model.Agent, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Agent{}, fmt.Errorf("sqlite: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if agent.ID == uuid.Nil {
		agent.ID = uuid.New()
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO agents (id, agent_id, org_id, name, role, api_key_hash, tags, metadata, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuidStr(agent.ID),
		agent.AgentID,
		uuidStr(agent.OrgID),
		agent.Name,
		string(agent.Role),
		agent.APIKeyHash,
		jsonStr(agent.Tags),
		jsonStr(agent.Metadata),
		timeStr(agent.CreatedAt),
		timeStr(agent.UpdatedAt),
	)
	if err != nil {
		return model.Agent{}, fmt.Errorf("sqlite: create agent: %w", err)
	}

	if err := insertAuditTx(ctx, tx, audit); err != nil {
		return model.Agent{}, err
	}

	if err := tx.Commit(); err != nil {
		return model.Agent{}, fmt.Errorf("sqlite: commit: %w", err)
	}
	return agent, nil
}

// CountAgents returns the number of agents in the org.
func (l *LiteDB) CountAgents(ctx context.Context, orgID uuid.UUID) (int, error) {
	var count int
	err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE org_id = ?`,
		uuidStr(orgID),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count agents: %w", err)
	}
	return count, nil
}

// ListAgentIDsBySharedTags returns agent_ids that share at least one tag with the given set.
func (l *LiteDB) ListAgentIDsBySharedTags(ctx context.Context, orgID uuid.UUID, tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("sqlite: marshal tags: %w", err)
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT DISTINCT agent_id FROM agents
		 WHERE org_id = ?
		   AND EXISTS (
		       SELECT 1 FROM json_each(agents.tags) AS at, json_each(?) AS qt
		       WHERE at.value = qt.value
		   )`,
		uuidStr(orgID), string(tagsJSON),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list agents by shared tags: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan agent_id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// scanAgent scans a single agent row.
func scanAgent(row *sql.Row) (model.Agent, error) {
	var (
		a        model.Agent
		id       string
		orgID    string
		role     string
		keyHash  sql.NullString
		tagsJSON sql.NullString
		metaJSON sql.NullString
		created  string
		updated  string
		lastSeen sql.NullString
	)
	err := row.Scan(&id, &a.AgentID, &orgID, &a.Name, &role, &keyHash,
		&tagsJSON, &metaJSON, &created, &updated, &lastSeen)
	if err != nil {
		if err == sql.ErrNoRows {
			return model.Agent{}, storage.ErrNotFound
		}
		return model.Agent{}, fmt.Errorf("sqlite: scan agent: %w", err)
	}
	a.ID = parseUUID(id)
	a.OrgID = parseUUID(orgID)
	a.Role = model.AgentRole(role)
	if keyHash.Valid {
		a.APIKeyHash = &keyHash.String
	}
	a.Tags = []string{}
	if err := scanJSON(tagsJSON, &a.Tags); err != nil {
		return model.Agent{}, fmt.Errorf("sqlite: scan agent tags: %w", err)
	}
	a.Metadata = map[string]any{}
	if err := scanJSON(metaJSON, &a.Metadata); err != nil {
		return model.Agent{}, fmt.Errorf("sqlite: scan agent metadata: %w", err)
	}
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	a.LastSeen = parseNullTime(lastSeen)
	return a, nil
}

// insertAuditTx inserts a mutation audit entry inside an existing transaction.
func insertAuditTx(ctx context.Context, tx *sql.Tx, e storage.MutationAuditEntry) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO mutation_audit_log
		 (request_id, org_id, actor_agent_id, actor_role, http_method, endpoint,
		  operation, resource_type, resource_id, before_data, after_data, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RequestID,
		uuidStr(e.OrgID),
		e.ActorAgentID,
		e.ActorRole,
		e.HTTPMethod,
		e.Endpoint,
		e.Operation,
		e.ResourceType,
		e.ResourceID,
		jsonStr(e.BeforeData),
		jsonStr(e.AfterData),
		jsonStr(e.Metadata),
	)
	if err != nil {
		return fmt.Errorf("sqlite: insert audit: %w", err)
	}
	return nil
}
