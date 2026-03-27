//go:build !lite

package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
)

// decisionCols is the SELECT column list for the standard 25-column decision query.
// Every function that scans into model.Decision via scanOneDecision must SELECT
// exactly these columns in this order.
const decisionCols = `id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
	metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
	valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, project`

// pgxRowScanner is satisfied by both pgx.Row (single-row) and pgx.Rows (multi-row).
type pgxRowScanner interface {
	Scan(dest ...any) error
}

// scanOneDecision scans the 25-column decisionCols from a single row.
func scanOneDecision(row pgxRowScanner) (model.Decision, error) {
	var d model.Decision
	if err := row.Scan(
		&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence,
		&d.Reasoning, &d.Metadata, &d.CompletenessScore, &d.OutcomeScore, &d.PrecedentRef,
		&d.PrecedentReason, &d.SupersedesID, &d.ContentHash,
		&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
		&d.SessionID, &d.AgentContext, &d.APIKeyID,
		&d.Tool, &d.Model, &d.Project,
	); err != nil {
		return model.Decision{}, fmt.Errorf("storage: scan decision: %w", err)
	}
	return d, nil
}

// txExecer is satisfied by both pgx.Tx and *pgxpool.Pool.
type txExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// queueSearchOutbox inserts or resets a search outbox entry for a decision.
// operation must be "upsert" or "delete".
func queueSearchOutbox(ctx context.Context, exec txExecer, decisionID, orgID uuid.UUID, operation string) error {
	_, err := exec.Exec(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
		decisionID, orgID, operation)
	return err
}

// CreateDecision inserts a decision and queues a search outbox entry if the
// decision has an embedding. Both writes happen atomically in a single transaction.
//
// WARNING: This function does NOT insert a mutation audit entry. It exists for
// test/eval use only (conflicts/eval.go). Production traces MUST use
// CreateTraceTx, which atomically records the audit trail.
// Do not call this from handler or service code.
func (db *DB) CreateDecision(ctx context.Context, d model.Decision) (model.Decision, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	now := time.Now().UTC()
	if d.ValidFrom.IsZero() {
		d.ValidFrom = now
	}
	if d.TransactionTime.IsZero() {
		d.TransactionTime = now
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	if d.Metadata == nil {
		d.Metadata = map[string]any{}
	}
	if d.AgentContext == nil {
		d.AgentContext = map[string]any{}
	}

	d.ContentHash = integrity.ComputeContentHash(d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)

	err := db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome, confidence,
			 reasoning, embedding, outcome_embedding, metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
			 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`,
			d.ID, d.RunID, d.AgentID, d.OrgID, d.DecisionType, d.Outcome, d.Confidence,
			d.Reasoning, d.Embedding, d.OutcomeEmbedding, d.Metadata, d.CompletenessScore, d.OutcomeScore, d.PrecedentRef,
			d.PrecedentReason, d.SupersedesID, d.ContentHash,
			d.ValidFrom, d.ValidTo, d.TransactionTime, d.CreatedAt,
			d.SessionID, d.AgentContext, d.APIKeyID,
		)
		if err != nil {
			return fmt.Errorf("storage: create decision: %w", err)
		}

		// Queue search index update inside the same transaction.
		// Always queue regardless of embedding status — the outbox worker will
		// generate embeddings asynchronously if needed. Without this, decisions
		// created without an embedding (e.g. via the REST API) are permanently
		// invisible to search.
		if err := queueSearchOutbox(ctx, tx, d.ID, d.OrgID, "upsert"); err != nil {
			return fmt.Errorf("storage: queue search outbox in create decision: %w", err)
		}
		return nil
	})
	if err != nil {
		return model.Decision{}, err
	}
	return d, nil
}

// GetDecisionOpts controls GetDecision behavior.
type GetDecisionOpts struct {
	IncludeAlts     bool // Load alternatives.
	IncludeEvidence bool // Load evidence.
	CurrentOnly     bool // If true, return only if the decision has not been superseded (valid_to IS NULL).
}

// GetDecision retrieves a decision by ID with configurable includes and filtering.
func (db *DB) GetDecision(ctx context.Context, orgID, id uuid.UUID, opts GetDecisionOpts) (model.Decision, error) {
	query := `SELECT ` + decisionCols + ` FROM decisions WHERE id = $1 AND org_id = $2`
	if opts.CurrentOnly {
		query += ` AND valid_to IS NULL`
	}

	d, err := scanOneDecision(db.pool.QueryRow(ctx, query, id, orgID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Decision{}, fmt.Errorf("storage: decision %s: %w", id, ErrNotFound)
		}
		return model.Decision{}, fmt.Errorf("storage: get decision: %w", err)
	}

	if opts.IncludeAlts {
		alts, err := db.GetAlternativesByDecision(ctx, id, orgID)
		if err != nil {
			return model.Decision{}, err
		}
		d.Alternatives = alts
	}

	if opts.IncludeEvidence {
		ev, err := db.GetEvidenceByDecision(ctx, id, orgID)
		if err != nil {
			return model.Decision{}, err
		}
		d.Evidence = ev
	}

	return d, nil
}

// ReviseDecision invalidates an existing decision by setting valid_to
// and creates a new decision with the revised data. When audit is non-nil,
// a mutation audit entry recording the revision is inserted in the same transaction.
func (db *DB) ReviseDecision(ctx context.Context, originalID uuid.UUID, revised model.Decision, audit *MutationAuditEntry) (model.Decision, error) {
	now := time.Now().UTC()

	// Prepare revised decision fields before entering the transaction.
	revised.ID = uuid.New()
	revised.ValidFrom = now
	revised.TransactionTime = now
	revised.CreatedAt = now
	revised.SupersedesID = &originalID
	if revised.Metadata == nil {
		revised.Metadata = map[string]any{}
	}
	revised.ContentHash = integrity.ComputeContentHash(revised.ID, revised.DecisionType, revised.Outcome, revised.Confidence, revised.Reasoning, revised.ValidFrom)
	if revised.AgentContext == nil {
		revised.AgentContext = map[string]any{}
	}

	err := db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Invalidate original decision, scoped by org_id for tenant isolation.
		tag, err := tx.Exec(ctx,
			`UPDATE decisions SET valid_to = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
			now, originalID, revised.OrgID,
		)
		if err != nil {
			return fmt.Errorf("storage: invalidate decision: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("storage: original decision %s (or already revised): %w", originalID, ErrNotFound)
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome, confidence,
			 reasoning, embedding, outcome_embedding, metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
			 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`,
			revised.ID, revised.RunID, revised.AgentID, revised.OrgID, revised.DecisionType, revised.Outcome,
			revised.Confidence, revised.Reasoning, revised.Embedding, revised.OutcomeEmbedding, revised.Metadata,
			revised.CompletenessScore, revised.OutcomeScore, revised.PrecedentRef, revised.PrecedentReason, revised.SupersedesID, revised.ContentHash,
			revised.ValidFrom, revised.ValidTo, revised.TransactionTime, revised.CreatedAt,
			revised.SessionID, revised.AgentContext, revised.APIKeyID,
		)
		if err != nil {
			return fmt.Errorf("storage: insert revised decision: %w", err)
		}

		// Queue search index updates: delete the old decision, upsert the new one.
		if err := queueSearchOutbox(ctx, tx, originalID, revised.OrgID, "delete"); err != nil {
			return fmt.Errorf("storage: queue search outbox delete in revision: %w", err)
		}
		if err := queueSearchOutbox(ctx, tx, revised.ID, revised.OrgID, "upsert"); err != nil {
			return fmt.Errorf("storage: queue search outbox upsert in revision: %w", err)
		}

		// Auto-resolve open conflicts involving the superseded decision. The revised
		// decision replaces the old one, so stale conflicts should not persist.
		// If the revision still conflicts, the scorer will create a new conflict.
		autoResolved, err := AutoResolveSupersededConflictsTx(ctx, tx, revised.OrgID, originalID, revised.ID)
		if err != nil {
			return fmt.Errorf("storage: auto-resolve in revision tx: %w", err)
		}

		// Insert revision audit entry (same tx — atomic with the revision).
		if audit != nil {
			audit.Operation = "decision_revised"
			audit.ResourceType = "decision"
			audit.ResourceID = originalID.String()
			audit.BeforeData = map[string]any{"valid_to": nil}
			audit.AfterData = map[string]any{
				"superseded_by":           revised.ID.String(),
				"valid_to":                now,
				"conflicts_auto_resolved": autoResolved,
			}
			if err := InsertMutationAuditTx(ctx, tx, *audit); err != nil {
				return fmt.Errorf("storage: audit in revision tx: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return model.Decision{}, err
	}
	return revised, nil
}

// RetractDecision soft-deletes a decision by setting valid_to, recording a
// DecisionRetracted event, queuing a search index deletion, and inserting a
// mutation audit entry — all atomically in a single transaction.
func (db *DB) RetractDecision(ctx context.Context, orgID, decisionID uuid.UUID, reason, retractedBy string, audit *MutationAuditEntry) error {
	return db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := time.Now().UTC()

		// Fetch the decision's run_id and agent_id for the retraction event.
		var runID uuid.UUID
		var agentID string
		err := tx.QueryRow(ctx,
			`SELECT run_id, agent_id FROM decisions WHERE id = $1 AND org_id = $2 AND valid_to IS NULL`,
			decisionID, orgID,
		).Scan(&runID, &agentID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("storage: decision %s: %w", decisionID, ErrNotFound)
			}
			return fmt.Errorf("storage: fetch decision for retraction: %w", err)
		}

		// Soft-delete: set valid_to on the decision.
		_, err = tx.Exec(ctx,
			`UPDATE decisions SET valid_to = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
			now, decisionID, orgID,
		)
		if err != nil {
			return fmt.Errorf("storage: retract decision: %w", err)
		}

		// Queue search index deletion.
		if err := queueSearchOutbox(ctx, tx, decisionID, orgID, "delete"); err != nil {
			return fmt.Errorf("storage: queue search outbox delete in retraction: %w", err)
		}

		// Insert DecisionRetracted event.
		payload := map[string]any{
			"decision_id":  decisionID.String(),
			"retracted_by": retractedBy,
		}
		if reason != "" {
			payload["reason"] = reason
		}
		var seqNum int64
		err = tx.QueryRow(ctx, `SELECT nextval('event_sequence_num_seq')`).Scan(&seqNum)
		if err != nil {
			return fmt.Errorf("storage: reserve sequence num for retraction event: %w", err)
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO agent_events (id, run_id, org_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			uuid.New(), runID, orgID, string(model.EventDecisionRetracted), seqNum,
			now, agentID, payload, now,
		)
		if err != nil {
			return fmt.Errorf("storage: insert retraction event: %w", err)
		}

		// Insert mutation audit entry.
		if audit != nil {
			audit.Operation = "decision_retracted"
			audit.ResourceType = "decision"
			audit.ResourceID = decisionID.String()
			audit.BeforeData = map[string]any{"valid_to": nil}
			audit.AfterData = map[string]any{
				"valid_to":     now,
				"retracted_by": retractedBy,
				"reason":       reason,
			}
			if err := InsertMutationAuditTx(ctx, tx, *audit); err != nil {
				return fmt.Errorf("storage: audit in retraction tx: %w", err)
			}
		}
		return nil
	})
}

