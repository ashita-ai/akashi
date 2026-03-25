-- 080: enforce append-only invariant on integrity_audit_results.
--
-- Migration 077 created integrity_audit_results to provide a durable paper
-- trail for integrity audit outcomes (both passes and failures). However,
-- unlike integrity_violations (migration 079) and integrity_proofs
-- (migration 042), the table had no immutability triggers. A compromised
-- admin could delete passing audit results to hide the fact that a
-- subsequent failure is anomalous, or update result rows to flip failures
-- to passes — destroying the positive attestation ("we checked, it was
-- fine") that makes the audit trail trustworthy.
--
-- Also replaces ON DELETE CASCADE with RESTRICT on proof_id FK. The same
-- reasoning from migration 079 applies: integrity_proofs has immutability
-- triggers blocking DELETE today, but CASCADE creates a latent hazard if
-- those triggers were ever dropped during a data migration.

-- 1. Immutability triggers (same pattern as migrations 042 and 079).

CREATE OR REPLACE FUNCTION prevent_integrity_audit_results_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'integrity_audit_results is append-only';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER integrity_audit_results_immutable_update
  BEFORE UPDATE ON integrity_audit_results
  FOR EACH ROW EXECUTE FUNCTION prevent_integrity_audit_results_modify();

CREATE TRIGGER integrity_audit_results_immutable_delete
  BEFORE DELETE ON integrity_audit_results
  FOR EACH ROW EXECUTE FUNCTION prevent_integrity_audit_results_modify();

-- 2. Replace ON DELETE CASCADE with RESTRICT on proof_id FK.

ALTER TABLE integrity_audit_results
    DROP CONSTRAINT integrity_audit_results_proof_id_fkey;

ALTER TABLE integrity_audit_results
    ADD CONSTRAINT integrity_audit_results_proof_id_fkey
    FOREIGN KEY (proof_id) REFERENCES integrity_proofs(id) ON DELETE RESTRICT;
