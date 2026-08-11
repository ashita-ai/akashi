-- 111: Sampled record of pairs the pre-LLM structural filters suppressed.
--
-- Twelve structural rules in scoreForDecision drop a candidate pair before any
-- LLM call. Each increments a counter, so the suppression RATE is observable;
-- none records WHICH pair, so the suppression's CORRECTNESS is not.
-- disjoint_resource_filtered = 114 is equally consistent with 114 correct drops
-- and with 100 correct drops plus 14 missed contradictions, and nothing in the
-- system distinguishes those worlds. Precision was measured this way once
-- already (conflict_gold_labels, migration 107) and the result changed the
-- detector; recall has never been measured at all, which makes every new
-- suppressor unfalsifiable by construction.
--
-- Why a separate table and not sampled scored_conflicts rows: every path that
-- lists conflicts would have to exclude them, and six apply no status filter at
-- all -- ListConflicts (GET /v1/conflicts sets a status filter only when
-- ?status is present), ListConflictsByDecisionIDs (run enrichment),
-- NewConflictsSinceByOrg, GetConflictStatusCounts, GetConflictAnalytics, and
-- FindOrCreateTopicGroup. A sampled row would surface in an operator's queue,
-- which is precisely the noise the suppressors exist to remove. It would also
-- squat UNIQUE(decision_a_id, decision_b_id) against a later genuine detection,
-- and no read path filters on scoring_method, so a distinct method value would
-- provide no isolation whatsoever.
--
-- Sampling is deterministic in the pair, not random per call. A rescore
-- re-suppresses the same pairs, so per-call randomness would walk the sampled
-- set toward 100% of all suppressed pairs and make "a 5% sample" false within
-- a few rescores. Hashing the ordered pair keeps the sampled set stable, the
-- corpus statistically clean, and the table bounded.
--
-- No retention job: both decision FKs cascade, so a purge takes the samples
-- with it (the same reasoning migration 107 applies to a genuine deletion), and
-- deterministic sampling bounds the row count by construction.
--
-- If a thirteenth structural suppressor is added to scoreForDecision it MUST
-- call sampleSuppression too. A rule that samples nothing is a blind spot
-- exactly where the new rule is wrong.
CREATE TABLE IF NOT EXISTS suppressed_pair_samples (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    decision_a_id UUID NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
    decision_b_id UUID NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
    rule          TEXT NOT NULL,
    sampled_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Label columns mirror conflict_gold_labels (107) so the same blind rating
    -- pass can be pointed at this table unchanged. NULL until rated.
    label         TEXT,
    rationale     TEXT,
    method        TEXT,
    labeled_by    TEXT,
    labeled_at    TIMESTAMPTZ,

    CONSTRAINT suppressed_pair_samples_label_check
        CHECK (label IS NULL OR label IN ('contradiction', 'supersession', 'related_not_contradicting', 'unrelated', 'insufficient')),
    -- A label without provenance is the failure that made conflict_labels
    -- unusable; refuse the half-written row rather than accept it.
    CONSTRAINT suppressed_pair_samples_label_provenance
        CHECK ((label IS NULL AND method IS NULL AND labeled_by IS NULL AND labeled_at IS NULL)
            OR (label IS NOT NULL AND method IS NOT NULL AND labeled_by IS NOT NULL AND labeled_at IS NOT NULL)),
    -- Pair IDs are stored in canonical order so (a,b) and (b,a) are one row and
    -- the sampling hash is side-independent.
    CONSTRAINT suppressed_pair_samples_ordered
        CHECK (decision_a_id < decision_b_id),
    UNIQUE (decision_a_id, decision_b_id, rule)
);

-- False-negative rate per rule: the reason this table exists.
CREATE INDEX IF NOT EXISTS idx_suppressed_pair_samples_org_rule
    ON suppressed_pair_samples (org_id, rule);

-- The labelling pass pulls the unrated backlog.
CREATE INDEX IF NOT EXISTS idx_suppressed_pair_samples_unlabeled
    ON suppressed_pair_samples (org_id) WHERE label IS NULL;
