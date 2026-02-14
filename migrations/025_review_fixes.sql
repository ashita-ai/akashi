-- 025: Performance indexes, schema fixes, and documentation.
--
-- Indexes: org+time for query/check, keyset pagination for export.
-- evidence.org_id: Add FK (NOT VALID + VALIDATE).
-- To keep migration deterministic and avoid data loss, archive orphan rows
-- into evidence_orphans before deleting them from evidence.
-- current_decisions: ORDER BY in view not guaranteed; callers add ORDER BY.

CREATE INDEX IF NOT EXISTS idx_decisions_org_current_time
    ON decisions (org_id, valid_from DESC) WHERE valid_to IS NULL;

CREATE INDEX IF NOT EXISTS idx_decisions_org_export
    ON decisions (org_id, valid_from ASC, id ASC) WHERE valid_to IS NULL;

CREATE TABLE IF NOT EXISTS evidence_orphans (
    LIKE evidence INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING IDENTITY INCLUDING STORAGE INCLUDING COMMENTS,
    PRIMARY KEY (id),
    archived_at timestamptz NOT NULL DEFAULT now(),
    archive_reason text NOT NULL DEFAULT 'orphan org_id before fk_evidence_org validation'
);

INSERT INTO evidence_orphans
SELECT e.*, now() AS archived_at, 'orphan org_id before fk_evidence_org validation' AS archive_reason
FROM evidence e
LEFT JOIN organizations o ON o.id = e.org_id
WHERE o.id IS NULL
ON CONFLICT (id) DO NOTHING;

DELETE FROM evidence e
USING evidence_orphans a
WHERE e.id = a.id;

ALTER TABLE evidence ADD CONSTRAINT fk_evidence_org
    FOREIGN KEY (org_id) REFERENCES organizations(id) NOT VALID;
ALTER TABLE evidence VALIDATE CONSTRAINT fk_evidence_org;

COMMENT ON VIEW current_decisions IS
'Current (non-superseded) decisions. Order is not guaranteed when used in subqueries; add ORDER BY to the outer query if ordering is required.';
