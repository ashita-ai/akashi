//go:build !lite

package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ResolveDecisionTypeAlias returns the canonical decision type for the given
// alias. Returns "" if no alias mapping exists for this org.
func (db *DB) ResolveDecisionTypeAlias(ctx context.Context, orgID uuid.UUID, alias string) (string, error) {
	var canonical string
	err := db.pool.QueryRow(ctx,
		`SELECT canonical FROM decision_type_aliases WHERE org_id = $1 AND alias = $2`,
		orgID, alias,
	).Scan(&canonical)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("storage: resolve decision type alias: %w", err)
	}
	return canonical, nil
}

// CreateDecisionTypeAlias upserts an alias→canonical mapping for decision types.
// Idempotent: if the alias already exists, updates the canonical target.
func (db *DB) CreateDecisionTypeAlias(ctx context.Context, orgID uuid.UUID, alias, canonical, createdBy string) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO decision_type_aliases (org_id, alias, canonical, created_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (org_id, alias) DO UPDATE SET canonical = EXCLUDED.canonical`,
		orgID, alias, canonical, createdBy,
	)
	if err != nil {
		return fmt.Errorf("storage: create decision type alias: %w", err)
	}
	return nil
}
