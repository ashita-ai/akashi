package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	if _, err := tx.Exec(ctx,
		`INSERT INTO decisions (id, run_id, agent_id, org_id, decision_type, outcome, confidence,
		 reasoning, embedding, metadata, quality_score, precedent_ref, valid_from, valid_to, transaction_time, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		d.ID, d.RunID, d.AgentID, d.OrgID, d.DecisionType, d.Outcome, d.Confidence,
		d.Reasoning, d.Embedding, d.Metadata, d.QualityScore, d.PrecedentRef,
		d.ValidFrom, d.ValidTo, d.TransactionTime, d.CreatedAt,
	); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: create decision in trace tx: %w", err)
	}

	// 3. Create alternatives via COPY.
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
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"alternatives"}, columns, pgx.CopyFromRows(rows)); err != nil {
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
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"evidence"}, columns, pgx.CopyFromRows(rows)); err != nil {
			return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: create evidence in trace tx: %w", err)
		}
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

	if err := tx.Commit(ctx); err != nil {
		return model.AgentRun{}, model.Decision{}, fmt.Errorf("storage: commit trace tx: %w", err)
	}
	return run, d, nil
}