// ErasedSentinel is the placeholder text that replaces PII fields during GDPR erasure.
const ErasedSentinel = "[erased]"

// DecisionErasureResult contains the erasure record and affected row counts.
type DecisionErasureResult struct {
	Erasure            model.DecisionErasure
	AlternativesErased int64
	EvidenceErased     int64
	ClaimsErased       int64
}

// EraseDecision scrubs PII from a decision in-place (GDPR Art. 17 tombstone erasure).
// Within a single transaction it:
//  1. Activates the immutability trigger bypass via SET LOCAL
//  2. Scrubs outcome/reasoning to "[erased]" and recomputes the content hash
//  3. Scrubs alternatives (label, rejection_reason) and evidence (content, source_uri)
//  4. Scrubs claims (claim_text derived from reasoning contains PII)
//  5. Nulls out embeddings (contain semantic PII)
//  6. Inserts a decision_erasures row with the original hash
//  7. Records a DecisionErased event and mutation audit entry
//  8. Queues a search index deletion
//
// Does NOT set valid_to — the decision remains "active" but scrubbed.
func (db *DB) EraseDecision(
	ctx context.Context,
	orgID, decisionID uuid.UUID,
	reason, erasedBy string,
	audit *MutationAuditEntry,
) (DecisionErasureResult, error) {
	var result DecisionErasureResult
	err := db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Activate the erasure bypass for the immutability trigger.
		// SET LOCAL is scoped to this transaction and auto-resets on commit/rollback.
		if _, err := tx.Exec(ctx, `SET LOCAL akashi.erasure_in_progress = 'true'`); err != nil {
			return fmt.Errorf("storage: set erasure session var: %w", err)
		}

		// Fetch the decision. Must exist and belong to org.
		var runID uuid.UUID
		var agentID, oldOutcome, oldContentHash string
		var oldReasoning *string
		var decisionType string
		var confidence float32
		var validFrom time.Time
		err := tx.QueryRow(ctx,
			`SELECT run_id, agent_id, outcome, reasoning, decision_type, confidence,
		        content_hash, valid_from
		 FROM decisions WHERE id = $1 AND org_id = $2`,
			decisionID, orgID,
		).Scan(&runID, &agentID, &oldOutcome, &oldReasoning, &decisionType,
			&confidence, &oldContentHash, &validFrom)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("storage: decision %s: %w", decisionID, ErrNotFound)
			}
			return fmt.Errorf("storage: fetch decision for erasure: %w", err)
		}

		// Check idempotency: if already erased, return error.
		var alreadyErased bool
		err = tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM decision_erasures WHERE decision_id = $1)`,
			decisionID,
		).Scan(&alreadyErased)
		if err != nil {
			return fmt.Errorf("storage: check existing erasure: %w", err)
		}
		if alreadyErased {
			return fmt.Errorf("storage: decision %s: %w", decisionID, ErrAlreadyErased)
		}

		// Compute new content hash over scrubbed fields.
		erasedReasoning := ErasedSentinel
		newHash := integrity.ComputeContentHash(
			decisionID, decisionType, ErasedSentinel, confidence, &erasedReasoning, validFrom,
		)

		// Scrub the decision row.
		_, err = tx.Exec(ctx,
			`UPDATE decisions
		 SET outcome = $1, reasoning = $2, content_hash = $3,
		     embedding = NULL, outcome_embedding = NULL
		 WHERE id = $4 AND org_id = $5`,
			ErasedSentinel, ErasedSentinel, newHash, decisionID, orgID,
		)
		if err != nil {
			return fmt.Errorf("storage: scrub decision: %w", err)
		}

		// Scrub alternatives.
		altTag, err := tx.Exec(ctx,
			`UPDATE alternatives
		 SET label = $1, rejection_reason = $1
		 WHERE decision_id = $2
		   AND decision_id IN (SELECT id FROM decisions WHERE org_id = $3)`,
			ErasedSentinel, decisionID, orgID,
		)
		if err != nil {
			return fmt.Errorf("storage: scrub alternatives: %w", err)
		}

		// Scrub evidence.
		evTag, err := tx.Exec(ctx,
			`UPDATE evidence
		 SET content = $1, source_uri = NULL, embedding = NULL
		 WHERE decision_id = $2 AND org_id = $3`,
			ErasedSentinel, decisionID, orgID,
		)
		if err != nil {
			return fmt.Errorf("storage: scrub evidence: %w", err)
		}

		// Scrub claims (claim_text is derived from reasoning and may contain PII).
		claimTag, err := tx.Exec(ctx,
			`UPDATE decision_claims
		 SET claim_text = $1, embedding = NULL
		 WHERE decision_id = $2 AND org_id = $3`,
			ErasedSentinel, decisionID, orgID,
		)
		if err != nil {
			return fmt.Errorf("storage: scrub claims: %w", err)
		}

		// Queue search index deletion.
		if err := queueSearchOutbox(ctx, tx, decisionID, orgID, "delete"); err != nil {
			return fmt.Errorf("storage: queue search outbox delete in erasure: %w", err)
		}

		// Insert decision_erasures row.
		var erasure model.DecisionErasure
		err = tx.QueryRow(ctx,
			`INSERT INTO decision_erasures (decision_id, org_id, erased_by, original_hash, erased_hash, reason)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, decision_id, org_id, erased_by, original_hash, erased_hash, reason, erased_at`,
			decisionID, orgID, erasedBy, oldContentHash, newHash, reason,
		).Scan(&erasure.ID, &erasure.DecisionID, &erasure.OrgID, &erasure.ErasedBy,
			&erasure.OriginalHash, &erasure.ErasedHash, &erasure.Reason, &erasure.ErasedAt)
		if err != nil {
			return fmt.Errorf("storage: insert decision erasure: %w", err)
		}

		// Insert DecisionErased event.
		now := time.Now().UTC()
		payload := map[string]any{
			"decision_id":   decisionID.String(),
			"erased_by":     erasedBy,
			"original_hash": oldContentHash,
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
				"outcome":      oldOutcome,
				"reasoning":    oldReasoning,
				"content_hash": oldContentHash,
			}
			audit.AfterData = map[string]any{
				"outcome":      ErasedSentinel,
				"reasoning":    ErasedSentinel,
				"content_hash": newHash,
				"erased_by":    erasedBy,
				"reason":       reason,
			}
			if err := InsertMutationAuditTx(ctx, tx, *audit); err != nil {
				return fmt.Errorf("storage: audit in erasure tx: %w", err)
			}
		}

		result = DecisionErasureResult{
			Erasure:            erasure,
			AlternativesErased: altTag.RowsAffected(),
			EvidenceErased:     evTag.RowsAffected(),
			ClaimsErased:       claimTag.RowsAffected(),
		}
		return nil
	})
	if err != nil {
		return DecisionErasureResult{}, err
	}
	return result, nil
}

// GetDecisionErasure retrieves the erasure record for a decision, if one exists.
// Returns ErrNotFound if no erasure record exists for this decision.
func (db *DB) GetDecisionErasure(ctx context.Context, orgID, decisionID uuid.UUID) (model.DecisionErasure, error) {
	var e model.DecisionErasure
	err := db.pool.QueryRow(ctx,
		`SELECT id, decision_id, org_id, erased_by, original_hash, erased_hash, reason, erased_at
		 FROM decision_erasures WHERE decision_id = $1 AND org_id = $2`,
		decisionID, orgID,
	).Scan(&e.ID, &e.DecisionID, &e.OrgID, &e.ErasedBy,
		&e.OriginalHash, &e.ErasedHash, &e.Reason, &e.ErasedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.DecisionErasure{}, fmt.Errorf("storage: erasure for decision %s: %w", decisionID, ErrNotFound)
		}
		return model.DecisionErasure{}, fmt.Errorf("storage: get decision erasure: %w", err)
	}
	return e, nil
}

// QueryDecisions executes a structured query with filters, ordering, and pagination.
// Only returns active decisions (valid_to IS NULL). Use QueryDecisionsTemporal for
// point-in-time queries that include superseded decisions.
func (db *DB) QueryDecisions(ctx context.Context, orgID uuid.UUID, req model.QueryRequest) ([]model.Decision, int, error) {
	where, args := buildDecisionWhereClause(orgID, req.Filters, 1, true)

	// Filter by OTEL trace_id via agent_runs join.
	if req.TraceID != nil {
		args = append(args, *req.TraceID)
		where += fmt.Sprintf(" AND run_id IN (SELECT id FROM agent_runs WHERE trace_id = $%d AND org_id = $1)", len(args))
	}

	// Count total matching decisions.
	countQuery := "SELECT COUNT(*) FROM decisions" + where
	var total int
	if err := db.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("storage: count decisions: %w", err)
	}

	// Build order clause.
	orderBy := "valid_from"
	if req.OrderBy != "" {
		switch req.OrderBy {
		case "confidence", "valid_from", "decision_type", "outcome", "completeness_score", "outcome_score", "quality_score", "agent_id", "project":
			// quality_score accepted as deprecated alias; maps to the renamed column.
			if req.OrderBy == "quality_score" {
				orderBy = "completeness_score"
			} else {
				orderBy = req.OrderBy
			}
		}
	}
	orderDir := "DESC"
	if strings.EqualFold(req.OrderDir, "asc") {
		orderDir = "ASC"
	}

	limit, offset := clampPagination(req.Limit, req.Offset, 50, 1000)

	selectQuery := fmt.Sprintf(
		`SELECT %s FROM decisions%s ORDER BY %s %s LIMIT %d OFFSET %d`,
		decisionCols, where, orderBy, orderDir, limit, offset,
	)

	rows, err := db.pool.Query(ctx, selectQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: query decisions: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	if err != nil {
		return nil, 0, err
	}

	// Optionally load related data in batch (avoids N+1 queries).
	includeAlts := containsStr(req.Include, "alternatives")
	includeEvidence := containsStr(req.Include, "evidence")
	if (includeAlts || includeEvidence) && len(decisions) > 0 {
		ids := make([]uuid.UUID, len(decisions))
		for i := range decisions {
			ids[i] = decisions[i].ID
		}

		if includeAlts {
			altsMap, err := db.GetAlternativesByDecisions(ctx, ids, orgID)
			if err != nil {
				return nil, 0, err
			}
			for i := range decisions {
				decisions[i].Alternatives = altsMap[decisions[i].ID]
			}
		}
		if includeEvidence {
			evsMap, err := db.GetEvidenceByDecisions(ctx, ids, orgID)
			if err != nil {
				return nil, 0, err
			}
			for i := range decisions {
				decisions[i].Evidence = evsMap[decisions[i].ID]
			}
		}
	}

	return decisions, total, nil
}

// QueryDecisionsTemporal executes a bi-temporal point-in-time query.
func (db *DB) QueryDecisionsTemporal(ctx context.Context, orgID uuid.UUID, req model.TemporalQueryRequest) ([]model.Decision, error) {
	where, args := buildDecisionWhereClause(orgID, req.Filters, 1, false)

	// Add temporal conditions.
	argIdx := len(args) + 1
	where += fmt.Sprintf(
		" AND transaction_time <= $%d AND (valid_to IS NULL OR valid_to > $%d)",
		argIdx, argIdx+1,
	)
	args = append(args, req.AsOf, req.AsOf)

	// Enforce a result cap to prevent unbounded memory allocation.
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	argIdx = len(args) + 1
	limitClause := fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	query := `SELECT ` + decisionCols + ` FROM decisions` + where + ` ORDER BY valid_from DESC` + limitClause

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: temporal query: %w", err)
	}
	defer rows.Close()

	return scanDecisions(rows)
}

// SearchDecisionsByText performs full-text search over decision outcome, reasoning,
// and decision_type using PostgreSQL tsvector/tsquery (migration 022).
// Used as fallback when semantic search is disabled or the embedding provider is noop.
// Only returns active decisions (valid_to IS NULL) for consistency with the Qdrant search path.
//
// Search strategy:
//  1. websearch_to_tsquery: handles stemming, stop word removal, and supports
//     "quoted phrases", OR, and -exclusion. Most queries resolve here.
//  2. ILIKE fallback (OR-any-term): if FTS returns nothing (e.g. all stop words,
//     partial words, or terms absent from the English dictionary), try lenient
//     substring matching where any single term hitting any field is a match.
func (db *DB) SearchDecisionsByText(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}

	// Primary: PostgreSQL full-text search with ts_rank.
	// On FTS failure (e.g. websearch_to_tsquery parse error from malformed query),
	// fall back to ILIKE instead of returning 500.
	results, err := db.searchByFTS(ctx, orgID, query, filters, limit)
	if err != nil {
		return db.searchByILIKE(ctx, orgID, query, filters, limit)
	}
	if len(results) > 0 {
		return results, nil
	}

	// Fallback: OR-based ILIKE for cases FTS misses (typos, partial words,
	// all stop words, non-English terms).
	return db.searchByILIKE(ctx, orgID, query, filters, limit)
}

// HasDecisionsWithNullSearchVector returns true if any active decision has
// search_vector IS NULL (e.g. from a dropped trigger or incomplete backfill).
// Used for monitoring; FTS excludes such rows from results.
// SECURITY: Intentionally global — system health monitoring check, not
// user-facing. Result is a boolean logged by the maintenance goroutine.
func (db *DB) HasDecisionsWithNullSearchVector(ctx context.Context) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM decisions
			WHERE search_vector IS NULL AND valid_to IS NULL
		)`,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: check null search_vector: %w", err)
	}
	return exists, nil
}

