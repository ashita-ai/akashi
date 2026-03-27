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
	if pl.ID == uuid.Nil {
		pl.ID = uuid.New()
	}
	if pl.CreatedAt.IsZero() {
		pl.CreatedAt = time.Now().UTC()
	}

	err := db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO project_links (id, org_id, project_a, project_b, link_type, created_by, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			pl.ID, pl.OrgID, pl.ProjectA, pl.ProjectB, pl.LinkType, pl.CreatedBy, pl.CreatedAt,
		); err != nil {
			return fmt.Errorf("storage: create project link: %w", err)
		}

		audit.ResourceID = pl.ID.String()
		audit.AfterData = pl
		if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
			return fmt.Errorf("storage: audit in create project link tx: %w", err)
		}
		return nil
	})
	if err != nil {
		return model.ProjectLink{}, err
	}
	return pl, nil
}

// DeleteProjectLinkWithAudit removes a project link and inserts an audit entry atomically.
func (db *DB) DeleteProjectLinkWithAudit(ctx context.Context, orgID, id uuid.UUID, audit MutationAuditEntry) error {
	return db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
		return nil
	})
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
	limit, offset = clampPagination(limit, offset, 50, 1000)
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

	links := make([]model.ProjectLink, 0)
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

	projects := make([]string, 0)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("storage: scan linked project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// CreateProjectAlias upserts an alias→canonical mapping (link_type='alias')
// with an accompanying mutation_audit entry. If the alias already points to the
// same canonical, it's a no-op. If the canonical differs (e.g. the git remote
// changed), the mapping is updated to the new canonical (last-write-wins) and
// the audit entry captures both the old and new state.
func (db *DB) CreateProjectAlias(ctx context.Context, orgID uuid.UUID, alias, canonical, createdBy string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: begin create project alias tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Use a CTE to capture the previous canonical (if any) so we can record
	// BeforeData in the audit entry and distinguish creates from updates.
	proposedID := uuid.New()
	var actualID uuid.UUID
	var oldCanonical *string
	err = tx.QueryRow(ctx,
		`WITH old AS (
		     SELECT id, project_b FROM project_links
		     WHERE org_id = $2 AND project_a = $3 AND link_type = 'alias'
		 )
		 INSERT INTO project_links (id, org_id, project_a, project_b, link_type, created_by)
		 VALUES ($1, $2, $3, $4, 'alias', $5)
		 ON CONFLICT (org_id, project_a) WHERE link_type = 'alias'
		 DO UPDATE SET project_b = EXCLUDED.project_b, created_by = EXCLUDED.created_by
		 WHERE project_links.project_b != EXCLUDED.project_b
		 RETURNING id, (SELECT project_b FROM old)`,
		proposedID, orgID, alias, canonical, createdBy,
	).Scan(&actualID, &oldCanonical)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already exists with the same canonical — nothing to audit.
		return nil
	}
	if err != nil {
		return fmt.Errorf("storage: create project alias: %w", err)
	}

	operation := "create"
	var beforeData any
	if oldCanonical != nil {
		operation = "update"
		beforeData = model.ProjectLink{
			ID: actualID, OrgID: orgID,
			ProjectA: alias, ProjectB: *oldCanonical,
			LinkType: "alias", CreatedBy: createdBy,
		}
	}

	if err := InsertMutationAuditTx(ctx, tx, MutationAuditEntry{
		OrgID:        orgID,
		ActorAgentID: createdBy,
		ActorRole:    "system",
		Operation:    operation,
		ResourceType: "project_link",
		ResourceID:   actualID.String(),
		Endpoint:     "system:auto-alias",
		BeforeData:   beforeData,
		AfterData: model.ProjectLink{
			ID: actualID, OrgID: orgID,
			ProjectA: alias, ProjectB: canonical,
			LinkType: "alias", CreatedBy: createdBy,
		},
	}); err != nil {
		return fmt.Errorf("storage: audit in create project alias tx: %w", err)
	}

	return tx.Commit(ctx)
}

// IsAliasTarget reports whether the given name is the canonical target
// (project_b) of any existing alias link. Used to prevent alias chains:
// if X→name already exists, creating name→Y would form a chain X→name→Y
// where only one hop is resolved.
func (db *DB) IsAliasTarget(ctx context.Context, orgID uuid.UUID, name string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS(
		     SELECT 1 FROM project_links
		     WHERE org_id = $1 AND link_type = 'alias' AND project_b = $2
		 )`,
		orgID, name,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("storage: is alias target: %w", err)
	}
	return exists, nil
}

// ResolveProjectAlias looks up a canonical project name for the given alias.
// Alias links use link_type='alias' with project_a as the alias and project_b
// as the canonical name (unidirectional, unlike conflict_scope links).
// Returns "" if no alias mapping exists for the given name.
func (db *DB) ResolveProjectAlias(ctx context.Context, orgID uuid.UUID, alias string) (string, error) {
	var canonical string
	err := db.pool.QueryRow(ctx,
		`SELECT project_b
		 FROM project_links
		 WHERE org_id = $1 AND link_type = 'alias' AND project_a = $2
		 ORDER BY created_at DESC
		 LIMIT 1`,
		orgID, alias,
	).Scan(&canonical)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("storage: resolve project alias: %w", err)
	}
	return canonical, nil
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

	types := make([]string, 0)
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

	projects := make([]string, 0)
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
	var created int
	err := db.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
			return fmt.Errorf("storage: grant all project links: %w", err)
		}

		created = int(tag.RowsAffected())

		audit.Metadata = map[string]any{"links_created": created}
		if err := InsertMutationAuditTx(ctx, tx, audit); err != nil {
			return fmt.Errorf("storage: audit in grant all project links tx: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return created, nil
}
