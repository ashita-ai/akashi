-- 081: Add project columns to scored_conflicts for project-scoped queries.
--
-- The session hook and MCP akashi_conflicts tool need to filter conflicts by
-- project. Currently scored_conflicts has no project information; it lives only
-- on the decisions table. Denormalizing here avoids JOINing through decisions
-- on every session start (read-hot path).

ALTER TABLE scored_conflicts ADD COLUMN project_a TEXT;
ALTER TABLE scored_conflicts ADD COLUMN project_b TEXT;

-- Backfill from existing decisions.
UPDATE scored_conflicts sc
SET project_a = da.project, project_b = db.project
FROM decisions da, decisions db
WHERE sc.decision_a_id = da.id AND sc.decision_b_id = db.id;

-- Partial indexes for the read-hot open-conflicts query path.
CREATE INDEX idx_scored_conflicts_project_a ON scored_conflicts (project_a) WHERE status = 'open';
CREATE INDEX idx_scored_conflicts_project_b ON scored_conflicts (project_b) WHERE status = 'open';
