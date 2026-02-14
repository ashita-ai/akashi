-- 026: Smarter conflict detection.
--
-- 1. Type normalization: LOWER(TRIM(decision_type)) so "Architecture" matches "architecture".
-- 2. Outcome normalization: LOWER(TRIM(outcome)) so "microservices" vs "Microservices"
--    is not a false conflict (same outcome, different casing).
-- 3. Self-contradiction: same agent, same type, different outcomes, both current.
--    Catches agents flipping on decisions without revising. Uses 7-day window to avoid
--    flagging deliberate long-term policy changes.
-- 4. conflict_kind: 'cross_agent' | 'self_contradiction' so consumers can distinguish.

DROP MATERIALIZED VIEW IF EXISTS decision_conflicts;

CREATE MATERIALIZED VIEW decision_conflicts AS
-- Cross-agent: different agents, same (normalized) type, different (normalized) outcomes,
-- both current, within 1 hour. Time window keeps conflicts "in play" together.
SELECT
    'cross_agent'::TEXT AS conflict_kind,
    d1.id AS decision_a_id,
    d2.id AS decision_b_id,
    d1.org_id,
    d1.agent_id AS agent_a,
    d2.agent_id AS agent_b,
    d1.run_id AS run_a,
    d2.run_id AS run_b,
    d1.decision_type,
    d1.outcome AS outcome_a,
    d2.outcome AS outcome_b,
    d1.confidence AS confidence_a,
    d2.confidence AS confidence_b,
    d1.valid_from AS decided_at_a,
    d2.valid_from AS decided_at_b,
    GREATEST(d1.valid_from, d2.valid_from) AS detected_at
FROM decisions d1
JOIN decisions d2
    ON LOWER(TRIM(d1.decision_type)) = LOWER(TRIM(d2.decision_type))
    AND d1.org_id = d2.org_id
    AND d1.agent_id != d2.agent_id
    AND LOWER(TRIM(d1.outcome)) != LOWER(TRIM(d2.outcome))
    AND d1.valid_to IS NULL
    AND d2.valid_to IS NULL
    AND d1.id < d2.id
    AND ABS(EXTRACT(EPOCH FROM (d1.valid_from - d2.valid_from))) < 3600

UNION ALL

-- Self-contradiction: same agent, same type, different outcomes, both current,
-- within 7 days. Flags agents who said X then Y without revising.
SELECT
    'self_contradiction'::TEXT AS conflict_kind,
    d1.id AS decision_a_id,
    d2.id AS decision_b_id,
    d1.org_id,
    d1.agent_id AS agent_a,
    d2.agent_id AS agent_b,
    d1.run_id AS run_a,
    d2.run_id AS run_b,
    d1.decision_type,
    d1.outcome AS outcome_a,
    d2.outcome AS outcome_b,
    d1.confidence AS confidence_a,
    d2.confidence AS confidence_b,
    d1.valid_from AS decided_at_a,
    d2.valid_from AS decided_at_b,
    GREATEST(d1.valid_from, d2.valid_from) AS detected_at
FROM decisions d1
JOIN decisions d2
    ON LOWER(TRIM(d1.decision_type)) = LOWER(TRIM(d2.decision_type))
    AND d1.org_id = d2.org_id
    AND d1.agent_id = d2.agent_id
    AND LOWER(TRIM(d1.outcome)) != LOWER(TRIM(d2.outcome))
    AND d1.valid_to IS NULL
    AND d2.valid_to IS NULL
    AND d1.id < d2.id
    AND ABS(EXTRACT(EPOCH FROM (d1.valid_from - d2.valid_from))) < 604800  -- 7 days
WITH DATA;

CREATE UNIQUE INDEX idx_decision_conflicts_pair
    ON decision_conflicts(decision_a_id, decision_b_id);
