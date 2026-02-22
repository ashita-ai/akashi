package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/integrity"
	"github.com/ashita-ai/akashi/internal/model"
)

// CreateTraceParams holds all data needed to create a complete decision trace
// within a single database transaction.
type CreateTraceParams struct {
	AgentID      string
	OrgID        uuid.UUID
	TraceID      *string
	Metadata     map[string]any
	Decision     model.Decision
	Alternatives []model.Alternative
	Evidence     []model.Evidence
	SessionID    *uuid.UUID
	AgentContext map[string]any

	// AuditEntry, when non-nil, is inserted into mutation_audit_log inside the
	// same transaction. ResourceID is populated automatically from the generated
	// decision ID. This ensures the audit record is atomic with the trace —
	// if the tx rolls back, the audit entry never persists.
	AuditEntry *MutationAuditEntry
}

// CreateTraceTx creates a run, decision, alternatives, evidence, and completes
// the run atomically within a single database transaction. This prevents partial
// writes that could leave orphaned runs or decisions without their related data.
func (db *DB) CreateTraceTx(ctx context.Context, params CreateTraceParams) (model.AgentRun, model.Decision, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: begin trace tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, d, err := db.createTraceInTx(ctx, tx, params)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: commit trace tx: %w", err)
	}
	return run, d, nil
}

// AdjudicateConflictInTraceParams holds data needed for the conflict adjudication
// that should be committed atomically with the trace.
type AdjudicateConflictInTraceParams struct {
	ConflictID        uuid.UUID
	ResolvedBy        string
	ResNote           *string
	Audit             MutationAuditEntry
	WinningDecisionID *uuid.UUID // optional; must be decision_a_id or decision_b_id if set
}

// CreateTraceAndAdjudicateConflictTx creates a decision trace AND adjudicates a
// conflict in a single atomic transaction. This prevents the failure mode where
// an adjudication decision exists but the conflict remains unresolved.
func (db *DB) CreateTraceAndAdjudicateConflictTx(ctx context.Context, traceParams CreateTraceParams, conflictParams AdjudicateConflictInTraceParams) (model.AgentRun, model.Decision, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: begin trace+adjudicate tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run, d, err := db.createTraceInTx(ctx, tx, traceParams)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, err
	}

	// Adjudicate the conflict within the same transaction.
	tag, err := tx.Exec(ctx,
		`UPDATE scored_conflicts SET status = 'resolved', resolved_by = $1, resolved_at = now(),
		 resolution_note = $2, resolution_decision_id = $3, winning_decision_id = $4
		 WHERE id = $5 AND org_id = $6`,
		conflictParams.ResolvedBy, conflictParams.ResNote, d.ID,
		conflictParams.WinningDecisionID,
		conflictParams.ConflictID, traceParams.OrgID)
	if err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: adjudicate conflict in trace tx: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: conflict not found")
	}

	// Insert conflict adjudication audit entry.
	conflictParams.Audit.ResourceID = conflictParams.ConflictID.String()
	afterData := map[string]any{
		"status":                 "resolved",
		"resolved_by":            conflictParams.ResolvedBy,
		"resolution_decision_id": d.ID.String(),
	}
	if conflictParams.WinningDecisionID != nil {
		afterData["winning_decision_id"] = conflictParams.WinningDecisionID.String()
	}
	conflictParams.Audit.AfterData = afterData
	if err := InsertMutationAuditTx(ctx, tx, conflictParams.Audit); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: audit in trace+adjudicate tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: commit trace+adjudicate tx: %w", err)
	}
	return run, d, nil
}

