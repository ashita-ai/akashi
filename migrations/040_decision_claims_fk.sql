-- 040: Add FK from decision_claims to decisions with cascade delete.
-- Defense-in-depth: the application explicitly deletes claims in DeleteAgentData
-- (GDPR erasure), but the FK ensures no orphaned claims survive if a decision is
-- deleted through any other code path.

-- Clean up any orphaned claims before adding the constraint. These can exist if
-- decisions were deleted (e.g. via GDPR erasure or bi-temporal invalidation)
-- before this FK was added.
DELETE FROM decision_claims
WHERE decision_id NOT IN (SELECT id FROM decisions);

ALTER TABLE decision_claims
    ADD CONSTRAINT fk_decision_claims_decision
    FOREIGN KEY (decision_id) REFERENCES decisions(id) ON DELETE CASCADE;
