-- 082: Change integrity_violations.org_id FK from CASCADE to RESTRICT.
-- CASCADE bypasses the immutability triggers added in migration 079,
-- silently destroying tamper evidence when an org is deleted.
-- RESTRICT forces explicit handling of violation records before org deletion.

ALTER TABLE integrity_violations
    DROP CONSTRAINT IF EXISTS fk_integrity_violations_org,
    ADD CONSTRAINT fk_integrity_violations_org
        FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE RESTRICT;
