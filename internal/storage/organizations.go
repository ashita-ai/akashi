package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// EnsureDefaultOrg idempotently creates the default organization (uuid.Nil).
// Used by SeedAdmin to guarantee the FK target exists on a fresh database
// before inserting the admin agent.
func (db *DB) EnsureDefaultOrg(ctx context.Context) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug, plan, created_at, updated_at)
		 VALUES ($1, 'Default', 'default', 'enterprise', NOW(), NOW())
		 ON CONFLICT (id) DO NOTHING`,
		uuid.Nil,
	)
	if err != nil {
		return fmt.Errorf("storage: ensure default org: %w", err)
	}
	return nil
}

// GetOrganization retrieves an org by ID.
func (db *DB) GetOrganization(ctx context.Context, id uuid.UUID) (model.Organization, error) {
	var org model.Organization
	err := db.pool.QueryRow(ctx,
		`SELECT id, name, slug, plan, created_at, updated_at
		 FROM organizations WHERE id = $1`, id,
	).Scan(
		&org.ID, &org.Name, &org.Slug, &org.Plan, &org.CreatedAt, &org.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Organization{}, fmt.Errorf("storage: organization not found: %s", id)
		}
		return model.Organization{}, fmt.Errorf("storage: get organization: %w", err)
	}
	return org, nil
}
