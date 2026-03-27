-- 087: Add partial index for project alias lookups.
--
-- The existing idx_project_links_lookup uses WHERE link_type = 'conflict_scope',
-- which excludes alias rows. ResolveProjectAlias queries by (org_id, link_type='alias',
-- project_a) on every trace where MCP roots fail, so it needs its own index.

CREATE INDEX IF NOT EXISTS idx_project_links_alias
    ON project_links (org_id, project_a)
    WHERE link_type = 'alias';
