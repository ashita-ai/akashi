//go:build !lite

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

// CreateTraceTx creates a run, decision, alternatives, evidence, and completes
// the run atomically within a single database transaction. This prevents partial
// writes that could leave orphaned runs or decisions without their related data.
func (db *DB) CreateTraceTx(ctx context.Context, params CreateTraceParams) (model.AgentRun, model.Decision, error) {
	var run model.AgentRun
	var d model.Decision
	err := db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var txErr error
		run, d, txErr = db.createTraceInTx(ctx, tx, params)
		return txErr
	})
	if err != nil {
		return model.AgentRun{}, model.Decision{}, err
	}
	return run, d, nil
}

// CreateTraceAndAdjudicateConflictTx creates a decision trace AND adjudicates a
// conflict in a single atomic transaction. This prevents the failure mode where
// an adjudication decision exists but the conflict remains unresolved.
func (db *DB) CreateTraceAndAdjudicateConflictTx(ctx context.Context, traceParams CreateTraceParams, conflictParams AdjudicateConflictInTraceParams) (model.AgentRun, model.Decision, error) {
	var run model.AgentRun
	var d model.Decision
	supersedesIDs := uniqueUUIDs(conflictParams.SupersedesIDs)
	if len(supersedesIDs) > 0 {
		if traceParams.Decision.SupersedesID == nil {
			primary := supersedesIDs[0]
			traceParams.Decision.SupersedesID = &primary
		} else if !uuidInSlice(*traceParams.Decision.SupersedesID, supersedesIDs) {
			return model.AgentRun{}, model.Decision{}, ErrSupersededDecisionNotInConflict
		}
	}
	err := db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if len(supersedesIDs) > 0 {
			if err := validateAdjudicationSupersedes(ctx, tx, traceParams.OrgID, conflictParams.ConflictID, supersedesIDs); err != nil {
				return err
			}
		}

		var txErr error
		run, d, txErr = db.createTraceInTx(ctx, tx, traceParams)
		if txErr != nil {
			return txErr
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
			return fmt.Errorf("storage: adjudicate conflict in trace tx: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("storage: conflict: %w", ErrNotFound)
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
			return fmt.Errorf("storage: audit in trace+adjudicate tx: %w", err)
		}
		if len(supersedesIDs) > 0 {
			relationship := conflictParams.Relationship
			if relationship == "" {
				relationship = "supersedes"
			}
			if err := insertDecisionSupersedesRowsTx(ctx, tx, traceParams.OrgID, d.ID, supersedesIDs, traceParams.Decision.SupersedesID, relationship); err != nil {
				return err
			}
			for _, supersededID := range supersedesIDs {
				if traceParams.Decision.SupersedesID != nil && supersededID == *traceParams.Decision.SupersedesID {
					continue
				}
				if err := invalidateSupersededDecisionInTraceTx(ctx, tx, run.ID, traceParams.OrgID, traceParams.AgentID, supersededID, d.ID, d.ValidFrom); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return model.AgentRun{}, model.Decision{}, err
	}
	return run, d, nil
}

func uniqueUUIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func uuidInSlice(needle uuid.UUID, haystack []uuid.UUID) bool {
	for _, id := range haystack {
		if id == needle {
			return true
		}
	}
	return false
}

func validateAdjudicationSupersedes(ctx context.Context, tx pgx.Tx, orgID, conflictID uuid.UUID, supersedesIDs []uuid.UUID) error {
	var decisionAID, decisionBID uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT decision_a_id, decision_b_id FROM scored_conflicts WHERE id = $1 AND org_id = $2 FOR UPDATE`,
		conflictID, orgID,
	).Scan(&decisionAID, &decisionBID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("storage: load conflict for adjudication supersedes: %w", err)
		}
		return fmt.Errorf("storage: conflict: %w", ErrNotFound)
	}
	for _, id := range supersedesIDs {
		if id != decisionAID && id != decisionBID {
			return ErrSupersededDecisionNotInConflict
		}
	}
	return nil
}

func insertDecisionSupersedesRowsTx(ctx context.Context, tx pgx.Tx, orgID, supersedingID uuid.UUID, supersededIDs []uuid.UUID, primaryID *uuid.UUID, relationship string) error {
	for _, supersededID := range supersededIDs {
		isPrimary := primaryID != nil && supersededID == *primaryID
		if isPrimary {
			if _, err := tx.Exec(ctx,
				`UPDATE decision_supersedes SET is_primary = FALSE
				 WHERE superseding_id = $1 AND org_id = $2 AND superseded_id != $3 AND is_primary`,
				supersedingID, orgID, supersededID,
			); err != nil {
				return fmt.Errorf("storage: clear primary supersedes row: %w", err)
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO decision_supersedes (superseding_id, superseded_id, org_id, relationship, is_primary)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (superseding_id, superseded_id) DO UPDATE
			 SET relationship = EXCLUDED.relationship,
			     is_primary = EXCLUDED.is_primary`,
			supersedingID, supersededID, orgID, relationship, isPrimary,
		); err != nil {
			return fmt.Errorf("storage: insert decision supersedes row: %w", err)
		}
	}
	return nil
}

