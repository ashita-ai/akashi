-- 078: Durable persistence for integrity audit results.
--
-- The background integrity audit loop detects Merkle root mismatches and chain
-- linkage breaks, but previously only logged them. Logs rotate and OTel counters
-- are aggregates — neither preserves *which* proof failed or *when*. For a system
-- whose purpose is an immutable decision trail, audit detections must themselves
-- be immutable.
--
-- Both pass and fail results are stored for positive attestation: "we checked
-- proof X at time T and it was valid" is as valuable as "it was tampered."

CREATE TABLE integrity_audit_results (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    proof_id       UUID NOT NULL REFERENCES integrity_proofs(id) ON DELETE CASCADE,
    violation_type TEXT NOT NULL,  -- 'pass', 'merkle_root_mismatch', 'chain_linkage_broken', 'chain_linkage_nil_previous'
    details        JSONB,         -- optional structured context (stored_root, expected_previous, etc.)
    checked_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_integrity_audit_results_org_time
    ON integrity_audit_results (org_id, checked_at DESC);

CREATE INDEX idx_integrity_audit_results_violations
    ON integrity_audit_results (violation_type)
    WHERE violation_type != 'pass';
