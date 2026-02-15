package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// pgxExecer is the subset of pgx.Tx / pgxpool.Pool used for INSERT execution.
// Both *pgxpool.Pool and pgx.Tx satisfy this interface.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// insertMutationAudit is the shared implementation for both InsertMutationAudit
// and InsertMutationAuditTx. It marshals JSON fields and executes the INSERT
// against the provided executor (pool or transaction).
func insertMutationAudit(ctx context.Context, exec pgxExecer, e MutationAuditEntry) error {
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

	_, err = exec.Exec(ctx,
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

// InsertMutationAudit appends a mutation audit event using the connection pool.
// Use InsertMutationAuditTx when the audit must be atomic with a mutation.
func (db *DB) InsertMutationAudit(ctx context.Context, e MutationAuditEntry) error {
	return insertMutationAudit(ctx, db.pool, e)
}

// InsertMutationAuditTx appends a mutation audit event within an existing
// transaction. If the transaction rolls back, the audit entry is also rolled
// back — ensuring mutations never persist without their audit record.
func InsertMutationAuditTx(ctx context.Context, tx pgx.Tx, e MutationAuditEntry) error {
	return insertMutationAudit(ctx, tx, e)
}
