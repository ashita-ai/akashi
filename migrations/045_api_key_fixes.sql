-- 045: API key FK constraint and prefix lookup index.
--
-- Two changes:
--
-- 1. Prefix + agent lookup index for O(1) auth pre-filter before Argon2.
--    verifyAPIKey currently loads all active keys for an agent and runs a
--    full Argon2id hash for each. With this index and GetAPIKeyByPrefixAndAgent,
--    auth for managed-format keys (ak_<prefix>_<secret>) goes from O(N·hash)
--    to O(1·hash).
--
-- 2. FK from api_keys to agents for referential integrity.
--    api_keys.agent_id was TEXT NOT NULL with no FK. An orphaned key (agent
--    deleted without cascading) could authenticate successfully with no valid
--    agent backing it. The FK + ON DELETE CASCADE closes this gap.
--
--    Prerequisite: agents(org_id, agent_id) must have a UNIQUE constraint
--    (not just a unique index) for PostgreSQL to accept a FK reference.
--    idx_agents_org_agent (from 001) is a unique index; we promote it to a
--    named unique constraint here using USING INDEX to avoid building a second
--    index.
--
--    Safety pre-check: if any api_keys rows reference an agent_id/org_id pair
--    that no longer exists in agents, the ALTER TABLE will fail. Run:
--      SELECT k.agent_id, k.org_id
--        FROM api_keys k
--        LEFT JOIN agents a ON a.org_id = k.org_id AND a.agent_id = k.agent_id
--       WHERE a.id IS NULL;
--    Must return 0 rows before applying this migration.

-- Prefix + agent lookup index for O(1) auth pre-filter.
CREATE INDEX idx_api_keys_prefix_agent ON api_keys(prefix, agent_id)
    WHERE revoked_at IS NULL;

-- Promote the existing unique index to a named constraint so that the FK
-- below can reference it. USING INDEX adopts the existing index without
-- building a second one; the index is renamed to match the constraint name.
ALTER TABLE agents
    ADD CONSTRAINT uq_agents_org_agent UNIQUE USING INDEX idx_agents_org_agent;

-- FK from api_keys to agents.
-- Column order must match the unique constraint above: (org_id, agent_id).
-- ON DELETE CASCADE: deleting an agent cascades to its api_keys.
-- Decisions retain api_key_id with ON DELETE SET NULL (wired in 044).
ALTER TABLE api_keys
    ADD CONSTRAINT fk_api_keys_agent
    FOREIGN KEY (org_id, agent_id) REFERENCES agents(org_id, agent_id)
    ON DELETE CASCADE;
