-- 076: add missing org_id foreign key to decision_claims.
--
-- Migration 033 created the table and 040 added the decision_id FK, but
-- org_id was never constrained. Without this FK, orphaned claims survive
-- org deletion.
--
-- NOTE: future CREATE TABLE statements should always use IF NOT EXISTS
-- for defense-in-depth, even though Atlas tracks application state.

-- Clean up any orphaned claims before adding the constraint, matching the
-- pattern established in migration 040 (decision_id FK).
DELETE FROM decision_claims
WHERE org_id NOT IN (SELECT id FROM organizations);

ALTER TABLE decision_claims
    ADD CONSTRAINT fk_decision_claims_org
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE;