// searchByFTS uses PostgreSQL websearch_to_tsquery for full-text search with
// stemming, stop word removal, and weighted ranking (outcome > type > reasoning).
func (db *DB) searchByFTS(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	args = append(args, query)
	qp := len(args)
	where += fmt.Sprintf(` AND search_vector @@ websearch_to_tsquery('english', $%d)`, qp)

	sql := fmt.Sprintf(
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context,
		 api_key_id, tool, model, project,
		 ts_rank(search_vector, websearch_to_tsquery('english', $%d))
		   * (0.5 + 0.2 * COALESCE(completeness_score, 0) + 0.3 * COALESCE(outcome_score, 0))
		   * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - valid_from)) / 86400.0 / 90.0))
		   AS relevance
		 FROM decisions%s
		 ORDER BY relevance DESC
		 LIMIT %d`, qp, where, limit,
	)

	return db.execSearchQuery(ctx, sql, args)
}

// searchByILIKE uses OR-any-term ILIKE matching as a fallback when FTS returns nothing.
// A result matches if any single query term appears in any searchable field.
func (db *DB) searchByILIKE(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	words := strings.Fields(query)
	if len(words) > 20 {
		words = words[:20]
	}
	if len(words) == 0 {
		return nil, nil
	}

	// OR across all terms: any word matching any field qualifies the row.
	// Uses ILIKE instead of LOWER()+LIKE so PostgreSQL can use pg_trgm GIN indexes.
	var termClauses []string
	for _, word := range words {
		escaped := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(word)
		args = append(args, "%"+escaped+"%")
		p := len(args)
		termClauses = append(termClauses, fmt.Sprintf(
			`(outcome ILIKE $%d OR COALESCE(reasoning, '') ILIKE $%d OR decision_type ILIKE $%d)`,
			p, p, p))
	}
	where += " AND (" + strings.Join(termClauses, " OR ") + ")"

	sql := fmt.Sprintf(
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context,
		 api_key_id, tool, model, project,
		 (0.5 + 0.2 * COALESCE(completeness_score, 0) + 0.3 * COALESCE(outcome_score, 0))
		   * (1.0 / (1.0 + EXTRACT(EPOCH FROM (NOW() - valid_from)) / 86400.0 / 90.0))
		   AS relevance
		 FROM decisions%s
		 ORDER BY relevance DESC
		 LIMIT %d`, where, limit,
	)

	return db.execSearchQuery(ctx, sql, args)
}

// execSearchQuery runs a search SQL and scans results into SearchResult structs.
func (db *DB) execSearchQuery(ctx context.Context, sql string, args []any) ([]model.SearchResult, error) {
	rows, err := db.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: text search decisions: %w", err)
	}
	defer rows.Close()

	results := make([]model.SearchResult, 0)
	for rows.Next() {
		var d model.Decision
		var relevance float32
		if err := rows.Scan(
			&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence,
			&d.Reasoning, &d.Metadata, &d.CompletenessScore, &d.OutcomeScore, &d.PrecedentRef,
			&d.PrecedentReason, &d.SupersedesID, &d.ContentHash,
			&d.ValidFrom, &d.ValidTo, &d.TransactionTime, &d.CreatedAt,
			&d.SessionID, &d.AgentContext, &d.APIKeyID,
			&d.Tool, &d.Model, &d.Project,
			&relevance,
		); err != nil {
			return nil, fmt.Errorf("storage: scan text search result: %w", err)
		}
		results = append(results, model.SearchResult{Decision: d, SimilarityScore: relevance})
	}
	return results, rows.Err()
}

// GetDecisionsByAgent returns active decisions for a given agent within an org with pagination.
// Only returns decisions with valid_to IS NULL (not revised/invalidated).
func (db *DB) GetDecisionsByAgent(ctx context.Context, orgID uuid.UUID, agentID string, limit, offset int, from, to *time.Time) ([]model.Decision, int, error) {
	limit, offset = clampPagination(limit, offset, 50, 1000)

	filters := model.QueryFilters{
		AgentIDs: []string{agentID},
	}
	if from != nil || to != nil {
		filters.TimeRange = &model.TimeRange{From: from, To: to}
	}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	var total int
	if err := db.pool.QueryRow(ctx, "SELECT COUNT(*) FROM decisions"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("storage: count agent decisions: %w", err)
	}

	query := fmt.Sprintf(
		`SELECT %s FROM decisions%s ORDER BY valid_from DESC LIMIT %d OFFSET %d`,
		decisionCols, where, limit, offset,
	)

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: get agent decisions: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	return decisions, total, err
}