func invalidateSupersededDecisionInTraceTx(ctx context.Context, tx pgx.Tx, runID, orgID uuid.UUID, agentID string, supersededID, newDecisionID uuid.UUID, validTo time.Time) error {
	if validTo.IsZero() {
		validTo = time.Now().UTC()
	}
	tag, err := tx.Exec(ctx,
		`UPDATE decisions SET valid_to = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
		validTo, supersededID, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: invalidate superseded decision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: superseded decision %s not found (or already superseded): %w", supersededID, ErrNotFound)
	}
	if err := queueSearchOutbox(ctx, tx, supersededID, orgID, "delete"); err != nil {
		return fmt.Errorf("storage: queue search outbox delete for superseded: %w", err)
	}
	if _, err := AutoResolveSupersededConflictsTx(ctx, tx, orgID, supersededID, newDecisionID); err != nil {
		return fmt.Errorf("storage: auto-resolve superseded conflicts in trace: %w", err)
	}

	var seqNum int64
	if err := tx.QueryRow(ctx, `SELECT nextval('event_sequence_num_seq')`).Scan(&seqNum); err != nil {
		return fmt.Errorf("storage: reserve sequence num for supersession event: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_events (id, run_id, org_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		uuid.New(), runID, orgID, string(model.EventDecisionSuperseded), seqNum,
		validTo, agentID, map[string]any{
			"superseded_decision_id": supersededID.String(),
			"new_decision_id":        newDecisionID.String(),
		}, validTo,
	); err != nil {
		return fmt.Errorf("storage: insert supersession event: %w", err)
	}
	if err := InsertMutationAuditTx(ctx, tx, MutationAuditEntry{
		OrgID:        orgID,
		ActorAgentID: agentID,
		ActorRole:    "agent",
		Operation:    "supersede_decision",
		ResourceType: "decision",
		ResourceID:   supersededID.String(),
		BeforeData:   map[string]any{"valid_to": nil},
		AfterData: map[string]any{
			"valid_to":        validTo,
			"superseded_by":   newDecisionID,
			"new_decision_id": newDecisionID,
			"superseded_id":   supersededID,
		},
	}); err != nil {
		return fmt.Errorf("storage: audit supersession in trace tx: %w", err)
	}
	return nil
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
		 reasoning, embedding, outcome_embedding, metadata, completeness_score, outcome_score, precedent_ref, precedent_reason, supersedes_id, content_hash,
		 valid_from, valid_to, transaction_time, created_at, session_id, agent_context, api_key_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`,
		d.ID, d.RunID, d.AgentID, d.OrgID, d.DecisionType, d.Outcome, d.Confidence,
		d.Reasoning, d.Embedding, d.OutcomeEmbedding, d.Metadata, d.CompletenessScore, d.OutcomeScore, d.PrecedentRef,
		d.PrecedentReason, d.SupersedesID, d.ContentHash,
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
		columns := []string{"id", "decision_id", "label", "rejection_reason", "metadata", "created_at"}
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
			rows[i] = []any{id, d.ID, a.Label, a.RejectionReason, meta, createdAt}
		}
		copyCtx, copyCancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := tx.CopyFrom(copyCtx, pgx.Identifier{"alternatives"}, columns, pgx.CopyFromRows(rows))
		copyCancel()
		if err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: create alternatives in trace tx: %w", err)
		}
	}

	// Bindings go in the same transaction as the decision that declares them.
	// A binding that outlived a rolled-back trace would claim a parameter no
	// decision set, and would then conflict with real decisions forever.
	if len(params.Bindings) > 0 {
		columns := []string{"id", "decision_id", "org_id", "parameter", "parameter_key", "value", "value_key", "created_at"}
		rows := make([][]any, len(params.Bindings))
		for i, b := range params.Bindings {
			if b.ParameterKey == "" || b.ValueKey == "" {
				return model.AgentRun{}, model.Decision{}, fmt.Errorf(
					"storage: binding %q has no canonical key; call model.CanonicalizeBindings first", b.Parameter)
			}
			id := b.ID
			if id == uuid.Nil {
				id = uuid.New()
			}
			rows[i] = []any{id, d.ID, params.OrgID, b.Parameter, b.ParameterKey, b.Value, b.ValueKey, now}
		}
		copyCtx, copyCancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := tx.CopyFrom(copyCtx, pgx.Identifier{"decision_bindings"}, columns, pgx.CopyFromRows(rows))
		copyCancel()
		if err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: create bindings in trace tx: %w", err)
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
	if err := queueSearchOutbox(ctx, tx, d.ID, params.OrgID, "upsert"); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: queue search outbox in trace tx: %w", err)
	}

	// 4c. Handle explicit supersession: invalidate the superseded decision and
	// auto-resolve its open conflicts, matching the ReviseDecision pattern.
	if d.SupersedesID != nil {
		if err := insertDecisionSupersedesRowsTx(ctx, tx, params.OrgID, d.ID, []uuid.UUID{*d.SupersedesID}, d.SupersedesID, "supersedes"); err != nil {
			return model.AgentRun{}, model.Decision{}, err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE decisions SET valid_to = $1 WHERE id = $2 AND org_id = $3 AND valid_to IS NULL`,
			now, *d.SupersedesID, params.OrgID,
		)
		if err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: invalidate superseded decision: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: superseded decision %s not found (or already superseded): %w", *d.SupersedesID, ErrNotFound)
		}
		// Queue search index deletion for the superseded decision.
		if err := queueSearchOutbox(ctx, tx, *d.SupersedesID, params.OrgID, "delete"); err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: queue search outbox delete for superseded: %w", err)
		}
		// Auto-resolve open conflicts involving the superseded decision.
		if _, err := AutoResolveSupersededConflictsTx(ctx, tx, params.OrgID, *d.SupersedesID, d.ID); err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: auto-resolve superseded conflicts in trace: %w", err)
		}
		// Emit DecisionSuperseded event into the event stream (matching the
		// retraction pattern) so SSE consumers and event queries see it.
		var supersessionSeqNum int64
		if err := tx.QueryRow(ctx, `SELECT nextval('event_sequence_num_seq')`).Scan(&supersessionSeqNum); err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: reserve sequence num for supersession event: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_events (id, run_id, org_id, event_type, sequence_num, occurred_at, agent_id, payload, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			uuid.New(), run.ID, params.OrgID, string(model.EventDecisionSuperseded), supersessionSeqNum,
			now, params.AgentID, map[string]any{
				"superseded_decision_id": d.SupersedesID.String(),
				"new_decision_id":        d.ID.String(),
			}, now,
		); err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: insert supersession event: %w", err)
		}
		// Record supersession in the mutation audit log so the paper trail
		// captures who replaced what, atomically with the invalidation.
		if err := InsertMutationAuditTx(ctx, tx, MutationAuditEntry{
			OrgID:        params.OrgID,
			ActorAgentID: params.AgentID,
			ActorRole:    "agent",
			Operation:    "supersede_decision",
			ResourceType: "decision",
			ResourceID:   d.SupersedesID.String(),
			BeforeData:   map[string]any{"valid_to": nil},
			AfterData: map[string]any{
				"valid_to":        now,
				"superseded_by":   d.ID,
				"new_decision_id": d.ID,
				"superseded_id":   *d.SupersedesID,
			},
		}); err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: audit supersession in trace tx: %w", err)
		}
	}

	// 5. Complete run.
	if _, err := tx.Exec(ctx,
		`UPDATE agent_runs SET status = $1, completed_at = $2 WHERE id = $3 AND org_id = $4`,
		string(model.RunStatusCompleted), now, run.ID, params.OrgID,
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
