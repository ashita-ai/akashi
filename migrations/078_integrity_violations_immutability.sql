-- 078: enforce append-only invariant on integrity_violations.
--
-- Migration 077 created integrity_violations and labeled it "append-only"
-- in a comment, but unlike integrity_proofs (migration 042), there were no
-- triggers enforcing immutability. A compromised process or careless admin
-- could DELETE or UPDATE violation records, erasing evidence of detected
-- tampering. This migration closes that gap.
--
-- Also changes the proof_id FK from CASCADE to RESTRICT. integrity_proofs
-- already has immutability triggers blocking DELETE, so the CASCADE could
-- never fire today — but it created a latent hazard: if the proof trigger
-- were ever dropped (e.g. for a data migration), all violation records
-- referencing those proofs would silently vanish. RESTRICT makes the
-- protection explicit rather than relying on a transitive guarantee.

-- 1. Immutability triggers (same pattern as migration 042 for integrity_proofs).

CREATE OR REPLACE FUNCTION prevent_integrity_violations_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'integrity_violations is append-only';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER integrity_violations_immutable_update
  BEFORE UPDATE ON integrity_violations
  FOR EACH ROW EXECUTE FUNCTION prevent_integrity_violations_modify();

CREATE TRIGGER integrity_violations_immutable_delete
  BEFORE DELETE ON integrity_violations
  FOR EACH ROW EXECUTE FUNCTION prevent_integrity_violations_modify();

-- 2. Replace ON DELETE CASCADE with RESTRICT on proof_id FK.
--    Drop the old constraint and re-add with RESTRICT.

ALTER TABLE integrity_violations
    DROP CONSTRAINT integrity_violations_proof_id_fkey;

ALTER TABLE integrity_violations
    ADD CONSTRAINT integrity_violations_proof_id_fkey
    FOREIGN KEY (proof_id) REFERENCES integrity_proofs(id) ON DELETE RESTRICT;
