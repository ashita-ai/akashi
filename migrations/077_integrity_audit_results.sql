-- 077: Create integrity_audit_results for durable persistence of integrity
-- audit outcomes.
--
-- The background integrity audit verifies Merkle proofs and chain linkage.
-- Previously violations were only logged; this table provides a durable
-- paper trail that survives log rotation. Both passes and failures are
-- recorded so operators can prove the audit ran (positive attestation).

CREATE TABLE integrity_audit_results (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    proof_id    UUID NOT NULL REFERENCES integrity_proofs(id) ON DELETE CASCADE,
    check_type  TEXT NOT NULL,     -- 'merkle_root' or 'chain_linkage'
    passed      BOOLEAN NOT NULL,
    sweep_type  TEXT NOT NULL DEFAULT 'sample', -- 'sample' or 'full'
    detail      TEXT,              -- human-readable context on failure
    checked_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Primary lookup: "show me recent audit results for this org"
CREATE INDEX idx_integrity_audit_results_org_checked
    ON integrity_audit_results (org_id, checked_at DESC);

-- Alert path: "show me failures for this org"
CREATE INDEX idx_integrity_audit_results_failed
    ON integrity_audit_results (org_id, checked_at DESC)
    WHERE NOT passed;
