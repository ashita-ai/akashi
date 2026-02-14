-- 029: Durability and audit trail hardening.
--
-- Adds:
--   1) search_outbox_dead_letters: immutable archive before outbox dead-letter cleanup.
--   2) deletion_audit_log: append-only row-level archive for destructive deletes.

CREATE TABLE IF NOT EXISTS search_outbox_dead_letters (
    outbox_id    BIGINT PRIMARY KEY,
    decision_id  UUID NOT NULL,
    org_id       UUID NOT NULL,
    operation    TEXT NOT NULL CHECK (operation IN ('upsert', 'delete')),
    attempts     INT NOT NULL,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL,
    locked_until TIMESTAMPTZ,
    archived_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_search_outbox_dead_letters_org_created
    ON search_outbox_dead_letters (org_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_search_outbox_dead_letters_archived
    ON search_outbox_dead_letters (archived_at DESC);

CREATE TABLE IF NOT EXISTS deletion_audit_log (
    id          BIGSERIAL PRIMARY KEY,
    org_id      UUID NOT NULL,
    agent_id    TEXT NOT NULL,
    table_name  TEXT NOT NULL,
    record_id   TEXT NOT NULL,
    record_data JSONB NOT NULL,
    deleted_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_deletion_audit_log_org_deleted
    ON deletion_audit_log (org_id, deleted_at DESC);

CREATE INDEX IF NOT EXISTS idx_deletion_audit_log_agent_deleted
    ON deletion_audit_log (org_id, agent_id, deleted_at DESC);

CREATE INDEX IF NOT EXISTS idx_deletion_audit_log_table
    ON deletion_audit_log (table_name, deleted_at DESC);
