//go:build !lite

package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateBindingsBatch writes a decision's bindings.
//
// Callers must have run model.CanonicalizeBindings first: this writes
// ParameterKey and ValueKey as given, and an unpopulated key would be stored
// blank and never join with anything, which fails silently.
func (db *DB) CreateBindingsBatch(ctx context.Context, bindings []model.Binding) error {
	if len(bindings) == 0 {
		return nil
	}
	columns := []string{"id", "decision_id", "org_id", "parameter", "parameter_key", "value", "value_key", "created_at"}
	rows := make([][]any, len(bindings))
	now := time.Now().UTC()
	for i, b := range bindings {
		if b.ParameterKey == "" || b.ValueKey == "" {
			return fmt.Errorf("storage: binding %q has no canonical key; call model.CanonicalizeBindings first", b.Parameter)
		}
		id := b.ID
		if id == uuid.Nil {
			id = uuid.New()
		}
		rows[i] = []any{id, b.DecisionID, b.OrgID, b.Parameter, b.ParameterKey, b.Value, b.ValueKey, now}
	}
	if _, err := db.pool.CopyFrom(ctx, pgx.Identifier{"decision_bindings"}, columns, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("storage: copy decision bindings: %w", err)
	}
	return nil
}

// ConflictingBinding is one prior decision that bound the same parameter to a
// different value.
type ConflictingBinding struct {
	Parameter       string
	Value           string
	OtherDecisionID uuid.UUID
	OtherValue      string
	OtherAgentID    string
	OtherProject    *string
}

// FindConflictingBindings returns prior decisions that bind any of this
// decision's parameters to a different value.
//
// This is the exact half of conflict detection: no model, no threshold, no
// false-positive rate. The scoping conditions are what keep it exact rather
// than merely cheap.
//
//   - Same org. Every query in this codebase is org-scoped.
//   - Same project. Two services can legitimately set a parameter of the same
//     name to different values; cross-project collision was the leading source
//     of false positives before projects were recorded. A decision with no
//     project only matches other decisions with no project.
//   - Current state only (valid_to IS NULL). A superseded decision no longer
//     binds anything, so it must not conflict with its own replacement.
//   - Different decision. A decision cannot conflict with itself, and
//     re-tracing an identical binding must be a no-op.
//
// Branch is deliberately NOT part of the join. Two branches setting the same
// parameter differently is a real conflict that surfaces at merge; suppressing
// it here would hide the case this feature exists to catch. Callers that want
// branch-aware suppression should apply it downstream where the existing
// branch logic lives.
func (db *DB) FindConflictingBindings(
	ctx context.Context,
	decisionID, orgID uuid.UUID,
	bindings []model.Binding,
) ([]ConflictingBinding, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	paramKeys := make([]string, len(bindings))
	valueKeys := make([]string, len(bindings))
	for i, b := range bindings {
		paramKeys[i] = b.ParameterKey
		valueKeys[i] = b.ValueKey
	}

	// unnest pairs each parameter with the value THIS decision binds it to, so
	// one query covers every binding without building a variable-length OR.
	rows, err := db.pool.Query(ctx,
		`WITH mine AS (
		     SELECT unnest($3::text[]) AS parameter_key,
		            unnest($4::text[]) AS value_key
		 )
		 SELECT b.parameter, mine.value_key, b.decision_id, b.value, d.agent_id, d.project
		 FROM decision_bindings b
		 JOIN mine ON mine.parameter_key = b.parameter_key
		 JOIN decisions d ON d.id = b.decision_id
		 WHERE b.org_id = $2
		   AND b.decision_id <> $1
		   AND b.value_key <> mine.value_key
		   AND d.valid_to IS NULL
		   AND d.project IS NOT DISTINCT FROM (
		       SELECT project FROM decisions WHERE id = $1 AND org_id = $2
		   )
		 ORDER BY d.created_at DESC`,
		decisionID, orgID, paramKeys, valueKeys)
	if err != nil {
		return nil, fmt.Errorf("storage: find conflicting bindings: %w", err)
	}
	defer rows.Close()

	var out []ConflictingBinding
	for rows.Next() {
		var c ConflictingBinding
		var myValueKey string
		if err := rows.Scan(&c.Parameter, &myValueKey, &c.OtherDecisionID, &c.OtherValue, &c.OtherAgentID, &c.OtherProject); err != nil {
			return nil, fmt.Errorf("storage: scan conflicting binding: %w", err)
		}
		c.Value = myValueKey
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate conflicting bindings: %w", err)
	}
	return out, nil
}

// GetBindingsByDecision returns a decision's bindings.
func (db *DB) GetBindingsByDecision(ctx context.Context, decisionID, orgID uuid.UUID) ([]model.Binding, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, decision_id, org_id, parameter, parameter_key, value, value_key
		 FROM decision_bindings
		 WHERE decision_id = $1 AND org_id = $2
		 ORDER BY parameter_key`, decisionID, orgID)
	if err != nil {
		return nil, fmt.Errorf("storage: get bindings: %w", err)
	}
	defer rows.Close()

	var out []model.Binding
	for rows.Next() {
		var b model.Binding
		if err := rows.Scan(&b.ID, &b.DecisionID, &b.OrgID, &b.Parameter, &b.ParameterKey, &b.Value, &b.ValueKey); err != nil {
			return nil, fmt.Errorf("storage: scan binding: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
