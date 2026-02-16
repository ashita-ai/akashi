-- 042: Add immutability triggers to deletion_audit_log and integrity_proofs,
-- and widen scored_conflicts float columns from REAL to DOUBLE PRECISION.

-- 1. deletion_audit_log immutability (append-only)
CREATE OR REPLACE FUNCTION prevent_deletion_audit_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'deletion_audit_log is append-only';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER deletion_audit_immutable_update
  BEFORE UPDATE ON deletion_audit_log
  FOR EACH ROW EXECUTE FUNCTION prevent_deletion_audit_modify();

CREATE TRIGGER deletion_audit_immutable_delete
  BEFORE DELETE ON deletion_audit_log
  FOR EACH ROW EXECUTE FUNCTION prevent_deletion_audit_modify();

-- 2. integrity_proofs immutability (append-only)
CREATE OR REPLACE FUNCTION prevent_integrity_proofs_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'integrity_proofs is append-only';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER integrity_proofs_immutable_update
  BEFORE UPDATE ON integrity_proofs
  FOR EACH ROW EXECUTE FUNCTION prevent_integrity_proofs_modify();

CREATE TRIGGER integrity_proofs_immutable_delete
  BEFORE DELETE ON integrity_proofs
  FOR EACH ROW EXECUTE FUNCTION prevent_integrity_proofs_modify();

-- 3. Widen scored_conflicts float columns from REAL to DOUBLE PRECISION.
-- These were REAL (float4) from migration 027; the Go code uses float64.
-- confidence_weight and temporal_decay are already DOUBLE PRECISION (038).
ALTER TABLE scored_conflicts
  ALTER COLUMN topic_similarity    TYPE DOUBLE PRECISION,
  ALTER COLUMN outcome_divergence  TYPE DOUBLE PRECISION,
  ALTER COLUMN significance        TYPE DOUBLE PRECISION;
