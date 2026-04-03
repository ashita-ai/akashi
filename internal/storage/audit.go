//go:build !lite

package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Design note: mutation_audit_log and deletion_audit_log intentionally lack a
// foreign key to organizations. Audit records MUST survive org deletion — they
// are the record OF the deletion. Adding ON DELETE CASCADE would silently
// destroy the audit trail when an org is removed.

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

// InsertMutationAuditBatch inserts multiple audit entries in a single
// transaction. Either all entries are committed or none are — preventing
// duplicate rows on partial-failure retry.
func (db *DB) InsertMutationAuditBatch(ctx context.Context, entries []MutationAuditEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) == 1 {
		return db.InsertMutationAudit(ctx, entries[0])
	}
	return db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, e := range entries {
			if err := InsertMutationAuditTx(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}