func buildDecisionWhereClause(orgID uuid.UUID, f model.QueryFilters, startArgIdx int, currentOnly bool) (string, []any) {
	var conditions []string
	var args []any
	idx := startArgIdx

	// Org isolation is always the first condition.
	conditions = append(conditions, fmt.Sprintf("org_id = $%d", idx))
	args = append(args, orgID)
	idx++

	if currentOnly {
		conditions = append(conditions, "valid_to IS NULL")
	}

	if len(f.AgentIDs) > 0 {
		conditions = append(conditions, fmt.Sprintf("agent_id = ANY($%d)", idx))
		args = append(args, f.AgentIDs)
		idx++
	}
	if f.RunID != nil {
		conditions = append(conditions, fmt.Sprintf("run_id = $%d", idx))
		args = append(args, *f.RunID)
		idx++
	}
	if f.DecisionType != nil {
		conditions = append(conditions, fmt.Sprintf("decision_type = $%d", idx))
		args = append(args, *f.DecisionType)
		idx++
	}
	if f.ConfidenceMin != nil {
		conditions = append(conditions, fmt.Sprintf("confidence >= $%d", idx))
		args = append(args, *f.ConfidenceMin)
		idx++
	}
	if f.Outcome != nil {
		conditions = append(conditions, fmt.Sprintf("outcome = $%d", idx))
		args = append(args, *f.Outcome)
		idx++
	}
	if f.TimeRange != nil {
		if f.TimeRange.From != nil {
			conditions = append(conditions, fmt.Sprintf("valid_from >= $%d", idx))
			args = append(args, *f.TimeRange.From)
			idx++
		}
		if f.TimeRange.To != nil {
			conditions = append(conditions, fmt.Sprintf("valid_from <= $%d", idx))
			args = append(args, *f.TimeRange.To)
			idx++
		}
	}
	if f.SessionID != nil {
		conditions = append(conditions, fmt.Sprintf("session_id = $%d", idx))
		args = append(args, *f.SessionID)
		idx++
	}
	if f.Tool != nil {
		conditions = append(conditions, fmt.Sprintf("tool = $%d", idx))
		args = append(args, *f.Tool)
		idx++
	}
	if f.Model != nil {
		conditions = append(conditions, fmt.Sprintf("model = $%d", idx))
		args = append(args, *f.Model)
		idx++
	}
	if f.Project != nil {
		conditions = append(conditions, fmt.Sprintf("project = $%d", idx))
		args = append(args, *f.Project)
		idx++ //nolint:ineffassign // keep idx consistent so future additions don't miscount
	}

	return " WHERE " + strings.Join(conditions, " AND "), args
}

// ExportDecisionsCursor returns a page of decisions using keyset pagination on
// (valid_from, id). This avoids the O(offset) scan cost of OFFSET-based pagination,
// making it suitable for streaming large exports. Pass a nil cursor for the first page.
func (db *DB) ExportDecisionsCursor(ctx context.Context, orgID uuid.UUID, filters model.QueryFilters, cursor *ExportCursor, limit int) ([]model.Decision, error) {
	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	if cursor != nil {
		idx := len(args) + 1
		where += fmt.Sprintf(" AND (valid_from, id) > ($%d, $%d)", idx, idx+1)
		args = append(args, cursor.ValidFrom, cursor.ID)
	}

	query := fmt.Sprintf(
		`SELECT %s FROM decisions%s ORDER BY valid_from ASC, id ASC LIMIT %d`,
		decisionCols, where, limit,
	)

	rows, err := db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: export decisions cursor: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	if err != nil {
		return nil, err
	}

	// Batch-load alternatives and evidence.
	if len(decisions) > 0 {
		ids := make([]uuid.UUID, len(decisions))
		for i := range decisions {
			ids[i] = decisions[i].ID
		}

		altsMap, err := db.GetAlternativesByDecisions(ctx, ids, orgID)
		if err != nil {
			return nil, err
		}
		evsMap, err := db.GetEvidenceByDecisions(ctx, ids, orgID)
		if err != nil {
			return nil, err
		}
		for i := range decisions {
			decisions[i].Alternatives = altsMap[decisions[i].ID]
			decisions[i].Evidence = evsMap[decisions[i].ID]
		}
	}

	return decisions, nil
}

// ExportCursor holds the keyset cursor position for cursor-based export pagination.
type ExportCursor struct {
	ValidFrom time.Time
	ID        uuid.UUID
}

