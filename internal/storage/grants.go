package storage

import (
	"context"
	"errors"
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

// CreateGrantWithAudit inserts a new access grant and a mutation audit entry
// atomically within a single transaction.
func (db *DB) CreateGrantWithAudit(ctx context.Context, grant model.AccessGrant, audit MutationAuditEntry) (model.AccessGrant, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.AccessGrant{}, fmt.Errorf("storage: begin create grant tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if grant.ID == uuid.Nil {
		grant.ID = uuid.New()
	}
	if grant.GrantedAt.IsZero() {
		grant.GrantedAt = time.Now().UTC()
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO access_grants (id, org_id, grantor_id, grantee_id, resource_type, resource_id,
		 permission, granted_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		grant.ID, grant.OrgID, grant.GrantorID, grant.GranteeID, grant.ResourceType,
		grant.ResourceID, grant.Permission, grant.GrantedAt, grant.ExpiresAt,
	); err != nil {
		return model.AccessGrant{}, fmt.Errorf("storage: create grant: %w", err)
	}

	audit.ResourceID = grant.ID.String()
	audit.AfterData = grant
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return model.AccessGrant{}, fmt.Errorf("storage: audit in create grant tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.AccessGrant{}, fmt.Errorf("storage: commit create grant tx: %w", err)
	}
	return grant, nil
}

// DeleteGrant removes an access grant by ID, scoped to an org for tenant isolation.
func (db *DB) DeleteGrant(ctx context.Context, orgID, id uuid.UUID) error {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM access_grants WHERE id = $1 AND org_id = $2`, id, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: delete grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: grant %s: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteGrantWithAudit removes an access grant and inserts a mutation audit entry
// atomically within a single transaction.
func (db *DB) DeleteGrantWithAudit(ctx context.Context, orgID, id uuid.UUID, audit MutationAuditEntry) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin delete grant tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`DELETE FROM access_grants WHERE id = $1 AND org_id = $2`, id, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: delete grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: grant %s: %w", id, ErrNotFound)
	}

	audit.ResourceID = id.String()
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return fmt.Errorf("storage: audit in delete grant tx: %w", err)
	}

	return tx.Commit(ctx)
}

// GetGrant retrieves a grant by ID, scoped to an org for defense-in-depth
// tenant isolation. Even though the handler performs its own authz check,
// the storage layer must enforce org boundaries on every query.
func (db *DB) GetGrant(ctx context.Context, orgID uuid.UUID, id uuid.UUID) (model.AccessGrant, error) {
	var g model.AccessGrant
	err := db.pool.QueryRow(ctx,
		`SELECT id, org_id, grantor_id, grantee_id, resource_type, resource_id,
		 permission, granted_at, expires_at
		 FROM access_grants WHERE id = $1 AND org_id = $2`, id, orgID,
	).Scan(
		&g.ID, &g.OrgID, &g.GrantorID, &g.GranteeID, &g.ResourceType, &g.ResourceID,
		&g.Permission, &g.GrantedAt, &g.ExpiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.AccessGrant{}, fmt.Errorf("storage: grant %s: %w", id, ErrNotFound)
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

// ListGrantedAgentIDs returns the set of agent_ids that the grantee has been
// granted read access to (via agent_traces grants) within the given org.
// The caller's own agent_id is always included.
func (db *DB) ListGrantedAgentIDs(ctx context.Context, orgID uuid.UUID, granteeID uuid.UUID, selfAgentID string) (map[string]bool, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT DISTINCT resource_id FROM access_grants
		 WHERE org_id = $1 AND grantee_id = $2
		 AND resource_type = 'agent_traces' AND permission = 'read'
		 AND (expires_at IS NULL OR expires_at > now())
		 AND resource_id IS NOT NULL`,
		orgID, granteeID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list granted agent ids: %w", err)
	}
	defer rows.Close()

	granted := map[string]bool{selfAgentID: true}
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			return nil, fmt.Errorf("storage: scan granted agent id: %w", err)
		}
		granted[agentID] = true
	}
	return granted, rows.Err()
}

// ListGrants returns all grants within an org, ordered by granted_at descending.
// Includes both active and expired grants so admins have full visibility.
func (db *DB) ListGrants(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]model.AccessGrant, int, error) {
	var total int
	err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM access_grants WHERE org_id = $1`, orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: count grants: %w", err)
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, org_id, grantor_id, grantee_id, resource_type, resource_id,
		 permission, granted_at, expires_at
		 FROM access_grants
		 WHERE org_id = $1
		 ORDER BY granted_at DESC
		 LIMIT $2 OFFSET $3`, orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: list grants: %w", err)
	}
	defer rows.Close()

	var grants []model.AccessGrant
	for rows.Next() {
		var g model.AccessGrant
		if err := rows.Scan(
			&g.ID, &g.OrgID, &g.GrantorID, &g.GranteeID, &g.ResourceType, &g.ResourceID,
			&g.Permission, &g.GrantedAt, &g.ExpiresAt,
		); err != nil {
			return nil, 0, fmt.Errorf("storage: scan grant: %w", err)
		}
		grants = append(grants, g)
	}
	return grants, total, rows.Err()
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
