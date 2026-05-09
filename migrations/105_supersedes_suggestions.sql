-- 105: Extend decision_supersedes with the 'suggested' relationship for
-- detector-inferred latent supersedes_id links (PR-2 of #708, see #710).
--
-- A 'suggested' row is the conflict scorer's hypothesis that a newer trace
-- intended to supersede an older one but the agent forgot to set
-- supersedes_id. akashi_check returns these so agents can confirm by
-- re-tracing with supersedes_id set, at which point the trigger from
-- migration 104 promotes the row to relationship='supersedes' is_primary=TRUE.
--
-- Suggestions always have is_primary=FALSE; the unique partial index from 104
-- (idx_decision_supersedes_primary) prevents accidental promotion.

ALTER TABLE decision_supersedes
    DROP CONSTRAINT decision_supersedes_relationship_check;

ALTER TABLE decision_supersedes
    ADD CONSTRAINT decision_supersedes_relationship_check
    CHECK (relationship IN ('supersedes', 'reconciles', 'suggested'));

ALTER TABLE decision_supersedes
    ADD COLUMN suggested_by           TEXT,
    ADD COLUMN suggested_confidence   REAL,
    ADD COLUMN suggested_reason       TEXT;

-- Suggestions must carry source attribution; non-suggested rows must not.
-- Enforced as a CHECK so accidental writes from the trace path can't pollute
-- the suggestion columns and so suggestion writers can't omit the source.
ALTER TABLE decision_supersedes
    ADD CONSTRAINT decision_supersedes_suggestion_fields_check
    CHECK (
        (relationship = 'suggested' AND suggested_by IS NOT NULL)
        OR
        (relationship <> 'suggested' AND suggested_by IS NULL AND suggested_confidence IS NULL AND suggested_reason IS NULL)
    );

-- Suggestions must never be promoted to primary (primary is reserved for
-- agent-confirmed supersedes/reconciles links).
ALTER TABLE decision_supersedes
    ADD CONSTRAINT decision_supersedes_suggestion_not_primary_check
    CHECK (relationship <> 'suggested' OR is_primary = FALSE);

-- Read-path index for the akashi_check fan-out: "for these decision IDs
-- (caller's recent traces), find any suggestions where they are the
-- superseding side". Partial-index keeps it lean — the table is otherwise
-- dominated by confirmed 'supersedes' rows.
CREATE INDEX idx_decision_supersedes_suggested
    ON decision_supersedes(org_id, superseding_id, recorded_at DESC)
    WHERE relationship = 'suggested';

-- Cleanup index for retention worker: prune by recorded_at across orgs.
CREATE INDEX idx_decision_supersedes_suggested_recorded_at
    ON decision_supersedes(recorded_at)
    WHERE relationship = 'suggested';
