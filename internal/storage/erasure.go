package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
)

// EraseDecision performs a GDPR tombstone erasure: scrubs PII fields in-place,
// recomputes the content hash over the scrubbed content, records the original
// hash in decision_erasures, queues a search index deletion, inserts a
// DecisionErased event, and appends a mutation audit entry — all atomically in
// a single transaction.
//
// Erasure is NOT retraction. The row is kept (valid_to is NOT set) so the
// audit chain remains intact. Only the PII-bearing content fields are replaced
// with the [erased] sentinel.
func (db *DB) EraseDecision(ctx context.Context, orgID, decisionID uuid.UUID, reason, erasedBy string, audit *MutationAuditEntry) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin erase decision tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()

	// Fetch the decision to capture original state. We allow erasure of both
	// active (valid_to IS NULL) and retracted decisions — PII must be scrubbed
	// regardless of retraction status.
	var (
		runID       uuid.UUID
		agentID     string
		outcome     string
		reasoning   *string
		contentHash string
		decType     string
		confidence  float32
		validFrom   time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT run_id, agent_id, outcome, reasoning, content_hash, decision_type, confidence, valid_from
		 FROM decisions WHERE id = $1 AND org_id = $2`,
		decisionID, orgID,
	).Scan(&runID, &agentID, &outcome, &reasoning, &contentHash, &decType, &confidence, &validFrom)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("storage: decision %s: %w", decisionID, ErrNotFound)
		}
		return fmt.Errorf("storage: fetch decision for erasure: %w", err)
	}

	// Guard: don't erase an already-erased decision.
	if outcome == model.ErasedPlaceholder {
		return fmt.Errorf("storage: decision %s: %w", decisionID, ErrAlreadyErased)
	}

	// Preserve original hash. If the decision predates content hashing,
	// compute one now so the erasure record always has a hash to reference.
	originalHash := contentHash
	if originalHash == "" {
		originalHash = integrity.ComputeContentHash(decisionID, decType, outcome, confidence, reasoning, validFrom)
	}

	// Scrub PII fields on the decision row.
	erasedReasoning := model.ErasedPlaceholder
	newHash := integrity.ComputeContentHash(decisionID, decType, model.ErasedPlaceholder, confidence, &erasedReasoning, validFrom)

	_, err = tx.Exec(ctx,
		`UPDATE decisions
		 SET outcome = $1, reasoning = $2, content_hash = $3, embedding = NULL, outcome_embedding = NULL
		 WHERE id = $4 AND org_id = $5`,
		model.ErasedPlaceholder, erasedReasoning, newHash, decisionID, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: scrub decision fields: %w", err)
	}

	// Scrub alternatives content.
	_, err = tx.Exec(ctx,
		`UPDATE alternatives
		 SET label = $1, rejection_reason = $2
		 WHERE decision_id = $3
		   AND decision_id IN (SELECT id FROM decisions WHERE org_id = $4)`,
		model.ErasedPlaceholder, nil, decisionID, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: scrub alternatives: %w", err)
	}

	// Scrub evidence content.
	_, err = tx.Exec(ctx,
		`UPDATE evidence
		 SET content = $1, source_uri = NULL, embedding = NULL
		 WHERE decision_id = $2 AND org_id = $3`,
		model.ErasedPlaceholder, decisionID, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: scrub evidence: %w", err)
	}

	// Insert decision_erasures record with original hash.
	_, err = tx.Exec(ctx,
		`INSERT INTO decision_erasures (org_id, decision_id, erased_by, erased_at, original_hash, reason)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		orgID, decisionID, erasedBy, now, originalHash, reason,
	)
	if err != nil {
		return fmt.Errorf("storage: insert decision erasure: %w", err)
	}

	// Queue search index deletion so Qdrant removes the vector.
	if _, err := tx.Exec(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation)
		 VALUES ($1, $2, 'delete')
		 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
		decisionID, orgID); err != nil {
		return fmt.Errorf("storage: queue search outbox delete in erasure: %w", err)
	}

	// Insert DecisionErased event.
	payload := map[string]any{
		"decision_id": decisionID.String(),
		"erased_by":   erasedBy,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	var seqNum int64
	err = tx.QueryRow(ctx, `SELECT nextval('event_sequence_num_seq')`).Scan(&seqNum)
	if err != nil {
		return fmt.Errorf("storage: reserve sequence num for erasure event: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO agent_events (id, run_id, org_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		uuid.New(), runID, orgID, string(model.EventDecisionErased), seqNum,
		now, agentID, payload, now,
	)
	if err != nil {
		return fmt.Errorf("storage: insert erasure event: %w", err)
	}

	// Insert mutation audit entry.
	if audit != nil {
		audit.Operation = "decision_erased"
		audit.ResourceType = "decision"
		audit.ResourceID = decisionID.String()
		audit.BeforeData = map[string]any{
			"outcome":      outcome,
			"reasoning":    reasoning,
			"content_hash": contentHash,
		}
		audit.AfterData = map[string]any{
			"outcome":       model.ErasedPlaceholder,
			"reasoning":     erasedReasoning,
			"content_hash":  newHash,
			"original_hash": originalHash,
			"erased_by":     erasedBy,
			"reason":        reason,
		}
		if err := InsertMutationAuditTx(ctx, tx, *audit); err != nil {
			return fmt.Errorf("storage: audit in erasure tx: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: commit erasure: %w", err)
	}
	return nil
}

// GetDecisionErasure returns the erasure record for a decision, or ErrNotFound
// if the decision has not been erased.
func (db *DB) GetDecisionErasure(ctx context.Context, orgID, decisionID uuid.UUID) (model.DecisionErasure, error) {
	var e model.DecisionErasure
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, decision_id, erased_by, erased_at, original_hash, reason
		 FROM decision_erasures
		 WHERE decision_id = $1 AND org_id = $2`,
		decisionID, orgID,
	).Scan(&e.ID, &e.OrgID, &e.DecisionID, &e.ErasedBy, &e.ErasedAt, &e.OriginalHash, &e.Reason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.DecisionErasure{}, ErrNotFound
		}
		return model.DecisionErasure{}, fmt.Errorf("storage: get decision erasure: %w", err)
	}
	return e, nil
}
