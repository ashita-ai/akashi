-- 055: GDPR tombstone erasure support.
-- Adds decision_erasures table for tracking erasure provenance while
-- preserving the decision row in-place (tombstone approach). PII fields
-- are scrubbed to '[erased]' and the original content hash is preserved
-- in this table so the audit chain remains verifiable.

CREATE TABLE decision_erasures (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID NOT NULL REFERENCES organizations(id),
    decision_id   UUID NOT NULL REFERENCES decisions(id),
    erased_by     TEXT NOT NULL,
    erased_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    original_hash TEXT NOT NULL,
    reason        TEXT
);

CREATE INDEX idx_decision_erasures_org_decision
    ON decision_erasures (org_id, decision_id);

CREATE INDEX idx_decision_erasures_erased_at
    ON decision_erasures (org_id, erased_at DESC);

-- Each decision may only be erased once.
CREATE UNIQUE INDEX idx_decision_erasures_unique_decision
    ON decision_erasures (decision_id);

-- Append-only: prevent UPDATE and DELETE on erasure records.
CREATE OR REPLACE FUNCTION prevent_erasure_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'decision_erasures is append-only: % not allowed', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_decision_erasures_no_update
    BEFORE UPDATE ON decision_erasures
    FOR EACH ROW EXECUTE FUNCTION prevent_erasure_mutation();

CREATE TRIGGER trg_decision_erasures_no_delete
    BEFORE DELETE ON decision_erasures
    FOR EACH ROW EXECUTE FUNCTION prevent_erasure_mutation();
