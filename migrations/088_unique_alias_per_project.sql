-- 088: Enforce one canonical per alias in project_links.
--
-- The (org_id, project_a, project_b, link_type) constraint allows multiple
-- alias rows for the same project_a with different project_b values, which
-- makes ResolveProjectAlias nondeterministic. This migration removes
-- duplicates (keeping the most recently created) and adds a partial unique
-- index so each alias resolves to exactly one canonical within an org.

-- Remove older duplicates, keeping only the newest alias per (org_id, project_a).
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

-- One canonical per alias name per org.
CREATE UNIQUE INDEX idx_project_links_alias_unique
    ON project_links (org_id, project_a)
    WHERE link_type = 'alias';
