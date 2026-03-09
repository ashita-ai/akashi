-- 063: Add project_links table for cross-project conflict detection.
-- Controls which projects' decisions should be compared during conflict scoring.
-- Separates conflict topology (which decision spaces overlap) from access control.

CREATE TABLE project_links (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL,
    project_a TEXT NOT NULL,
    project_b TEXT NOT NULL,
    link_type TEXT NOT NULL DEFAULT 'conflict_scope',
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, project_a, project_b, link_type)
);

CREATE INDEX idx_project_links_org ON project_links (org_id);
CREATE INDEX idx_project_links_lookup ON project_links (org_id, link_type)
    WHERE link_type = 'conflict_scope';
