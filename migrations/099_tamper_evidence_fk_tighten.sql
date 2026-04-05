-- 099: Tighten FK constraints on tamper-evidence tables for consistency.
--
-- Three issues (GitHub #658):
--
-- 1. proof_leaves.proof_id uses ON DELETE CASCADE (migration 084). If integrity_proofs'
--    immutability trigger were ever dropped (e.g. during a data migration), all leaf
--    hashes would silently vanish — destroying the ability to verify Merkle proofs.
--    Migrations 079/080 applied the same CASCADE→RESTRICT fix to integrity_violations
--    and integrity_audit_results; proof_leaves was missed.
--
-- 2. proof_leaves.org_id uses the PostgreSQL default ON DELETE NO ACTION (implicit).
--    Migration 082 explicitly switched integrity_violations.org_id to RESTRICT.
--    Make the same choice explicit here: org deletion is blocked while proof leaves
--    exist, which is correct for append-only tamper-evidence.
--
-- 3. proof_leaves has no immutability triggers despite being an append-only table
--    that preserves cryptographic proof chains. Migrations 079/080 added triggers
--    for integrity_violations and integrity_audit_results; proof_leaves was missed.
--
-- 4. decision_assessments.org_id (migration 051) has no FK constraint to organizations
--    at all. Migration 086 already tightened decision_id to RESTRICT; org_id should
--    have an FK too. Use RESTRICT to match the append-only audit semantics.

-- ---------------------------------------------------------------------------
-- Part 1: proof_leaves.proof_id — CASCADE → RESTRICT
-- ---------------------------------------------------------------------------

-- The inline FK from migration 084 uses the auto-generated name.
ALTER TABLE proof_leaves
    DROP CONSTRAINT proof_leaves_proof_id_fkey;

ALTER TABLE proof_leaves
    ADD CONSTRAINT proof_leaves_proof_id_fkey
    FOREIGN KEY (proof_id) REFERENCES integrity_proofs(id) ON DELETE RESTRICT;

-- ---------------------------------------------------------------------------
-- Part 2: proof_leaves.org_id — NO ACTION (implicit) → RESTRICT (explicit)
-- ---------------------------------------------------------------------------

-- Migration 089 dropped the named fk_proof_leaves_org, leaving only the inline
-- REFERENCES. PostgreSQL names inline FKs as <table>_<col>_fkey.
ALTER TABLE proof_leaves
    DROP CONSTRAINT proof_leaves_org_id_fkey;

ALTER TABLE proof_leaves
    ADD CONSTRAINT proof_leaves_org_id_fkey
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE RESTRICT;

-- ---------------------------------------------------------------------------
-- Part 3: proof_leaves immutability triggers
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION prevent_proof_leaves_modify()
RETURNS TRIGGER AS $$ BEGIN
  RAISE EXCEPTION 'proof_leaves is append-only';
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER proof_leaves_immutable_update
  BEFORE UPDATE ON proof_leaves
  FOR EACH ROW EXECUTE FUNCTION prevent_proof_leaves_modify();

CREATE TRIGGER proof_leaves_immutable_delete
  BEFORE DELETE ON proof_leaves
  FOR EACH ROW EXECUTE FUNCTION prevent_proof_leaves_modify();

-- ---------------------------------------------------------------------------
-- Part 4: decision_assessments.org_id — add missing FK with RESTRICT
-- ---------------------------------------------------------------------------

-- Use NOT VALID + VALIDATE to avoid holding ACCESS EXCLUSIVE on organizations
-- for a full table scan (same pattern as migration 097 for project_links).
ALTER TABLE decision_assessments
    ADD CONSTRAINT fk_decision_assessments_org
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE RESTRICT
    NOT VALID;

ALTER TABLE decision_assessments VALIDATE CONSTRAINT fk_decision_assessments_org;
