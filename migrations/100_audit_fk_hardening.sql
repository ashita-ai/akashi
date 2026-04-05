-- 100: Harden FK constraints on audit and tamper-evidence tables.
--
-- Three issues addressed:
--
-- 1) integrity_audit_results.org_id uses ON DELETE CASCADE (migration 077).
--    Migration 080 tightened the proof_id FK to RESTRICT but left org_id as
--    CASCADE. If an organization is deleted, its integrity audit results
--    silently vanish — destroying the attestation record ("we checked this org,
--    it was fine"). Audit tables must never CASCADE on org deletion.
--
-- 2) deletion_log.org_id uses ON DELETE CASCADE (migration 053). Same problem:
--    the operation-level audit trail for retention runs would be silently
--    destroyed if the org is deleted. Additionally, deletion_log has no
--    immutability triggers — a compromised admin could delete or modify
--    retention run records to cover tracks.
--
-- 3) evidence.org_id has no FK at all (migration 001). The column exists and is
--    NOT NULL, but orphaned evidence rows can survive org deletion because
--    there is no referential constraint. This is defense-in-depth: the
--    application already scopes by org_id, but a stale org_id should fail
--    at the constraint level too.
--
-- 4) deletion_audit_log.org_id has no FK (migration 029). Same as evidence:
--    the column exists but has no referential constraint. Adding RESTRICT
--    ensures the per-row archive survives org deletion attempts.

-- ============================================================================
-- 1. integrity_audit_results.org_id: CASCADE → RESTRICT
-- ============================================================================

ALTER TABLE integrity_audit_results
    DROP CONSTRAINT integrity_audit_results_org_id_fkey;

ALTER TABLE integrity_audit_results
    ADD CONSTRAINT integrity_audit_results_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE RESTRICT;

-- ============================================================================
-- 2. deletion_log.org_id: CASCADE → RESTRICT + immutability triggers
-- ============================================================================

ALTER TABLE deletion_log
    DROP CONSTRAINT deletion_log_org_id_fkey;

ALTER TABLE deletion_log
    ADD CONSTRAINT deletion_log_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE RESTRICT;

-- Immutability: deletion_log is an audit trail. DELETE is always blocked.
-- UPDATE is restricted to the completion fields (deleted_counts, completed_at)
-- which are written once after the retention run finishes. All other fields
-- (id, org_id, trigger, initiated_by, criteria, started_at) are immutable.
-- This follows the same field-level guard pattern as decisions_immutable_guard
-- (migration 036) rather than a blanket block, because the two-phase
-- lifecycle (start → complete) requires a single UPDATE.

CREATE OR REPLACE FUNCTION deletion_log_immutable_guard()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id THEN
        RAISE EXCEPTION 'deletion_log: cannot modify id';
    END IF;
    IF NEW.org_id IS DISTINCT FROM OLD.org_id THEN
        RAISE EXCEPTION 'deletion_log: cannot modify org_id';
    END IF;
    IF NEW.trigger IS DISTINCT FROM OLD.trigger THEN
        RAISE EXCEPTION 'deletion_log: cannot modify trigger';
    END IF;
    IF NEW.initiated_by IS DISTINCT FROM OLD.initiated_by THEN
        RAISE EXCEPTION 'deletion_log: cannot modify initiated_by';
    END IF;
    IF NEW.criteria IS DISTINCT FROM OLD.criteria THEN
        RAISE EXCEPTION 'deletion_log: cannot modify criteria';
    END IF;
    IF NEW.started_at IS DISTINCT FROM OLD.started_at THEN
        RAISE EXCEPTION 'deletion_log: cannot modify started_at';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deletion_log_immutable_update
  BEFORE UPDATE ON deletion_log
  FOR EACH ROW EXECUTE FUNCTION deletion_log_immutable_guard();

CREATE OR REPLACE FUNCTION prevent_deletion_log_delete()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'deletion_log rows cannot be deleted';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER deletion_log_immutable_delete
  BEFORE DELETE ON deletion_log
  FOR EACH ROW EXECUTE FUNCTION prevent_deletion_log_delete();

-- ============================================================================
-- 3. evidence.org_id: add FK with RESTRICT
-- ============================================================================
-- Use NOT VALID + VALIDATE to avoid a full table lock on large tables.
-- NOT VALID adds the constraint without scanning existing rows (instant).
-- VALIDATE CONSTRAINT scans rows with only a SHARE UPDATE EXCLUSIVE lock
-- (concurrent reads and writes are not blocked).

ALTER TABLE evidence
    ADD CONSTRAINT evidence_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE RESTRICT
    NOT VALID;

ALTER TABLE evidence
    VALIDATE CONSTRAINT evidence_org_id_fkey;

-- ============================================================================
-- 4. deletion_audit_log.org_id: add FK with RESTRICT
-- ============================================================================
-- Same NOT VALID + VALIDATE pattern for online safety.

ALTER TABLE deletion_audit_log
    ADD CONSTRAINT deletion_audit_log_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE RESTRICT
    NOT VALID;

ALTER TABLE deletion_audit_log
    VALIDATE CONSTRAINT deletion_audit_log_org_id_fkey;
