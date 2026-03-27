-- 088: Enforce one canonical per alias in project_links.
--
-- The (org_id, project_a, project_b, link_type) constraint allows multiple
-- alias rows for the same project_a with different project_b values, which
-- makes ResolveProjectAlias nondeterministic. This migration:
--   1. Audits duplicate rows into mutation_audit_log (paper trail).
--   2. Deletes the duplicates (keeping the most recently created).
--   3. Drops the now-redundant non-unique index from migration 087.
--   4. Creates a partial unique index to enforce one canonical per alias.

-- Step 1: Audit the rows we're about to delete so there is a paper trail.
-- mutation_audit_log is append-only (triggers block UPDATE/DELETE on it),
-- so these records are permanent.
INSERT INTO mutation_audit_log (
    request_id, org_id, actor_agent_id, actor_role,
    http_method, endpoint, operation, resource_type, resource_id,
    before_data, after_data, metadata
)
SELECT
    'migration-088',
    pl.org_id,
    'system:migration',
    'system',
    'MIGRATION',
    'migrations/088_unique_alias_per_project',
    'delete',
    'project_link',
    pl.id::text,
    jsonb_build_object(
        'id', pl.id,
        'org_id', pl.org_id,
        'project_a', pl.project_a,
        'project_b', pl.project_b,
        'link_type', pl.link_type,
        'created_by', pl.created_by,
        'created_at', pl.created_at
    ),
    NULL,
    jsonb_build_object(
        'reason', 'duplicate alias removed during uniqueness enforcement',
        'kept_row', (
            SELECT kept.id::text FROM project_links kept
            WHERE kept.org_id = pl.org_id
              AND kept.project_a = pl.project_a
              AND kept.link_type = 'alias'
            ORDER BY kept.created_at DESC
            LIMIT 1
        )
    )
FROM project_links pl
WHERE pl.id IN (
    SELECT id FROM (
        SELECT id,
               ROW_NUMBER() OVER (
                   PARTITION BY org_id, project_a
                   ORDER BY created_at DESC
               ) AS rn
        FROM project_links
        WHERE link_type = 'alias'
    ) ranked
    WHERE rn > 1
);

-- Step 2: Remove older duplicates, keeping only the newest alias per (org_id, project_a).
DELETE FROM project_links
WHERE id IN (
    SELECT id FROM (
        SELECT id,
               ROW_NUMBER() OVER (
                   PARTITION BY org_id, project_a
                   ORDER BY created_at DESC
               ) AS rn
        FROM project_links
        WHERE link_type = 'alias'
    ) ranked
    WHERE rn > 1
);

-- Step 3: Drop the non-unique partial index from migration 087 — the unique
-- index below covers the same columns with the same WHERE clause.
DROP INDEX IF EXISTS idx_project_links_alias;

-- Step 4: One canonical per alias name per org.
CREATE UNIQUE INDEX idx_project_links_alias_unique
    ON project_links (org_id, project_a)
    WHERE link_type = 'alias';
