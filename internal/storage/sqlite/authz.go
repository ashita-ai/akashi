package sqlite

import (
	"context"

	"github.com/google/uuid"
)

// HasAccess checks access grants. In lite mode, admin claims make this unreachable,
// but the implementation queries the grants table for correctness.
func (l *LiteDB) HasAccess(ctx context.Context, orgID uuid.UUID, granteeID uuid.UUID, resourceType, resourceID, permission string) (bool, error) {
	var exists bool
	err := l.db.QueryRowContext(ctx,
		`SELECT EXISTS(
		     SELECT 1 FROM access_grants
		     WHERE org_id = ? AND grantee_id = ? AND resource_type = ?
		       AND (resource_id = ? OR resource_id IS NULL)
		       AND permission = ?
		       AND (expires_at IS NULL OR expires_at > datetime('now'))
		 )`,
		uuidStr(orgID), uuidStr(granteeID), resourceType, resourceID, permission,
	).Scan(&exists)
	return exists, err
}

// ListGrantedAgentIDs returns agent IDs accessible via grants.
// In lite mode, admin claims short-circuit before this is called,
// but the implementation is correct for completeness.
func (l *LiteDB) ListGrantedAgentIDs(ctx context.Context, orgID uuid.UUID, granteeID uuid.UUID, selfAgentID string) (map[string]bool, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT DISTINCT resource_id FROM access_grants
		 WHERE org_id = ? AND grantee_id = ?
		   AND resource_type = 'agent_traces' AND permission = 'read'
		   AND (expires_at IS NULL OR expires_at > datetime('now'))
		   AND resource_id IS NOT NULL`,
		uuidStr(orgID), uuidStr(granteeID),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	granted := map[string]bool{selfAgentID: true}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		granted[id] = true
	}
	return granted, rows.Err()
}