// GetDecisionTimeline returns decisions aggregated into time buckets for the
// timeline summary view. It performs two queries:
//  1. Aggregate counts, avg confidence, and type/agent breakdowns per bucket.
//  2. Top N decisions per bucket (by confidence DESC) for the executive summary.
//
// Conflicts are counted from scored_conflicts.
func (db *DB) GetDecisionTimeline(ctx context.Context, orgID uuid.UUID, granularity string, agentID, project *string, from, to time.Time, topN int) ([]model.TimelineBucket, error) {
	truncFn := "day"
	if granularity == "week" {
		truncFn = "week"
	}

	// Build WHERE clause for filters.
	conditions := []string{"org_id = $1", "valid_to IS NULL"}
	args := []any{orgID}
	idx := 2

	conditions = append(conditions, fmt.Sprintf("valid_from >= $%d", idx))
	args = append(args, from)
	idx++
	conditions = append(conditions, fmt.Sprintf("valid_from < $%d", idx))
	args = append(args, to)
	idx++

	if agentID != nil && *agentID != "" {
		conditions = append(conditions, fmt.Sprintf("agent_id = $%d", idx))
		args = append(args, *agentID)
		idx++
	}
	if project != nil && *project != "" {
		conditions = append(conditions, fmt.Sprintf("project = $%d", idx))
		args = append(args, *project)
		idx++ //nolint:ineffassign
	}

	where := " WHERE " + strings.Join(conditions, " AND ")

	// Get all matching decisions with minimal columns, aggregate in Go.
	selectQuery := fmt.Sprintf(
		`SELECT id, agent_id, decision_type, outcome, confidence, project, valid_from
		 FROM decisions%s
		 ORDER BY valid_from DESC`,
		where,
	)

	rows, err := db.pool.Query(ctx, selectQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: decision timeline: %w", err)
	}
	defer rows.Close()

	type lightDecision struct {
		ID           uuid.UUID
		AgentID      string
		DecisionType string
		Outcome      string
		Confidence   float32
		Project      *string
		ValidFrom    time.Time
	}

	var allDecisions []lightDecision
	for rows.Next() {
		var d lightDecision
		if err := rows.Scan(&d.ID, &d.AgentID, &d.DecisionType, &d.Outcome, &d.Confidence, &d.Project, &d.ValidFrom); err != nil {
			return nil, fmt.Errorf("storage: scan timeline decision: %w", err)
		}
		allDecisions = append(allDecisions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: timeline rows: %w", err)
	}

	// Group by bucket.
	bucketMap := make(map[string]*model.TimelineBucket)
	var bucketOrder []string

	for _, d := range allDecisions {
		var bucketKey string
		if truncFn == "week" {
			y, w := d.ValidFrom.UTC().ISOWeek()
			// Find the Monday of this ISO week.
			monday := isoWeekStart(y, w)
			bucketKey = monday.Format("2006-01-02")
		} else {
			bucketKey = d.ValidFrom.UTC().Format("2006-01-02")
		}

		b, ok := bucketMap[bucketKey]
		if !ok {
			b = &model.TimelineBucket{
				Bucket:        bucketKey,
				DecisionTypes: make(map[string]int),
				Agents:        make(map[string]int),
			}
			bucketMap[bucketKey] = b
			bucketOrder = append(bucketOrder, bucketKey)
		}

		b.DecisionCount++
		b.AvgConfidence += float64(d.Confidence)
		b.DecisionTypes[d.DecisionType]++
		b.Agents[d.AgentID]++

		// Keep top N by confidence for each bucket.
		td := model.TimelineDecision{
			ID:           d.ID,
			AgentID:      d.AgentID,
			DecisionType: d.DecisionType,
			Outcome:      d.Outcome,
			Confidence:   d.Confidence,
			Project:      d.Project,
			CreatedAt:    d.ValidFrom,
		}
		if len(b.TopDecisions) < topN {
			b.TopDecisions = append(b.TopDecisions, td)
			// Sort to keep highest confidence first.
			sort.Slice(b.TopDecisions, func(i, j int) bool {
				return b.TopDecisions[i].Confidence > b.TopDecisions[j].Confidence
			})
		} else if d.Confidence > b.TopDecisions[topN-1].Confidence {
			b.TopDecisions[topN-1] = td
			sort.Slice(b.TopDecisions, func(i, j int) bool {
				return b.TopDecisions[i].Confidence > b.TopDecisions[j].Confidence
			})
		}
	}

	// Finalize averages and truncate outcomes.
	const maxOutcomeLen = 200
	for _, key := range bucketOrder {
		b := bucketMap[key]
		if b.DecisionCount > 0 {
			b.AvgConfidence /= float64(b.DecisionCount)
		}
		for i := range b.TopDecisions {
			if len(b.TopDecisions[i].Outcome) > maxOutcomeLen {
				b.TopDecisions[i].Outcome = b.TopDecisions[i].Outcome[:maxOutcomeLen] + "..."
			}
		}
	}

	// Count conflicts per bucket, respecting agent/project filters.
	conflictConditions := []string{"sc.org_id = $1", "sc.detected_at >= $2", "sc.detected_at < $3"}
	conflictArgs := []any{orgID, from, to}
	conflictIdx := 4

	if agentID != nil && *agentID != "" {
		conflictConditions = append(conflictConditions,
			fmt.Sprintf("(sc.agent_a = $%d OR sc.agent_b = $%d)", conflictIdx, conflictIdx))
		conflictArgs = append(conflictArgs, *agentID)
		conflictIdx++
	}
	if project != nil && *project != "" {
		// scored_conflicts has no project column; join through decisions.
		conflictConditions = append(conflictConditions,
			fmt.Sprintf(`EXISTS (
				SELECT 1 FROM decisions d
				WHERE d.id IN (sc.decision_a_id, sc.decision_b_id)
				  AND d.project = $%d AND d.org_id = sc.org_id
			)`, conflictIdx))
		conflictArgs = append(conflictArgs, *project)
		conflictIdx++ //nolint:ineffassign
	}

	conflictQuery := fmt.Sprintf(
		`SELECT date_trunc('%s', sc.detected_at) AS bucket, COUNT(*)
		 FROM scored_conflicts sc
		 WHERE %s
		 GROUP BY bucket`,
		truncFn, strings.Join(conflictConditions, " AND "),
	)
	conflictRows, err := db.pool.Query(ctx, conflictQuery, conflictArgs...)
	if err != nil {
		return nil, fmt.Errorf("storage: timeline conflicts: %w", err)
	}
	defer conflictRows.Close()

	for conflictRows.Next() {
		var bucketTime time.Time
		var count int
		if err := conflictRows.Scan(&bucketTime, &count); err != nil {
			return nil, fmt.Errorf("storage: scan timeline conflict: %w", err)
		}
		var bucketKey string
		if truncFn == "week" {
			y, w := bucketTime.UTC().ISOWeek()
			monday := isoWeekStart(y, w)
			bucketKey = monday.Format("2006-01-02")
		} else {
			bucketKey = bucketTime.UTC().Format("2006-01-02")
		}
		if b, ok := bucketMap[bucketKey]; ok {
			b.ConflictCount = count
		}
	}

	// Build result slice in order.
	result := make([]model.TimelineBucket, 0, len(bucketOrder))
	for _, key := range bucketOrder {
		result = append(result, *bucketMap[key])
	}
	return result, nil
}

// isoWeekStart returns the Monday of the given ISO week.
func isoWeekStart(isoYear, isoWeek int) time.Time {
	// Jan 4 is always in ISO week 1.
	jan4 := time.Date(isoYear, time.January, 4, 0, 0, 0, 0, time.UTC)
	// Find the Monday of week 1.
	weekday := jan4.Weekday()
	if weekday == time.Sunday {
		weekday = 7
	}
	week1Monday := jan4.AddDate(0, 0, -int(weekday-time.Monday))
	return week1Monday.AddDate(0, 0, (isoWeek-1)*7)
}

func scanDecisions(rows pgx.Rows) ([]model.Decision, error) {
	decisions := make([]model.Decision, 0)
	for rows.Next() {
		d, err := scanOneDecision(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

// GetDecisionsByIDs returns active decisions for the given IDs within an org.
// Only returns decisions with valid_to IS NULL (not revised/invalidated).
// Used to hydrate search results from Qdrant back to full Decision objects.
func (db *DB) GetDecisionsByIDs(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]model.Decision, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT `+decisionCols+` FROM decisions
		 WHERE org_id = $1 AND id = ANY($2) AND valid_to IS NULL`,
		orgID, ids,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get decisions by IDs: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	if err != nil {
		return nil, err
	}

	result := make(map[uuid.UUID]model.Decision, len(decisions))
	for _, d := range decisions {
		result[d.ID] = d
	}
	return result, nil
}

// GetDecisionRevisions returns the full revision chain for a decision, walking
// both backwards (via supersedes_id) and forwards (via decisions that reference
// this one's id as their supersedes_id). Results are ordered by valid_from ASC.
//
// The CTE is split into two separate recursive queries (forward_chain and
// backward_chain) then UNIONed, because PostgreSQL only treats the last
// branch of a UNION as the recursive term. A single CTE with three branches
// would only recurse on the last (forward) branch, truncating deep backward chains.
// Each recursive branch has a LIMIT 100 safety cap to prevent infinite loops from
// circular supersedes_id references.
func (db *DB) GetDecisionRevisions(ctx context.Context, orgID, id uuid.UUID) ([]model.Decision, error) {
	query := `
	WITH RECURSIVE
	forward_chain AS (
		-- Anchor: the target decision.
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, project, 0 AS depth
		FROM decisions
		WHERE id = $1 AND org_id = $2

		UNION ALL

		-- Walk forward: find decisions that supersede the current one.
		SELECT d.id, d.run_id, d.agent_id, d.org_id, d.decision_type, d.outcome, d.confidence, d.reasoning,
		       d.metadata, d.completeness_score, d.outcome_score, d.precedent_ref, d.precedent_reason, d.supersedes_id, d.content_hash,
		       d.valid_from, d.valid_to, d.transaction_time, d.created_at, d.session_id, d.agent_context, d.api_key_id, d.tool, d.model, d.project, fc.depth + 1
		FROM decisions d
		INNER JOIN forward_chain fc ON d.supersedes_id = fc.id
		WHERE d.org_id = $2 AND fc.depth < 100
	),
	backward_chain AS (
		-- Anchor: the target decision.
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, project, 0 AS depth
		FROM decisions
		WHERE id = $1 AND org_id = $2

		UNION ALL

		-- Walk backward: follow supersedes_id links.
		SELECT d.id, d.run_id, d.agent_id, d.org_id, d.decision_type, d.outcome, d.confidence, d.reasoning,
		       d.metadata, d.completeness_score, d.outcome_score, d.precedent_ref, d.precedent_reason, d.supersedes_id, d.content_hash,
		       d.valid_from, d.valid_to, d.transaction_time, d.created_at, d.session_id, d.agent_context, d.api_key_id, d.tool, d.model, d.project, bc.depth + 1
		FROM decisions d
		INNER JOIN backward_chain bc ON bc.supersedes_id = d.id
		WHERE d.org_id = $2 AND bc.depth < 100
	),
	all_revisions AS (
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, project
		FROM forward_chain
		UNION
		SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		       metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
		       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, project
		FROM backward_chain
	)
	SELECT DISTINCT ON (id) id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
	       metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
	       valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id, tool, model, project
	FROM all_revisions
	ORDER BY id, valid_from ASC`

	rows, err := db.pool.Query(ctx, query, id, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get decision revisions: %w", err)
	}
	defer rows.Close()

	decisions, err := scanDecisions(rows)
	if err != nil {
		return nil, err
	}

	// Re-sort by valid_from ASC for chronological ordering.
	// DISTINCT ON (id) sorts by (id, valid_from) which doesn't give us valid_from ASC across IDs.
	sortDecisionsByValidFrom(decisions)
	return decisions, nil
}

// GetRevisionChainIDs returns the IDs of all decisions in the same revision
// chain as the given decision. Walks both forward (decisions that supersede
// this one) and backward (decisions this one supersedes), capped at 100 hops
// in each direction. The result excludes the input ID itself.
//
// This is a lightweight alternative to GetDecisionRevisions for cases where
// only membership testing is needed (e.g., conflict scorer exclusion).
func (db *DB) GetRevisionChainIDs(ctx context.Context, id, orgID uuid.UUID) ([]uuid.UUID, error) {
	query := `
	WITH RECURSIVE
	forward_chain AS (
		SELECT id, supersedes_id, 0 AS depth FROM decisions WHERE id = $1 AND org_id = $2
		UNION ALL
		SELECT d.id, d.supersedes_id, fc.depth + 1
		FROM decisions d
		INNER JOIN forward_chain fc ON d.supersedes_id = fc.id
		WHERE d.org_id = $2 AND fc.depth < 100
	),
	backward_chain AS (
		SELECT id, supersedes_id, 0 AS depth FROM decisions WHERE id = $1 AND org_id = $2
		UNION ALL
		SELECT d.id, d.supersedes_id, bc.depth + 1
		FROM decisions d
		INNER JOIN backward_chain bc ON bc.supersedes_id = d.id
		WHERE d.org_id = $2 AND bc.depth < 100
	)
	SELECT DISTINCT id FROM (
		SELECT id FROM forward_chain WHERE id != $1
		UNION
		SELECT id FROM backward_chain WHERE id != $1
	) chain_ids`

	rows, err := db.pool.Query(ctx, query, id, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get revision chain IDs: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var chainID uuid.UUID
		if err := rows.Scan(&chainID); err != nil {
			return nil, fmt.Errorf("storage: scan revision chain ID: %w", err)
		}
		ids = append(ids, chainID)
	}
	return ids, rows.Err()
}

// GetRevisionDepth returns how many preceding revisions exist for a decision
// by walking backward through the supersedes_id chain. A decision with no
// supersedes_id has depth 0. Capped at 100 hops to prevent infinite loops.
func (db *DB) GetRevisionDepth(ctx context.Context, id, orgID uuid.UUID) (int, error) {
	var depth int
	err := db.pool.QueryRow(ctx, `
		WITH RECURSIVE chain AS (
			SELECT supersedes_id, 0 AS depth
			FROM decisions
			WHERE id = $1 AND org_id = $2
			UNION ALL
			SELECT d.supersedes_id, c.depth + 1
			FROM decisions d
			INNER JOIN chain c ON c.supersedes_id = d.id
			WHERE d.org_id = $2 AND c.depth < 100
		)
		SELECT COALESCE(MAX(depth), 0) FROM chain`,
		id, orgID,
	).Scan(&depth)
	if err != nil {
		return 0, fmt.Errorf("storage: get revision depth: %w", err)
	}
	return depth, nil
}

// sortDecisionsByValidFrom sorts a slice of decisions by valid_from ascending.
func sortDecisionsByValidFrom(decisions []model.Decision) {
	sort.Slice(decisions, func(i, j int) bool {
		return decisions[i].ValidFrom.Before(decisions[j].ValidFrom)
	})
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// FindUnembeddedDecisions returns active decisions that have no embedding vector,
// ordered oldest-first so the backfill processes them chronologically.
// SECURITY: Intentionally global — background backfill across all orgs. Each
// returned row includes OrgID for downstream scoping (BackfillEmbedding).
func (db *DB) FindUnembeddedDecisions(ctx context.Context, limit int) ([]UnembeddedDecision, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, decision_type, outcome, reasoning
		 FROM decisions
		 WHERE embedding IS NULL AND valid_to IS NULL
		 ORDER BY valid_from ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: find unembedded decisions: %w", err)
	}
	defer rows.Close()

	var results []UnembeddedDecision
	for rows.Next() {
		var d UnembeddedDecision
		if err := rows.Scan(&d.ID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Reasoning); err != nil {
			return nil, fmt.Errorf("storage: scan unembedded decision: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// BackfillEmbedding updates a decision's embedding and queues a search outbox
// entry so the outbox worker syncs it to Qdrant. Both writes are atomic.
func (db *DB) BackfillEmbedding(ctx context.Context, id, orgID uuid.UUID, emb pgvector.Vector) error {
	return db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE decisions SET embedding = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
			emb, id, orgID)
		if err != nil {
			return fmt.Errorf("storage: update embedding: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // Decision was revised or deleted — skip silently.
		}

		if err := queueSearchOutbox(ctx, tx, id, orgID, "upsert"); err != nil {
			return fmt.Errorf("storage: queue backfill outbox: %w", err)
		}
		return nil
	})
}

// FindDecisionsMissingOutcomeEmbedding returns active decisions that have
// embedding but no outcome_embedding (for backfilling Option B).
// SECURITY: Intentionally global — background backfill across all orgs. Each
// returned row includes OrgID for downstream scoping (BackfillOutcomeEmbedding).
func (db *DB) FindDecisionsMissingOutcomeEmbedding(ctx context.Context, limit int) ([]UnembeddedDecision, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, decision_type, outcome, reasoning
		 FROM decisions
		 WHERE embedding IS NOT NULL AND outcome_embedding IS NULL AND valid_to IS NULL
		 ORDER BY valid_from ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: find decisions missing outcome embedding: %w", err)
	}
	defer rows.Close()

	var results []UnembeddedDecision
	for rows.Next() {
		var d UnembeddedDecision
		if err := rows.Scan(&d.ID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Reasoning); err != nil {
			return nil, fmt.Errorf("storage: scan decision: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// BackfillOutcomeEmbedding updates a decision's outcome_embedding. Unlike
// BackfillEmbedding, this does not queue an outbox entry — outcome_embedding
// is used only for semantic conflict detection, not for Qdrant vector search.
func (db *DB) BackfillOutcomeEmbedding(ctx context.Context, id, orgID uuid.UUID, emb pgvector.Vector) error {
	tag, err := db.pool.Exec(ctx,
		`UPDATE decisions SET outcome_embedding = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
		emb, id, orgID)
	if err != nil {
		return fmt.Errorf("storage: update outcome embedding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // Decision was revised or deleted — skip silently.
	}
	return nil
}

// PgCandidateFinder implements search.CandidateFinder using a Postgres sequential scan.
// This is the "no-Qdrant" fallback: acceptable for small deployments (<100k decisions)
// where latency from a sequential scan is tolerable. At scale, use QdrantIndex instead.
//
// Once Postgres HNSW indexes are dropped (migration 049), this performs a full sequential
// scan for each ANN call — suitable only when Qdrant is unavailable and the dataset is small.
type PgCandidateFinder struct {
	db *DB
}

// NewPgCandidateFinder returns a CandidateFinder backed by Postgres sequential scan.
func NewPgCandidateFinder(db *DB) *PgCandidateFinder {
	return &PgCandidateFinder{db: db}
}

// FindSimilar returns the top-limit decisions ordered by cosine distance to embedding,
// scoped to the org. excludeID is filtered in SQL. project scoping is strict: a non-nil
// project matches only decisions with that exact project value; a nil project matches only
// decisions with no project set. This mirrors the Qdrant path and prevents cross-project
// conflict contamination.
func (f *PgCandidateFinder) FindSimilar(ctx context.Context, orgID uuid.UUID, embedding []float32, excludeID uuid.UUID, projects []string, limit int) ([]search.Result, error) {
	if limit <= 0 {
		limit = 50
	}
	emb := pgvector.NewVector(embedding)

	var q string
	var args []any
	switch len(projects) {
	case 0:
		q = `SELECT id, 1 - (embedding <=> $3) AS score
		     FROM decisions
		     WHERE org_id = $1 AND id != $2 AND embedding IS NOT NULL AND outcome_embedding IS NOT NULL AND valid_to IS NULL
		       AND project IS NULL
		     ORDER BY embedding <=> $3
		     LIMIT $4`
		args = []any{orgID, excludeID, emb, limit}
	case 1:
		q = `SELECT id, 1 - (embedding <=> $3) AS score
		     FROM decisions
		     WHERE org_id = $1 AND id != $2 AND embedding IS NOT NULL AND outcome_embedding IS NOT NULL AND valid_to IS NULL
		       AND project = $5
		     ORDER BY embedding <=> $3
		     LIMIT $4`
		args = []any{orgID, excludeID, emb, limit, projects[0]}
	default:
		q = `SELECT id, 1 - (embedding <=> $3) AS score
		     FROM decisions
		     WHERE org_id = $1 AND id != $2 AND embedding IS NOT NULL AND outcome_embedding IS NOT NULL AND valid_to IS NULL
		       AND project = ANY($5)
		     ORDER BY embedding <=> $3
		     LIMIT $4`
		args = []any{orgID, excludeID, emb, limit, projects}
	}

	rows, err := f.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: pg candidate finder: %w", err)
	}
	defer rows.Close()

	var results []search.Result
	for rows.Next() {
		var id uuid.UUID
		var score float32
		if err := rows.Scan(&id, &score); err != nil {
			return nil, fmt.Errorf("storage: scan candidate: %w", err)
		}
		results = append(results, search.Result{DecisionID: id, Score: score})
	}
	return results, rows.Err()
}

// GetDecisionEmbeddings returns (embedding, outcome_embedding) pairs for a batch of decisions.
// Used by consensus scoring to prepare Qdrant queries and pairwise cosine comparisons.
// Only decisions with both embeddings populated are included in the result.
func (db *DB) GetDecisionEmbeddings(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID][2]pgvector.Vector, error) {
	if len(ids) == 0 {
		return map[uuid.UUID][2]pgvector.Vector{}, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, embedding, outcome_embedding
		 FROM decisions
		 WHERE id = ANY($1) AND org_id = $2 AND valid_to IS NULL
		   AND embedding IS NOT NULL AND outcome_embedding IS NOT NULL`,
		ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get decision embeddings: %w", err)
	}
	defer rows.Close()

	result := make(map[uuid.UUID][2]pgvector.Vector, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var emb, outcomeEmb pgvector.Vector
		if err := rows.Scan(&id, &emb, &outcomeEmb); err != nil {
			return nil, fmt.Errorf("storage: scan decision embeddings: %w", err)
		}
		result[id] = [2]pgvector.Vector{emb, outcomeEmb}
	}
	return result, rows.Err()
}

// GetConflictCount returns the number of open conflicts involving a decision.
func (db *DB) GetConflictCount(ctx context.Context, decisionID, orgID uuid.UUID) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*)
		 FROM scored_conflicts
		 WHERE org_id = $2
		   AND status = 'open'
		   AND (decision_a_id = $1 OR decision_b_id = $1)`,
		decisionID, orgID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: conflict count: %w", err)
	}
	return count, nil
}

// GetConflictCountsBatch returns conflict counts for a batch of decisions.
// A conflict is counted if there is an open entry in scored_conflicts.
// Decisions with no conflicts are absent from the returned map.
func (db *DB) GetConflictCountsBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID]int, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]int{}, nil
	}

	rows, err := db.pool.Query(ctx, `
		WITH batch AS (SELECT unnest($1::uuid[]) AS id)
		SELECT b.id, COUNT(*) AS conflict_count
		FROM batch b
		JOIN scored_conflicts sc
		     ON (sc.decision_a_id = b.id OR sc.decision_b_id = b.id)
		     AND sc.org_id = $2
		     AND sc.status = 'open'
		GROUP BY b.id`, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: conflict counts batch: %w", err)
	}
	defer rows.Close()

	result := make(map[uuid.UUID]int, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("storage: scan conflict count: %w", err)
		}
		result[id] = count
	}
	return result, rows.Err()
}

