-- 077: durable storage for integrity violations detected by the audit loop.
--
-- The background integrity audit verifies Merkle proofs and chain linkage.
-- When a violation is found, it must be recorded durably — structured logs
-- are necessary for alerting but insufficient for a tamper-evident audit
-- trail because log rotation destroys the evidence.
--
-- This table is append-only by design. Application roles should not have
-- DELETE or UPDATE grants on it.

CREATE TABLE IF NOT EXISTS integrity_violations (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    proof_id       UUID NOT NULL REFERENCES integrity_proofs(id) ON DELETE CASCADE,
    violation_type TEXT NOT NULL CHECK (violation_type IN ('merkle_mismatch', 'chain_break', 'nil_previous_root')),
    expected       TEXT NOT NULL,
    actual         TEXT NOT NULL,
    detected_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Index for efficient lookups by org (most common query pattern for the UI).
CREATE INDEX IF NOT EXISTS idx_integrity_violations_org_detected
    ON integrity_violations (org_id, detected_at DESC);

-- Index for lookups by proof (to check if a specific proof has known violations).
CREATE INDEX IF NOT EXISTS idx_integrity_violations_proof
    ON integrity_violations (proof_id);
