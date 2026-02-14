-- 031: Universal immutable mutation audit log.
--
-- Records high-risk API mutations with request-scoped metadata and optional
-- before/after payloads. Rows are append-only and cannot be updated/deleted.

CREATE TABLE IF NOT EXISTS mutation_audit_log (
    id              BIGSERIAL PRIMARY KEY,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    request_id      TEXT NOT NULL,
    org_id          UUID NOT NULL,
    actor_agent_id  TEXT NOT NULL,
    actor_role      TEXT NOT NULL,
    http_method     TEXT NOT NULL,
    endpoint        TEXT NOT NULL,
    operation       TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    before_data     JSONB,
    after_data      JSONB,
    metadata        JSONB NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_mutation_audit_log_org_time
    ON mutation_audit_log (org_id, occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_mutation_audit_log_request
    ON mutation_audit_log (request_id);

CREATE INDEX IF NOT EXISTS idx_mutation_audit_log_resource
    ON mutation_audit_log (resource_type, resource_id, occurred_at DESC);

CREATE OR REPLACE FUNCTION mutation_audit_log_immutable_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'mutation_audit_log is append-only';
END;
$$;

DROP TRIGGER IF EXISTS trg_mutation_audit_log_no_update ON mutation_audit_log;
CREATE TRIGGER trg_mutation_audit_log_no_update
BEFORE UPDATE ON mutation_audit_log
FOR EACH ROW
EXECUTE FUNCTION mutation_audit_log_immutable_guard();

DROP TRIGGER IF EXISTS trg_mutation_audit_log_no_delete ON mutation_audit_log;
CREATE TRIGGER trg_mutation_audit_log_no_delete
BEFORE DELETE ON mutation_audit_log
FOR EACH ROW
EXECUTE FUNCTION mutation_audit_log_immutable_guard();
