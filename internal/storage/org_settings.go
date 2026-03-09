//go:build !lite

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// GetOrgSettings returns the settings for an org.
// Returns default empty settings if no row exists.
func (db *DB) GetOrgSettings(ctx context.Context, orgID uuid.UUID) (model.OrgSettings, error) {
	var s model.OrgSettings
	s.OrgID = orgID

	var raw []byte
	err := db.pool.QueryRow(ctx,
		`SELECT settings, updated_at, updated_by FROM org_settings WHERE org_id = $1`,
		orgID,
	).Scan(&raw, &s.UpdatedAt, &s.UpdatedBy)
	if err != nil {
		// No row → return empty defaults.
		if errors.Is(err, pgx.ErrNoRows) {
			return s, nil
		}
		return s, fmt.Errorf("storage: get org settings: %w", err)
	}
	if err := json.Unmarshal(raw, &s.Settings); err != nil {
		return s, fmt.Errorf("storage: unmarshal org settings: %w", err)
	}
	return s, nil
}

// UpsertOrgSettings inserts or updates the settings for an org.
func (db *DB) UpsertOrgSettings(ctx context.Context, orgID uuid.UUID, settings model.OrgSettingsData, updatedBy string) error {
	raw, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("storage: marshal org settings: %w", err)
	}
	_, err = db.pool.Exec(ctx,
		`INSERT INTO org_settings (org_id, settings, updated_at, updated_by)
		 VALUES ($1, $2, now(), $3)
		 ON CONFLICT (org_id) DO UPDATE
		 SET settings = $2, updated_at = now(), updated_by = $3`,
		orgID, raw, updatedBy,
	)
	if err != nil {
		return fmt.Errorf("storage: upsert org settings: %w", err)
	}
	return nil
}

// OrgAutoResolveConfig holds the parsed auto-resolution policy for an org.
type OrgAutoResolveConfig struct {
	OrgID  uuid.UUID
	Policy model.ConflictResolutionPolicy
}

// GetOrgsWithAutoResolution returns all orgs that have an auto-resolution policy configured.
func (db *DB) GetOrgsWithAutoResolution(ctx context.Context) ([]OrgAutoResolveConfig, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT org_id, settings
		 FROM org_settings
		 WHERE settings->'conflict_resolution' IS NOT NULL
		   AND settings->'conflict_resolution' != 'null'::jsonb`,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get orgs with auto resolution: %w", err)
	}
	defer rows.Close()

	var out []OrgAutoResolveConfig
	for rows.Next() {
		var c OrgAutoResolveConfig
		var raw []byte
		if err := rows.Scan(&c.OrgID, &raw); err != nil {
			return nil, fmt.Errorf("storage: scan org auto resolve config: %w", err)
		}
		var data model.OrgSettingsData
		if err := json.Unmarshal(raw, &data); err != nil {
			continue // skip malformed rows
		}
		if data.ConflictResolution == nil {
			continue
		}
		c.Policy = *data.ConflictResolution
		out = append(out, c)
	}
	return out, rows.Err()
}

// AutoResolvableConflicts returns open/acknowledged conflicts that have been
// open longer than afterDays and whose severity is eligible for auto-resolution.
func (db *DB) AutoResolvableConflicts(ctx context.Context, orgID uuid.UUID, maxSeverity string, neverSeverities []string, afterDays int) ([]model.DecisionConflict, error) {
	maxRank := model.SeverityRank(maxSeverity)

	query := conflictSelectBase + `
		WHERE sc.org_id = $1
		  AND sc.status IN ('open', 'acknowledged')
		  AND sc.detected_at < now() - ($2 * interval '1 day')
		  AND (
		      sc.severity IS NULL
		      OR CASE sc.severity
		           WHEN 'low' THEN 1
		           WHEN 'medium' THEN 2
		           WHEN 'high' THEN 3
		           WHEN 'critical' THEN 4
		           ELSE 0
		         END <= $3
		  )
		  AND (sc.severity IS NULL OR sc.severity != ALL($4))
		ORDER BY sc.detected_at ASC
		LIMIT 100`

	rows, err := db.pool.Query(ctx, query,
		orgID, afterDays, maxRank, neverSeverities,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: auto resolvable conflicts: %w", err)
	}
	defer rows.Close()

	return scanConflictRows(rows)
}
