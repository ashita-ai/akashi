-- 096: Re-run decision_type backfill with trigger bypass.
--
-- Migration 092 attempted to UPDATE decisions.decision_type using the alias
-- table from migration 091, but that UPDATE is blocked by the immutability
-- trigger (trg_decisions_immutable, migration 036, refined in 055). The
-- trigger protects decision_type even in the GDPR erasure bypass path.
--
-- This migration disables the trigger, runs the same backfill logic, then
-- re-enables it. The trigger is re-enabled unconditionally (even on error)
-- because Atlas runs each migration in a transaction — if the UPDATE fails,
-- the entire transaction rolls back including the DISABLE.

ALTER TABLE decisions DISABLE TRIGGER trg_decisions_immutable;

UPDATE decisions d
SET decision_type = a.canonical,
    metadata = d.metadata || jsonb_build_object('original_decision_type', d.decision_type)
FROM decision_type_aliases a
WHERE d.decision_type = a.alias
  AND d.org_id = a.org_id
  AND d.valid_to IS NULL;

ALTER TABLE decisions ENABLE TRIGGER trg_decisions_immutable;