// FindEmbeddedDecisionIDs returns IDs of current decisions that have both
// embedding and outcome_embedding populated but have NOT yet been scored for
// conflicts (conflict_scored_at IS NULL). Used by conflict scoring backfill
// so that server restarts only score new decisions, not the entire corpus.
// SECURITY: Intentionally global — background backfill across all orgs. Each
// returned row includes OrgID for downstream scoping (ScoreForDecision).
func (db *DB) FindEmbeddedDecisionIDs(ctx context.Context, limit int) ([]DecisionRef, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id FROM decisions
		 WHERE valid_to IS NULL
		   AND embedding IS NOT NULL
		   AND outcome_embedding IS NOT NULL
		   AND conflict_scored_at IS NULL
		 ORDER BY valid_from ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: find embedded decision IDs: %w", err)
	}
	defer rows.Close()

	var refs []DecisionRef
	for rows.Next() {
		var r DecisionRef
		if err := rows.Scan(&r.ID, &r.OrgID); err != nil {
			return nil, fmt.Errorf("storage: scan embedded decision ref: %w", err)
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// MarkDecisionConflictScored sets conflict_scored_at to now() for a decision.
// Called after ScoreForDecision completes so the decision won't be re-processed
// on subsequent backfill runs.
func (db *DB) MarkDecisionConflictScored(ctx context.Context, id, orgID uuid.UUID) error {
	_, err := db.pool.Exec(ctx,
		`UPDATE decisions SET conflict_scored_at = now() WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return fmt.Errorf("storage: mark decision conflict scored: %w", err)
	}
	return nil
}

// CountUnscoredDecisions returns the number of decisions that have embeddings
// but have not yet been conflict-scored. Used by the OpenTelemetry observable
// gauge callback to report backfill progress.
// SECURITY: Intentionally global — aggregate metric with no tenant data exposed.
func (db *DB) CountUnscoredDecisions(ctx context.Context) (int64, error) {
	var count int64
	err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM decisions
		 WHERE valid_to IS NULL
		   AND embedding IS NOT NULL
		   AND outcome_embedding IS NOT NULL
		   AND conflict_scored_at IS NULL`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: count unscored decisions: %w", err)
	}
	return count, nil
}

// ResetConflictScoredAt clears conflict_scored_at for all decisions, forcing
// the next backfill to re-score everything. Used when transitioning from
// embedding-only to LLM-validated scoring so all pairs get re-evaluated.
// SECURITY: Intentionally global — this is a one-time migration operation on
// startup that transitions ALL orgs from embedding-only to LLM-validated
// scoring. Scoping per-org would require iterating ListOrganizationIDs and
// yield the same result since the condition is server-wide (HasLLMValidator).
func (db *DB) ResetConflictScoredAt(ctx context.Context) (int64, error) {
	tag, err := db.pool.Exec(ctx,
		`UPDATE decisions SET conflict_scored_at = NULL WHERE conflict_scored_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("storage: reset conflict scored_at: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountUnvalidatedConflicts returns the number of scored_conflicts that were
// not scored by the current LLM classifier (llm_v2). This includes embedding-only
// conflicts and old binary-verdict 'llm' conflicts, both of which are stale.
// Used to decide whether to clear old conflicts at startup when transitioning
// to LLM validation. Returns 0 once all conflicts are llm_v2, so subsequent
// restarts skip the migration entirely.
// SECURITY: Intentionally global — startup check that determines whether the
// LLM migration path should fire. A non-zero count anywhere triggers the
// migration for all orgs.
func (db *DB) CountUnvalidatedConflicts(ctx context.Context) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM scored_conflicts WHERE scoring_method NOT IN ('llm_v2')`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storage: count unvalidated conflicts: %w", err)
	}
	return count, nil
}

// GetDecisionQualityStats returns aggregate quality metrics for current decisions in an org.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (db *DB) GetDecisionQualityStats(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (DecisionQualityStats, error) {
	var s DecisionQualityStats
	q := `
		SELECT count(*),
		       COALESCE(avg(completeness_score), 0),
		       count(*) FILTER (WHERE completeness_score < 0.5),
		       count(*) FILTER (WHERE completeness_score < 0.33),
		       count(*) FILTER (WHERE reasoning IS NOT NULL AND reasoning != ''),
		       count(*) FILTER (WHERE EXISTS (
		           SELECT 1 FROM alternatives a WHERE a.decision_id = decisions.id
		       ))
		FROM decisions
		WHERE org_id = $1 AND valid_to IS NULL`
	args := []any{orgID}
	if from != nil {
		args = append(args, *from)
		q += fmt.Sprintf(" AND valid_from >= $%d", len(args))
	}
	if to != nil {
		args = append(args, *to)
		q += fmt.Sprintf(" AND valid_from < $%d", len(args))
	}
	err := db.pool.QueryRow(ctx, q, args...).Scan(
		&s.Total, &s.AvgCompleteness, &s.BelowHalf, &s.BelowThird, &s.WithReasoning, &s.WithAlternatives)
	if err != nil {
		return s, fmt.Errorf("storage: decision quality stats: %w", err)
	}
	return s, nil
}

// GetDecisionTypeDistribution returns the count of current decisions grouped by
// decision_type, ordered by count descending. Only active decisions (valid_to IS NULL)
// are included. When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (db *DB) GetDecisionTypeDistribution(ctx context.Context, orgID uuid.UUID, from, to *time.Time) ([]DecisionTypeCount, error) {
	q := `SELECT decision_type, count(*)
		 FROM decisions
		 WHERE org_id = $1 AND valid_to IS NULL`
	args := []any{orgID}
	if from != nil {
		args = append(args, *from)
		q += fmt.Sprintf(" AND valid_from >= $%d", len(args))
	}
	if to != nil {
		args = append(args, *to)
		q += fmt.Sprintf(" AND valid_from < $%d", len(args))
	}
	q += ` GROUP BY decision_type ORDER BY count(*) DESC`

	rows, err := db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: decision type distribution: %w", err)
	}
	defer rows.Close()

	var result []DecisionTypeCount
	for rows.Next() {
		var dt DecisionTypeCount
		if err := rows.Scan(&dt.DecisionType, &dt.Count); err != nil {
			return nil, fmt.Errorf("storage: scan decision type count: %w", err)
		}
		result = append(result, dt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate decision type distribution: %w", err)
	}
	return result, nil
}

