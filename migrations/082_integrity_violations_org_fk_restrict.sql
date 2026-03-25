-- 082: change integrity_violations.org_id FK from CASCADE to RESTRICT.
--
-- Migration 079 corrected the proof_id FK from CASCADE to RESTRICT but
-- missed org_id. With CASCADE, deleting an organization silently removes
-- all its violation records — bypassing the immutability triggers added
-- in 079. RESTRICT ensures violation records block org deletion, which
-- is the correct behavior for an append-only tamper-evidence table.

ALTER TABLE integrity_violations
    DROP CONSTRAINT integrity_violations_org_id_fkey;

ALTER TABLE integrity_violations
    ADD CONSTRAINT integrity_violations_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE RESTRICT;
