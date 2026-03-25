-- 082: Preserve Merkle leaf hashes so integrity proofs survive retention purge and GDPR erasure.
--
-- Problem: verifyProofsForOrg re-fetches content_hash values from the decisions table,
-- but retention purges delete those rows and GDPR erasure changes their content_hash.
-- Either case causes false merkle_root_mismatch violations.
--
-- Fix: snapshot the leaf hashes at proof-creation time. Verification reads from this
-- table instead of re-querying decisions. The full decision content is still purged
-- (no PII retained), but the cryptographic proof chain remains verifiable.

CREATE TABLE proof_leaves (
    proof_id   UUID   NOT NULL REFERENCES integrity_proofs(id) ON DELETE CASCADE,
    org_id     UUID   NOT NULL REFERENCES organizations(id),
    leaf_hash  TEXT   NOT NULL,
    CONSTRAINT fk_proof_leaves_org FOREIGN KEY (org_id) REFERENCES organizations(id)
);

-- Primary lookup: fetch all leaves for a proof in deterministic order.
CREATE INDEX idx_proof_leaves_proof_id ON proof_leaves (proof_id, leaf_hash);

-- Org-scoped queries (e.g. admin dashboards, retention audits).
CREATE INDEX idx_proof_leaves_org_id ON proof_leaves (org_id);
