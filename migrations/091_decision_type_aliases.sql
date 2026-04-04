-- 091: Add decision_type_aliases table for normalizing free-form decision types
-- to canonical values. Mirrors the project alias pattern but simpler (dedicated table,
-- no link_type column).

CREATE TABLE decision_type_aliases (
    alias      TEXT        NOT NULL,
    canonical  TEXT        NOT NULL,
    org_id     UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_by TEXT        NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, alias)
);

-- Seed known duplicates observed in production data (824 decisions, 30 distinct types).
-- Each alias maps to the closest standard type from quality.DefaultStandardDecisionTypes.
INSERT INTO decision_type_aliases (alias, canonical, org_id, created_by)
SELECT alias, canonical, id, 'system:migration-091'
FROM organizations
CROSS JOIN (VALUES
    ('refactoring',                'refactor'),
    ('test',                       'testing'),
    ('review',                     'code_review'),
    ('code_change',                'implementation'),
    ('positioning_recommendation', 'assessment'),
    ('positioning_analysis',       'assessment'),
    ('competitive_analysis',       'assessment'),
    ('documentation_consolidation','documentation')
) AS seed(alias, canonical);