// GetCompletenessByDecisionType returns per-type average completeness for current
// decisions in an org. Results are ordered by average completeness ascending so
// the weakest decision types surface first. When from/to are non-nil, only
// decisions with valid_from in [from, to) are included.
func (db *DB) GetCompletenessByDecisionType(ctx context.Context, orgID uuid.UUID, from, to *time.Time) ([]DecisionTypeCompleteness, error) {
	q := `SELECT decision_type, count(*), COALESCE(avg(completeness_score), 0)
		 FROM decisions
		 WHERE org_id = $1 AND valid_to IS NULL`
	args := []any{orgID}
	if from != nil {
		args = append(args, *from)
		q += fmt.Sprintf(" AND valid_from >= $%d", len(args))
	}
	if to != nil {
		args = append(args, *to)
		q += fmt.Sprintf(" AND valid_from < $%d", len(args))
	}
	q += " GROUP BY decision_type ORDER BY COALESCE(avg(completeness_score), 0) ASC"

	rows, err := db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: completeness by decision type: %w", err)
	}
	defer rows.Close()

	var result []DecisionTypeCompleteness
	for rows.Next() {
		var dtc DecisionTypeCompleteness
		if err := rows.Scan(&dtc.DecisionType, &dtc.Count, &dtc.AvgCompleteness); err != nil {
			return nil, fmt.Errorf("storage: scan completeness by type: %w", err)
		}
		result = append(result, dtc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate completeness by type: %w", err)
	}
	return result, nil
}

// GetDecisionForScoring returns a decision with embedding, outcome_embedding,
// session_id, agent_context, and repo for conflict scoring. The additional fields
// provide project, task, and session context to the LLM validator.
func (db *DB) GetDecisionForScoring(ctx context.Context, id, orgID uuid.UUID) (model.Decision, error) {
	var d model.Decision
	err := db.pool.QueryRow(ctx,
		`SELECT id, run_id, agent_id, org_id, decision_type, outcome, confidence, reasoning,
		 valid_from, embedding, outcome_embedding, session_id, agent_context, project, transaction_time
		 FROM decisions WHERE id = $1 AND org_id = $2 AND valid_to IS NULL`,
		id, orgID,
	).Scan(
		&d.ID, &d.RunID, &d.AgentID, &d.OrgID, &d.DecisionType, &d.Outcome, &d.Confidence, &d.Reasoning,
		&d.ValidFrom, &d.Embedding, &d.OutcomeEmbedding, &d.SessionID, &d.AgentContext, &d.Project, &d.TransactionTime,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Decision{}, fmt.Errorf("storage: decision %s: %w", id, ErrNotFound)
		}
		return model.Decision{}, fmt.Errorf("storage: get decision for scoring: %w", err)
	}
	return d, nil
}

// GetDecisionOutcomeSignals returns temporal, graph, and fate outcome signals for a single decision.
// All signals are computed at query time from existing schema columns; none are stored.
func (db *DB) GetDecisionOutcomeSignals(ctx context.Context, id, orgID uuid.UUID) (model.OutcomeSignals, error) {
	var signals model.OutcomeSignals

	// Supersession velocity: hours from this decision's valid_from to the superseding decision's valid_from.
	if err := db.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM (s.valid_from - d.valid_from)) / 3600
		FROM decisions d
		JOIN decisions s ON s.supersedes_id = d.id AND s.org_id = d.org_id
		WHERE d.id = $1 AND d.org_id = $2
		LIMIT 1`, id, orgID).Scan(&signals.SupersessionVelocityHours); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return signals, fmt.Errorf("storage: supersession velocity: %w", err)
		}
		// ErrNoRows → never superseded, SupersessionVelocityHours remains nil.
	}

	// Precedent citation count: how many live decisions cite this one as a precedent.
	if err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM decisions
		WHERE precedent_ref = $1 AND org_id = $2 AND valid_to IS NULL`,
		id, orgID).Scan(&signals.PrecedentCitationCount); err != nil {
		return signals, fmt.Errorf("storage: precedent citation count: %w", err)
	}

	// Conflict fate: won/lost/no-winner counts across all resolved conflicts involving this decision.
	if err := db.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*) FILTER (WHERE winning_decision_id = $1)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NOT NULL AND winning_decision_id != $1)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NULL AND status = 'resolved')::int
		FROM scored_conflicts
		WHERE org_id = $2
		  AND status IN ('resolved', 'false_positive')
		  AND (decision_a_id = $1 OR decision_b_id = $1)`,
		id, orgID).Scan(
		&signals.ConflictFate.Won,
		&signals.ConflictFate.Lost,
		&signals.ConflictFate.ResolvedNoWinner,
	); err != nil {
		return signals, fmt.Errorf("storage: conflict fate: %w", err)
	}

	return signals, nil
}

// GetDecisionOutcomeSignalsBatch returns outcome signals for multiple decisions in three
// batched queries (no N+1). Returns a map of decision ID → OutcomeSignals.
// Decisions not found in the result have zero-value OutcomeSignals.
func (db *DB) GetDecisionOutcomeSignalsBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID]model.OutcomeSignals, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]model.OutcomeSignals{}, nil
	}

	result := make(map[uuid.UUID]model.OutcomeSignals, len(ids))
	for _, id := range ids {
		result[id] = model.OutcomeSignals{}
	}

	// Batched supersession velocity.
	velRows, err := db.pool.Query(ctx, `
		SELECT d.id, EXTRACT(EPOCH FROM (s.valid_from - d.valid_from)) / 3600
		FROM decisions d
		JOIN decisions s ON s.supersedes_id = d.id AND s.org_id = d.org_id
		WHERE d.id = ANY($1) AND d.org_id = $2`, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: batch supersession velocity: %w", err)
	}
	defer velRows.Close()
	for velRows.Next() {
		var id uuid.UUID
		var v float64
		if err := velRows.Scan(&id, &v); err != nil {
			return nil, fmt.Errorf("storage: scan batch supersession velocity: %w", err)
		}
		s := result[id]
		s.SupersessionVelocityHours = &v
		result[id] = s
	}
	if err := velRows.Err(); err != nil {
		return nil, fmt.Errorf("storage: batch supersession velocity rows: %w", err)
	}

	// Batched precedent citation counts.
	citeRows, err := db.pool.Query(ctx, `
		SELECT precedent_ref, COUNT(*)::int
		FROM decisions
		WHERE precedent_ref = ANY($1) AND org_id = $2 AND valid_to IS NULL
		GROUP BY precedent_ref`, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: batch precedent citations: %w", err)
	}
	defer citeRows.Close()
	for citeRows.Next() {
		var id uuid.UUID
		var count int
		if err := citeRows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("storage: scan batch precedent citations: %w", err)
		}
		s := result[id]
		s.PrecedentCitationCount = count
		result[id] = s
	}
	if err := citeRows.Err(); err != nil {
		return nil, fmt.Errorf("storage: batch precedent citations rows: %w", err)
	}

	// Batched conflict fate. Use UNION ALL so both sides of each conflict are correctly attributed.
	fateRows, err := db.pool.Query(ctx, `
		WITH sides AS (
		    SELECT decision_a_id AS target_id, winning_decision_id, status
		    FROM scored_conflicts
		    WHERE org_id = $2
		      AND status IN ('resolved', 'false_positive')
		      AND decision_a_id = ANY($1)
		    UNION ALL
		    SELECT decision_b_id AS target_id, winning_decision_id, status
		    FROM scored_conflicts
		    WHERE org_id = $2
		      AND status IN ('resolved', 'false_positive')
		      AND decision_b_id = ANY($1)
		)
		SELECT
		    target_id,
		    COUNT(*) FILTER (WHERE winning_decision_id = target_id)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NOT NULL AND winning_decision_id != target_id)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NULL AND status = 'resolved')::int
		FROM sides
		GROUP BY target_id`, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: batch conflict fate: %w", err)
	}
	defer fateRows.Close()
	for fateRows.Next() {
		var id uuid.UUID
		var won, lost, noWinner int
		if err := fateRows.Scan(&id, &won, &lost, &noWinner); err != nil {
			return nil, fmt.Errorf("storage: scan batch conflict fate: %w", err)
		}
		s := result[id]
		s.ConflictFate = model.ConflictFate{Won: won, Lost: lost, ResolvedNoWinner: noWinner}
		result[id] = s
	}
	if err := fateRows.Err(); err != nil {
		return nil, fmt.Errorf("storage: batch conflict fate rows: %w", err)
	}

	return result, nil
}

// GetCitationPercentilesForOrg computes percentile breakpoints [p25, p50, p75, p90]
// for precedent citation counts within an org's active decisions. Returns nil if
// no decisions have citations. Used by the percentile refresh loop to populate the
// in-memory PercentileCache for distribution-aware ReScore normalization (issue #264).
func (db *DB) GetCitationPercentilesForOrg(ctx context.Context, orgID uuid.UUID) ([]float64, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT percentile_cont(ARRAY[0.25, 0.50, 0.75, 0.90])
		       WITHIN GROUP (ORDER BY cnt)
		FROM (
			SELECT precedent_ref, COUNT(*)::int AS cnt
			FROM decisions
			WHERE org_id = $1 AND valid_to IS NULL AND precedent_ref IS NOT NULL
			GROUP BY precedent_ref
		) citation_counts`, orgID)

	var breakpoints []float64
	if err := row.Scan(&breakpoints); err != nil {
		// No rows = no citations in this org → return nil (cache miss → log fallback).
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("storage: citation percentiles for org %s: %w", orgID, err)
	}
	return breakpoints, nil
}

