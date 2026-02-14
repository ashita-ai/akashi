-- decision_claims stores sentence-level embeddings for fine-grained conflict
-- detection. Full-outcome embeddings dilute disagreements in multi-topic reviews;
-- claim-level embeddings let the scorer detect contradictions between specific
-- findings even when the overall outcomes are semantically similar.

CREATE TABLE IF NOT EXISTS decision_claims (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id uuid NOT NULL,
    org_id      uuid NOT NULL,
    claim_idx   smallint NOT NULL,
    claim_text  text NOT NULL,
    embedding   vector(1024),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE(decision_id, claim_idx)
);

CREATE INDEX idx_decision_claims_decision ON decision_claims(decision_id);
CREATE INDEX idx_decision_claims_org ON decision_claims(org_id);

-- Allow 'claim' as a scoring method for claim-level conflict detection.
ALTER TABLE scored_conflicts DROP CONSTRAINT IF EXISTS scored_conflicts_scoring_method_check;
ALTER TABLE scored_conflicts ADD CONSTRAINT scored_conflicts_scoring_method_check
    CHECK (scoring_method = ANY (ARRAY['embedding', 'text', 'claim']));
