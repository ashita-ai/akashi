-- 069: Fix conflict_groups.first_detected_at to reflect when conflicts could
-- have first existed, not when the scorer happened to run.
--
-- Problem: first_detected_at defaults to now() at group creation time, which
-- means backfills and rescores stamp every group with the current date. This
-- makes it impossible to trend false positive rates over time.
--
-- Fix: set first_detected_at = max(decision_a.transaction_time, decision_b.transaction_time)
-- for each group. A conflict cannot exist before both participating decisions exist,
-- so the later decision's creation time is the earliest possible detection time.

UPDATE conflict_groups cg
SET first_detected_at = sub.earliest_possible
FROM (
    SELECT sc.group_id,
           MIN(GREATEST(da.transaction_time, db.transaction_time)) AS earliest_possible
    FROM scored_conflicts sc
    JOIN decisions da ON da.id = sc.decision_a_id
    JOIN decisions db ON db.id = sc.decision_b_id
    WHERE sc.group_id IS NOT NULL
    GROUP BY sc.group_id
) sub
WHERE cg.id = sub.group_id
  AND sub.earliest_possible < cg.first_detected_at;
