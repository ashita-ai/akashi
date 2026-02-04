-- 009_create_materialized_views.sql
-- Materialized views for conflict detection and current agent state.

-- Conflict Detection: same decision_type, different agents, different outcomes,
-- both currently valid, within 1 hour of each other.
CREATE MATERIALIZED VIEW decision_conflicts AS
SELECT
    d1.id AS decision_a_id,
    d2.id AS decision_b_id,
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
    ON d1.decision_type = d2.decision_type
    AND d1.agent_id != d2.agent_id
    AND d1.outcome != d2.outcome
    AND d1.valid_to IS NULL
    AND d2.valid_to IS NULL
    AND d1.id < d2.id
    AND ABS(EXTRACT(EPOCH FROM (d1.valid_from - d2.valid_from))) < 3600
WITH DATA;

CREATE UNIQUE INDEX idx_decision_conflicts_pair ON decision_conflicts(decision_a_id, decision_b_id);

-- Current Agent State: latest run and activity summary per agent.
CREATE MATERIALIZED VIEW agent_current_state AS
SELECT
    ar.agent_id,
    ar.id AS latest_run_id,
    ar.status AS run_status,
    ar.started_at,
    COUNT(ae.id) AS event_count,
    MAX(ae.occurred_at) AS last_activity,
    (SELECT COUNT(*) FROM decisions d WHERE d.agent_id = ar.agent_id AND d.valid_to IS NULL) AS active_decisions
FROM agent_runs ar
LEFT JOIN agent_events ae ON ae.run_id = ar.id
WHERE ar.started_at = (
    SELECT MAX(started_at) FROM agent_runs WHERE agent_id = ar.agent_id
)
GROUP BY ar.agent_id, ar.id, ar.status, ar.started_at
WITH DATA;