// LineageEntry is a compact summary of a decision in a lineage chain.
// Note: precedent_reason is intentionally omitted here. Each entry's own
// precedent_reason describes why it cited *its* predecessor — not why the
// queried decision cited it. Surfacing it on LineageEntry would mislead
// readers into thinking it explains the link they're looking at. The full
// Decision object carries precedent_reason unambiguously.
type LineageEntry struct {
	ID           uuid.UUID  `json:"id"`
	RunID        uuid.UUID  `json:"run_id"`
	AgentID      string     `json:"agent_id"`
	DecisionType string     `json:"decision_type"`
	Outcome      string     `json:"outcome"`
	Confidence   float32    `json:"confidence"`
	Project      *string    `json:"project,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ValidFrom    time.Time  `json:"valid_from"`
	ValidTo      *time.Time `json:"valid_to,omitempty"`
}

// DecisionLineage holds the upstream precedent and downstream citations for a decision.
type DecisionLineage struct {
	DecisionID  uuid.UUID      `json:"decision_id"`
	PrecededBy  *LineageEntry  `json:"preceded_by"`
	CitedBy     []LineageEntry `json:"cited_by"`
	CitedByMore bool           `json:"cited_by_has_more"`
}

const lineageCols = `id, run_id, agent_id, decision_type, outcome, confidence, project, created_at, valid_from, valid_to`

func scanLineageEntry(row pgxRowScanner) (LineageEntry, error) {
	var e LineageEntry
	if err := row.Scan(
		&e.ID, &e.RunID, &e.AgentID, &e.DecisionType, &e.Outcome,
		&e.Confidence, &e.Project, &e.CreatedAt, &e.ValidFrom, &e.ValidTo,
	); err != nil {
		return LineageEntry{}, fmt.Errorf("storage: scan lineage entry: %w", err)
	}
	return e, nil
}

// GetDecisionLineage returns the precedent chain for a decision: the decision it
// cites (preceded_by) and decisions that cite it (cited_by), scoped by org_id.
// cited_by is capped at limit+1 rows so the caller can detect has_more.
func (db *DB) GetDecisionLineage(ctx context.Context, id, orgID uuid.UUID, limit int) (DecisionLineage, error) {
	if limit <= 0 {
		limit = 20
	}

	result := DecisionLineage{DecisionID: id, CitedBy: []LineageEntry{}}

	// Look up this decision's precedent_ref.
	var precedentRef *uuid.UUID
	if err := db.pool.QueryRow(ctx,
		`SELECT precedent_ref FROM decisions WHERE id = $1 AND org_id = $2`,
		id, orgID).Scan(&precedentRef); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, fmt.Errorf("storage: decision %s not found: %w", id, err)
		}
		return result, fmt.Errorf("storage: lineage lookup: %w", err)
	}

	// Fetch the precedent decision summary if one exists.
	if precedentRef != nil {
		row := db.pool.QueryRow(ctx,
			`SELECT `+lineageCols+` FROM decisions WHERE id = $1 AND org_id = $2`,
			*precedentRef, orgID)
		entry, err := scanLineageEntry(row)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return result, fmt.Errorf("storage: precedent fetch: %w", err)
		}
		if err == nil {
			result.PrecededBy = &entry
		}
	}

	// Fetch decisions that cite this one as their precedent, most recent first.
	rows, err := db.pool.Query(ctx,
		`SELECT `+lineageCols+` FROM decisions
		 WHERE precedent_ref = $1 AND org_id = $2 AND valid_to IS NULL
		 ORDER BY created_at DESC
		 LIMIT $3`,
		id, orgID, limit+1)
	if err != nil {
		return result, fmt.Errorf("storage: cited_by query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		entry, err := scanLineageEntry(rows)
		if err != nil {
			return result, err
		}
		result.CitedBy = append(result.CitedBy, entry)
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("storage: cited_by rows: %w", err)
	}

	// Trim to limit and set has_more flag.
	if len(result.CitedBy) > limit {
		result.CitedBy = result.CitedBy[:limit]
		result.CitedByMore = true
	}

	return result, nil
}

// GetDecisionLineageBatch fetches lineage for multiple decisions in three queries
// instead of 3*N queries. Returns a map keyed by decision ID.
func (db *DB) GetDecisionLineageBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID, citedByLimit int) (map[uuid.UUID]DecisionLineage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if citedByLimit <= 0 {
		citedByLimit = 20
	}

	result := make(map[uuid.UUID]DecisionLineage, len(ids))
	for _, id := range ids {
		result[id] = DecisionLineage{DecisionID: id, CitedBy: []LineageEntry{}}
	}

	// Step 1: Fetch precedent_ref for all target decisions.
	precRows, err := db.pool.Query(ctx,
		`SELECT id, precedent_ref FROM decisions WHERE id = ANY($1) AND org_id = $2`,
		ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: batch lineage precedent_ref: %w", err)
	}
	defer precRows.Close()

	var precedentRefs []uuid.UUID
	precMap := make(map[uuid.UUID]uuid.UUID) // decision_id -> precedent_ref
	for precRows.Next() {
		var id uuid.UUID
		var ref *uuid.UUID
		if err := precRows.Scan(&id, &ref); err != nil {
			return nil, fmt.Errorf("storage: scan precedent_ref: %w", err)
		}
		if ref != nil {
			precMap[id] = *ref
			precedentRefs = append(precedentRefs, *ref)
		}
	}
	if err := precRows.Err(); err != nil {
		return nil, fmt.Errorf("storage: precedent_ref rows: %w", err)
	}

	// Step 2: Fetch the precedent decisions in bulk.
	if len(precedentRefs) > 0 {
		precDecRows, err := db.pool.Query(ctx,
			`SELECT `+lineageCols+` FROM decisions WHERE id = ANY($1) AND org_id = $2`,
			precedentRefs, orgID)
		if err != nil {
			return nil, fmt.Errorf("storage: batch lineage precedent fetch: %w", err)
		}
		defer precDecRows.Close()

		precEntries := make(map[uuid.UUID]LineageEntry)
		for precDecRows.Next() {
			entry, err := scanLineageEntry(precDecRows)
			if err != nil {
				return nil, err
			}
			precEntries[entry.ID] = entry
		}
		if err := precDecRows.Err(); err != nil {
			return nil, fmt.Errorf("storage: batch lineage precedent rows: %w", err)
		}

		for decID, ref := range precMap {
			if entry, ok := precEntries[ref]; ok {
				entryCopy := entry
				l := result[decID]
				l.PrecededBy = &entryCopy
				result[decID] = l
			}
		}
	}

	// Step 3: Fetch cited_by for all target decisions in one query.
	// Use ROW_NUMBER() to cap per-decision so we don't blow up memory.
	citedRows, err := db.pool.Query(ctx,
		`SELECT precedent_ref, `+lineageCols+`
		 FROM (
		   SELECT precedent_ref, `+lineageCols+`,
		          ROW_NUMBER() OVER (PARTITION BY precedent_ref ORDER BY created_at DESC) AS rn
		   FROM decisions
		   WHERE precedent_ref = ANY($1) AND org_id = $2 AND valid_to IS NULL
		 ) sub
		 WHERE rn <= $3`,
		ids, orgID, citedByLimit+1)
	if err != nil {
		return nil, fmt.Errorf("storage: batch lineage cited_by: %w", err)
	}
	defer citedRows.Close()

	for citedRows.Next() {
		var precRef uuid.UUID
		var entry LineageEntry
		if err := citedRows.Scan(
			&precRef,
			&entry.ID, &entry.RunID, &entry.AgentID, &entry.DecisionType, &entry.Outcome,
			&entry.Confidence, &entry.Project, &entry.CreatedAt, &entry.ValidFrom, &entry.ValidTo,
		); err != nil {
			return nil, fmt.Errorf("storage: scan cited_by: %w", err)
		}
		l := result[precRef]
		l.CitedBy = append(l.CitedBy, entry)
		result[precRef] = l
	}
	if err := citedRows.Err(); err != nil {
		return nil, fmt.Errorf("storage: batch lineage cited_by rows: %w", err)
	}

	// Trim to limit and set has_more flag.
	for id, l := range result {
		if len(l.CitedBy) > citedByLimit {
			l.CitedBy = l.CitedBy[:citedByLimit]
			l.CitedByMore = true
			result[id] = l
		}
	}

	return result, nil
}