// createTraceInTx is the transactional core shared by CreateTraceTx and
// CreateTraceAndAdjudicateConflictTx. It creates the run, decision, alternatives,
// evidence, outbox entry, and audit within the provided transaction. The caller
// manages Begin/Commit/Rollback.
func (db *DB) createTraceInTx(ctx context.Context, tx pgx.Tx, params CreateTraceParams) (model.AgentRun, model.Decision, error) {
	now := time.Now().UTC()

	// 1. Create run.
	run := model.AgentRun{
		ID:        uuid.New(),
		AgentID:   params.AgentID,
		OrgID:     params.OrgID,
		TraceID:   params.TraceID,
		Status:    model.RunStatusRunning,
		StartedAt: now,
		Metadata:  params.Metadata,
		CreatedAt: now,
	}
	if run.Metadata == nil {
		run.Metadata = map[string]any{}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_runs (id, agent_id, org_id, trace_id, parent_run_id, status, started_at, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		run.ID, run.AgentID, run.OrgID, run.TraceID, nil,
		string(run.Status), run.StartedAt, run.Metadata, run.CreatedAt,
	); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: create run in trace tx: %w", err)
	}

	// 2. Create decision.
	d := params.Decision
	d.ID = uuid.New()
	d.RunID = run.ID
	d.AgentID = params.AgentID
	d.OrgID = params.OrgID
	d.SessionID = params.SessionID
	if params.AgentContext != nil {
		d.AgentContext = params.AgentContext
	}
	if d.AgentContext == nil {
		d.AgentContext = map[string]any{}
	}
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
	d.ContentHash = integrity.ComputeContentHash(d.ID, d.DecisionType, d.Outcome, d.Confidence, d.Reasoning, d.ValidFrom)
	if _, err := tx.Exec(ctx,
		`INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome, confidence,
		 reasoning, embedding, outcome_embedding, metadata, completeness_score, precedent_ref, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
		d.ID, d.RunID, d.AgentID, d.OrgID, d.DecisionType, d.Outcome, d.Confidence,
		d.Reasoning, d.Embedding, d.OutcomeEmbedding, d.Metadata, d.CompletenessScore, d.PrecedentRef,
		d.SupersedesID, d.ContentHash,
		d.ValidFrom, d.ValidTo, d.TransactionTime, d.CreatedAt,
		d.SessionID, d.AgentContext, d.APIKeyID,
	); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: create decision in trace tx: %w", err)
	}

	// 3. Create alternatives via COPY.
	// COPY operations get a dedicated 30-second timeout to prevent a hung Postgres
	// from blocking the transaction indefinitely. The parent request context may
	// have a longer deadline (WriteTimeout), but COPY should not consume it all.
	if len(params.Alternatives) > 0 {
		columns := []string{"id", "decision_id", "label", "score", "selected", "rejection_reason", "metadata", "created_at"}
		rows := make([][]any, len(params.Alternatives))
		for i, a := range params.Alternatives {
			id := a.ID
			if id == uuid.Nil {
				id = uuid.New()
			}
			createdAt := a.CreatedAt
			if createdAt.IsZero() {
				createdAt = now
			}
			meta := a.Metadata
			if meta == nil {
				meta = map[string]any{}
			}
			rows[i] = []any{id, d.ID, a.Label, a.Score, a.Selected, a.RejectionReason, meta, createdAt}
		}
		copyCtx, copyCancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := tx.CopyFrom(copyCtx, pgx.Identifier{"alternatives"}, columns, pgx.CopyFromRows(rows))
		copyCancel()
		if err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: create alternatives in trace tx: %w", err)
		}
	}

	// 4. Create evidence via COPY.
	if len(params.Evidence) > 0 {
		columns := []string{"id", "decision_id", "org_id", "source_type", "source_uri", "content",
			"relevance_score", "embedding", "metadata", "created_at"}
		rows := make([][]any, len(params.Evidence))
		for i, ev := range params.Evidence {
			id := ev.ID
			if id == uuid.Nil {
				id = uuid.New()
			}
			createdAt := ev.CreatedAt
			if createdAt.IsZero() {
				createdAt = now
			}
			meta := ev.Metadata
			if meta == nil {
				meta = map[string]any{}
			}
			rows[i] = []any{id, d.ID, params.OrgID, string(ev.SourceType), ev.SourceURI, ev.Content,
				ev.RelevanceScore, ev.Embedding, meta, createdAt}
		}
		copyCtx, copyCancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := tx.CopyFrom(copyCtx, pgx.Identifier{"evidence"}, columns, pgx.CopyFromRows(rows))
		copyCancel()
		if err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: create evidence in trace tx: %w", err)
		}
	}

	// 4b. Queue search index update (inside same tx — if decision commits, outbox commits).
	// Always queue regardless of embedding status — the outbox worker defers entries
	// whose decisions lack embeddings until a backfill provides one (issue #60).
	if _, err := tx.Exec(ctx,
		`INSERT INTO search_outbox (decision_id, org_id, operation)
		 VALUES ($1, $2, 'upsert')
		 ON CONFLICT (decision_id, operation) DO UPDATE SET created_at = now(), attempts = 0, locked_until = NULL`,
		d.ID, params.OrgID); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: queue search outbox in trace tx: %w", err)
	}

	// 5. Complete run.
	if _, err := tx.Exec(ctx,
		`UPDATE agent_runs SET status = $1, completed_at = $2 WHERE id = $3`,
		string(model.RunStatusCompleted), now, run.ID,
	); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: complete run in trace tx: %w", err)
	}
	run.Status = model.RunStatusCompleted
	run.CompletedAt = &now

	// 6. Insert mutation audit (same tx — atomic with the trace).
	if params.AuditEntry != nil {
		params.AuditEntry.ResourceID = d.ID.String()
		params.AuditEntry.AfterData = map[string]any{
			"run_id":      run.ID,
			"decision_id": d.ID,
			"event_count": len(params.Alternatives) + len(params.Evidence) + 1,
		}
		if err := InsertMutationAuditTx(ctx, tx, *params.AuditEntry); err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: audit in trace tx: %w", err)
		}
	}

	return run, d, nil
}
