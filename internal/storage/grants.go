package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateGrant inserts a new access grant.
func (db *DB) CreateGrant(ctx context.Context, grant model.AccessGrant) (model.AccessGrant, error) {
	if grant.ID == uuid.Nil {
		grant.ID = uuid.New()
	}
	if grant.GrantedAt.IsZero() {
		grant.GrantedAt = time.Now().UTC()
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO access_grants (id, org_id, grantor_id, grantee_id, resource_type, resource_id,
		 permission, granted_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		grant.ID, grant.OrgID, grant.GrantorID, grant.GranteeID, grant.ResourceType,
		grant.ResourceID, grant.Permission, grant.GrantedAt, grant.ExpiresAt,
	)
	if err != nil {
		return model.AccessGrant{}, fmt.Errorf("storage: create grant: %w", err)
	}
	return grant, nil
}

// DeleteGrant removes an access grant by ID.
func (db *DB) DeleteGrant(ctx context.Context, id uuid.UUID) error {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM access_grants WHERE id = $1`, id,
	)
	if err != nil {
		return fmt.Errorf("storage: delete grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: grant not found: %s", id)
	}
	return nil
}

// GetGrant retrieves a grant by ID.
func (db *DB) GetGrant(ctx context.Context, id uuid.UUID) (model.AccessGrant, error) {
	var g model.AccessGrant
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, grantor_id, grantee_id, resource_type, resource_id,
		 permission, granted_at, expires_at
		 FROM access_grants WHERE id = $1`, id,
	).Scan(
		&g.ID, &g.OrgID, &g.GrantorID, &g.GranteeID, &g.ResourceType, &g.ResourceID,
		&g.Permission, &g.GrantedAt, &g.ExpiresAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return model.AccessGrant{}, fmt.Errorf("storage: grant not found: %s", id)
		}
		return model.AccessGrant{}, fmt.Errorf("storage: get grant: %w", err)
	}
	return g, nil
}

// HasAccess checks whether a grantee has the specified permission on a resource within an org.
// Returns true if a valid (non-expired) grant exists.
func (db *DB) HasAccess(ctx context.Context, orgID uuid.UUID, granteeID uuid.UUID, resourceType, resourceID, permission string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM access_grants
			WHERE org_id = $1
			AND grantee_id = $2
			AND resource_type = $3
			AND (resource_id = $4 OR resource_id IS NULL)
			AND permission = $5
			AND (expires_at IS NULL OR expires_at > now())
		)`,
		orgID, granteeID, resourceType, resourceID, permission,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: check access: %w", err)
	}
	return exists, nil
}

// ListGrantsByGrantee returns all active grants for a grantee within an org.
func (db *DB) ListGrantsByGrantee(ctx context.Context, orgID uuid.UUID, granteeID uuid.UUID) ([]model.AccessGrant, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, grantor_id, grantee_id, resource_type, resource_id,
		 permission, granted_at, expires_at
		 FROM access_grants
		 WHERE org_id = $1 AND grantee_id = $2 AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY granted_at DESC`, orgID, granteeID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list grants: %w", err)
	}
	defer rows.Close()

	var grants []model.AccessGrant
	for rows.Next() {
		var g model.AccessGrant
		if err := rows.Scan(
			&g.ID, &g.OrgID, &g.GrantorID, &g.GranteeID, &g.ResourceType, &g.ResourceID,
			&g.Permission, &g.GrantedAt, &g.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan grant: %w", err)
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}
