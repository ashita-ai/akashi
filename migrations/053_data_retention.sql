-- 053: Data retention policies, legal holds, and deletion audit log.
--
-- Adds:
--   organizations.retention_days          -- NULL = retain forever (default)
--   organizations.retention_exclude_types -- decision_types exempt from auto-deletion
--   retention_holds                       -- legal holds that exempt decisions from auto-deletion
--   deletion_log                          -- operation-level audit trail for retention runs

ALTER TABLE organizations
    ADD COLUMN retention_days           INTEGER,
    ADD COLUMN retention_exclude_types  TEXT[];

CREATE TABLE retention_holds (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    reason          TEXT        NOT NULL,
    hold_from       TIMESTAMPTZ NOT NULL,
    hold_to         TIMESTAMPTZ NOT NULL,
    decision_types  TEXT[],       -- NULL = all types
    agent_ids       TEXT[],       -- NULL = all agents
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at     TIMESTAMPTZ           -- NULL = active
);

CREATE INDEX idx_retention_holds_org ON retention_holds(org_id) WHERE released_at IS NULL;

-- deletion_log is an operation-level summary (how many rows, why, by whom).
-- Distinct from deletion_audit_log (per-row content archives for GDPR forensics).
CREATE TABLE deletion_log (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    trigger         TEXT        NOT NULL CHECK (trigger IN ('policy', 'manual', 'gdpr')),
    initiated_by    TEXT,         -- agent_id for manual/gdpr, NULL for policy
    criteria        JSONB       NOT NULL,
    deleted_counts  JSONB       NOT NULL DEFAULT '{}',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

CREATE INDEX idx_deletion_log_org ON deletion_log(org_id, started_at DESC);
