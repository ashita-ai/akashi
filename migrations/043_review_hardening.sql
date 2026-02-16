-- 043: Review hardening — immutability triggers and FK cascade fix.
--
-- G1: search_outbox_dead_letters is an audit archive but lacked immutability
--     triggers, allowing accidental UPDATE/DELETE of dead-letter records.
-- G2: agent_events_archive is an audit archive but lacked immutability
--     triggers. Note: TimescaleDB retention policies drop entire chunks
--     (DDL, not row-level DELETE), so these triggers do not interfere with
--     the retention policy added in migration 041.
-- G3: resolution_decision_id FK lacked ON DELETE SET NULL. If an agent whose
--     decision resolved a conflict between two OTHER agents is deleted, the
--     FK would block the cascade. ON DELETE SET NULL allows the delete to
--     proceed while preserving the conflict row.

-- G1: search_outbox_dead_letters immutability (append-only).
CREATE OR REPLACE FUNCTION prevent_outbox_dead_letters_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'search_outbox_dead_letters is append-only';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER outbox_dead_letters_immutable_update
  BEFORE UPDATE ON search_outbox_dead_letters
  FOR EACH ROW EXECUTE FUNCTION prevent_outbox_dead_letters_modify();

CREATE TRIGGER outbox_dead_letters_immutable_delete
  BEFORE DELETE ON search_outbox_dead_letters
  FOR EACH ROW EXECUTE FUNCTION prevent_outbox_dead_letters_modify();

-- G2: agent_events_archive immutability (append-only).
-- Row-level triggers are safe alongside TimescaleDB chunk retention because
-- drop_chunks uses DDL (DROP TABLE on chunk), not row-level DELETE.
CREATE OR REPLACE FUNCTION prevent_events_archive_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'agent_events_archive is append-only';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER events_archive_immutable_update
  BEFORE UPDATE ON agent_events_archive
  FOR EACH ROW EXECUTE FUNCTION prevent_events_archive_modify();

CREATE TRIGGER events_archive_immutable_delete
  BEFORE DELETE ON agent_events_archive
  FOR EACH ROW EXECUTE FUNCTION prevent_events_archive_modify();

-- G3: Fix resolution_decision_id FK to use ON DELETE SET NULL.
-- Migration 038 added the column with a plain REFERENCES (implicit RESTRICT).
-- This blocks DeleteAgentData when the resolution decision belongs to the
-- deleted agent but the conflict is between two other agents' decisions.
ALTER TABLE scored_conflicts
  DROP CONSTRAINT IF EXISTS scored_conflicts_resolution_decision_id_fkey;

ALTER TABLE scored_conflicts
  ADD CONSTRAINT scored_conflicts_resolution_decision_id_fkey
  FOREIGN KEY (resolution_decision_id) REFERENCES decisions(id) ON DELETE SET NULL;
