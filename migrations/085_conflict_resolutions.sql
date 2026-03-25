-- 085: Conflict resolution history table.
--
-- When the conflict scorer re-detects a previously resolved conflict, the
-- ON CONFLICT DO UPDATE in InsertScoredConflict transitions it from
-- resolved → open and NULLs out resolved_by, resolved_at, resolution_note,
-- and winning_decision_id. Before this migration, the resolution metadata
-- was silently destroyed with no audit trail — a paper-trail violation
-- where human judgments could be undone without any record.
--
-- This table archives each resolution snapshot before the scorer overwrites
-- it. The conflict row shows current state; this table preserves the full
-- resolution lifecycle.

CREATE TABLE IF NOT EXISTS conflict_resolutions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conflict_id         UUID NOT NULL REFERENCES scored_conflicts(id) ON DELETE RESTRICT,
    org_id              UUID NOT NULL REFERENCES organizations(id),
    resolved_by         TEXT NOT NULL,
    resolved_at         TIMESTAMPTZ NOT NULL,
    resolution_note     TEXT,
    winning_decision_id UUID REFERENCES decisions(id) ON DELETE RESTRICT,
    archived_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_conflict_resolutions_conflict ON conflict_resolutions(conflict_id);
CREATE INDEX idx_conflict_resolutions_org ON conflict_resolutions(org_id);

-- Immutability triggers: archived resolutions are audit artifacts and must
-- never be modified or deleted (same pattern as migrations 042, 079, 080).

CREATE OR REPLACE FUNCTION prevent_conflict_resolutions_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'conflict_resolutions is append-only';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER conflict_resolutions_immutable_update
  BEFORE UPDATE ON conflict_resolutions
  FOR EACH ROW EXECUTE FUNCTION prevent_conflict_resolutions_modify();

CREATE TRIGGER conflict_resolutions_immutable_delete
  BEFORE DELETE ON conflict_resolutions
  FOR EACH ROW EXECUTE FUNCTION prevent_conflict_resolutions_modify();
