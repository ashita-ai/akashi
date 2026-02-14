package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// MutationAuditEntry is an append-only audit event for a state-changing API call.
type MutationAuditEntry struct {
	RequestID    string
	OrgID        uuid.UUID
	ActorAgentID string
	ActorRole    string
	HTTPMethod   string
	Endpoint     string
	Operation    string
	ResourceType string
	ResourceID   string
	BeforeData   any
	AfterData    any
	Metadata     map[string]any
}

// InsertMutationAudit appends a mutation audit event. The target table is immutable.
func (db *DB) InsertMutationAudit(ctx context.Context, e MutationAuditEntry) error {
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}

	var (
		beforeJSON []byte
		afterJSON  []byte
		err        error
	)
	if e.BeforeData != nil {
		beforeJSON, err = json.Marshal(e.BeforeData)
		if err != nil {
			return fmt.Errorf("storage: marshal mutation audit before_data: %w", err)
		}
	}
	if e.AfterData != nil {
		afterJSON, err = json.Marshal(e.AfterData)
		if err != nil {
			return fmt.Errorf("storage: marshal mutation audit after_data: %w", err)
		}
	}
	metaJSON, err := json.Marshal(e.Metadata)
	if err != nil {
		return fmt.Errorf("storage: marshal mutation audit metadata: %w", err)
	}

	_, err = db.pool.Exec(ctx,
		`INSERT INTO mutation_audit_log (
		     request_id, org_id, actor_agent_id, actor_role,
		     http_method, endpoint, operation, resource_type, resource_id,
		     before_data, after_data, metadata
		 )
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12::jsonb)`,
		e.RequestID, e.OrgID, e.ActorAgentID, e.ActorRole,
		e.HTTPMethod, e.Endpoint, e.Operation, e.ResourceType, e.ResourceID,
		beforeJSON, afterJSON, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("storage: insert mutation audit: %w", err)
	}
	return nil
}
