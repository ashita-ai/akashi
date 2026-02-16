-- 040: Add FK from decision_claims to decisions with cascade delete.
-- Defense-in-depth: the application explicitly deletes claims in DeleteAgentData
-- (GDPR erasure), but the FK ensures no orphaned claims survive if a decision is
-- deleted through any other code path.
ALTER TABLE decision_claims
    ADD CONSTRAINT fk_decision_claims_decision
    FOREIGN KEY (decision_id) REFERENCES decisions(id) ON DELETE CASCADE;
