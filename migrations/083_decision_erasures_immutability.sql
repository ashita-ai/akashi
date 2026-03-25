-- 083: enforce append-only invariant on decision_erasures.
--
-- Migration 055 created decision_erasures to serve as GDPR compliance proof —
-- a durable record that a decision's PII was erased. However, unlike
-- deletion_audit_log (migration 042), integrity_proofs (042),
-- integrity_violations (079), and integrity_audit_results (080), the table
-- had no immutability triggers. A compromised process or careless admin could
-- DELETE erasure records, destroying the legal proof that a GDPR Article 17
-- erasure was performed, or UPDATE rows to alter the recorded hash or
-- timestamp — both of which are compliance liabilities.
--
-- This migration closes that gap using the same pattern established in
-- migrations 042, 079, and 080.

-- 1. Immutability trigger function.

CREATE OR REPLACE FUNCTION prevent_decision_erasures_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'decision_erasures is append-only';
END; $$ LANGUAGE plpgsql;

-- 2. Block UPDATE and DELETE.

CREATE TRIGGER decision_erasures_immutable_update
  BEFORE UPDATE ON decision_erasures
  FOR EACH ROW EXECUTE FUNCTION prevent_decision_erasures_modify();

CREATE TRIGGER decision_erasures_immutable_delete
  BEFORE DELETE ON decision_erasures
  FOR EACH ROW EXECUTE FUNCTION prevent_decision_erasures_modify();
