-- 065: Org-level settings table with JSONB for extensible configuration.
-- Initial use: auto-resolution policy for conflicts (issue #358).
CREATE TABLE IF NOT EXISTS org_settings (
    org_id     UUID PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    settings   JSONB NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT NOT NULL DEFAULT ''
);
