-- 090: Add composite index on decisions(org_id, created_at) for integrity proof
-- batch queries and any future retention queries that filter by physical write time.
CREATE INDEX IF NOT EXISTS idx_decisions_org_created
    ON decisions (org_id, created_at);
