-- 077: add integrity_violations table to durably persist detected tampering.
--
-- The background integrity audit loop detects Merkle root mismatches and
-- chain linkage breaks but previously only logged them. Logs are ephemeral
-- (rotation, compromise). This table provides a durable, append-only record
-- that survives log rotation and is queryable for incident response.

CREATE TABLE IF NOT EXISTS integrity_violations (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    proof_id      UUID NOT NULL REFERENCES integrity_proofs(id) ON DELETE CASCADE,
    violation_type TEXT NOT NULL CHECK (violation_type IN (
        'merkle_root_mismatch',
        'chain_linkage_broken',
        'chain_linkage_nil_previous'
    )),
    details       JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_integrity_violations_org_created
    ON integrity_violations (org_id, created_at DESC);

COMMENT ON TABLE integrity_violations IS
    'Append-only record of detected integrity proof failures. Written by the background audit loop.';
