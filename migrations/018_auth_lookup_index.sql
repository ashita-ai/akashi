-- Migration 018: Index for global agent_id auth lookup.
--
-- GetAgentsByAgentIDGlobal queries agents WHERE agent_id = $1 across all orgs
-- during token issuance. The existing idx_agents_org_agent (org_id, agent_id)
-- cannot be used because org_id is not in the predicate. This index covers
-- the auth lookup without requiring a full table scan.
CREATE INDEX IF NOT EXISTS idx_agents_agent_id_global ON agents (agent_id);
