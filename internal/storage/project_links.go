//go:build !lite

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

const projectLinkCols = `id, org_id, project_a, project_b, link_type, created_by, created_at`

func scanOneProjectLink(row pgxRowScanner) (model.ProjectLink, error) {
	var pl model.ProjectLink
	if err := row.Scan(
		&pl.ID, &pl.OrgID, &pl.ProjectA, &pl.ProjectB,
		&pl.LinkType, &pl.CreatedBy, &pl.CreatedAt,
	); err != nil {
		return model.ProjectLink{}, fmt.Errorf("storage: scan project link: %w", err)
	}
	return pl, nil
}

// CreateProjectLinkWithAudit inserts a project link and an audit entry atomically.
func (db *DB) CreateProjectLinkWithAudit(ctx context.Context, pl model.ProjectLink, audit MutationAuditEntry) (model.ProjectLink, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return model.ProjectLink{}, fmt.Errorf("storage: begin create project link tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if pl.ID == uuid.Nil {
		pl.ID = uuid.New()
	}
	if pl.CreatedAt.IsZero() {
		pl.CreatedAt = time.Now().UTC()
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO project_links (id, org_id, project_a, project_b, link_type, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		pl.ID, pl.OrgID, pl.ProjectA, pl.ProjectB, pl.LinkType, pl.CreatedBy, pl.CreatedAt,
	); err != nil {
		return model.ProjectLink{}, fmt.Errorf("storage: create project link: %w", err)
	}

	audit.ResourceID = pl.ID.String()
	audit.AfterData = pl
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return model.ProjectLink{}, fmt.Errorf("storage: audit in create project link tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.ProjectLink{}, fmt.Errorf("storage: commit create project link tx: %w", err)
	}
	return pl, nil
}

// DeleteProjectLinkWithAudit removes a project link and inserts an audit entry atomically.
func (db *DB) DeleteProjectLinkWithAudit(ctx context.Context, orgID, id uuid.UUID, audit MutationAuditEntry) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin delete project link tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`DELETE FROM project_links WHERE id = $1 AND org_id = $2`, id, orgID,
	)
	if err != nil {
		return fmt.Errorf("storage: delete project link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("storage: project link %s: %w", id, ErrNotFound)
	}

	audit.ResourceID = id.String()
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return fmt.Errorf("storage: audit in delete project link tx: %w", err)
	}

	return tx.Commit(ctx)
}

// GetProjectLink retrieves a project link by ID, scoped to an org.
func (db *DB) GetProjectLink(ctx context.Context, orgID, id uuid.UUID) (model.ProjectLink, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT `+projectLinkCols+` FROM project_links WHERE id = $1 AND org_id = $2`, id, orgID,
	)
	pl, err := scanOneProjectLink(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ProjectLink{}, fmt.Errorf("storage: project link %s: %w", id, ErrNotFound)
		}
		return model.ProjectLink{}, fmt.Errorf("storage: get project link: %w", err)
	}
	return pl, nil
}

// ListProjectLinks returns all project links within an org, ordered by created_at descending.
func (db *DB) ListProjectLinks(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]model.ProjectLink, int, error) {
	var total int
	err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM project_links WHERE org_id = $1`, orgID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: count project links: %w", err)
	}

	rows, err := db.pool.Query(ctx,
		`SELECT `+projectLinkCols+`
		 FROM project_links
		 WHERE org_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`, orgID, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: list project links: %w", err)
	}
	defer rows.Close()

	var links []model.ProjectLink
	for rows.Next() {
		pl, err := scanOneProjectLink(rows)
		if err != nil {
			return nil, 0, err
		}
		links = append(links, pl)
	}
	return links, total, rows.Err()
}

// LinkedProjects returns all projects linked to the given project within an org
// for the specified link type. Links are bidirectional: if A is linked to B,
// querying for either A or B returns the other.
func (db *DB) LinkedProjects(ctx context.Context, orgID uuid.UUID, project, linkType string) ([]string, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT CASE WHEN project_a = $2 THEN project_b ELSE project_a END
		 FROM project_links
		 WHERE org_id = $1 AND link_type = $3
		   AND (project_a = $2 OR project_b = $2)`,
		orgID, project, linkType,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: linked projects: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("storage: scan linked project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// DistinctDecisionTypes returns all distinct decision_type values used in decisions within an org.
func (db *DB) DistinctDecisionTypes(ctx context.Context, orgID uuid.UUID) ([]string, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT DISTINCT decision_type FROM decisions
		 WHERE org_id = $1 AND valid_to IS NULL
		 ORDER BY decision_type`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: distinct decision types: %w", err)
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("storage: scan distinct decision type: %w", err)
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

// DistinctProjects returns all distinct project names used in decisions within an org.
func (db *DB) DistinctProjects(ctx context.Context, orgID uuid.UUID) ([]string, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT DISTINCT project FROM decisions
		 WHERE org_id = $1 AND project IS NOT NULL AND valid_to IS NULL
		 ORDER BY project`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: distinct projects: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("storage: scan distinct project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// GrantAllProjectLinks creates bidirectional conflict_scope links between all
// distinct projects in the org. Existing links are skipped (ON CONFLICT DO NOTHING).
// Returns the number of new links created.
func (db *DB) GrantAllProjectLinks(ctx context.Context, orgID uuid.UUID, createdBy, linkType string, audit MutationAuditEntry) (int, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("storage: begin grant all project links tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`INSERT INTO project_links (org_id, project_a, project_b, link_type, created_by)
		 SELECT DISTINCT $1, LEAST(a.project, b.project), GREATEST(a.project, b.project), $2, $3
		 FROM (SELECT DISTINCT project FROM decisions WHERE org_id = $1 AND project IS NOT NULL AND valid_to IS NULL) a
		 CROSS JOIN (SELECT DISTINCT project FROM decisions WHERE org_id = $1 AND project IS NOT NULL AND valid_to IS NULL) b
		 WHERE a.project < b.project
		 ON CONFLICT (org_id, project_a, project_b, link_type) DO NOTHING`,
		orgID, linkType, createdBy,
	)
	if err != nil {
		return 0, fmt.Errorf("storage: grant all project links: %w", err)
	}

	created := int(tag.RowsAffected())

	audit.Metadata = map[string]any{"links_created": created}
	if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
		return 0, fmt.Errorf("storage: audit in grant all project links tx: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("storage: commit grant all project links tx: %w", err)
	}
	return created, nil
}
