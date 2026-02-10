-- Remove RLS policies and role created in 014/016 that were never activated.
-- Tenant isolation is enforced at the application layer via WHERE org_id = $1 clauses.
-- The app connects as the table owner, which bypasses RLS regardless of policy.
-- No call to SET ROLE akashi_app or SET LOCAL app.org_id exists in the codebase.
-- See internal/specs/11-durability-hardening.md item 4 for rationale.

-- Drop policies first (they reference the akashi_app role).
-- 014 created policies on agents, agent_runs, decisions, access_grants.
-- 016 added a policy on evidence.
DROP POLICY IF EXISTS org_isolation_agents ON agents;
DROP POLICY IF EXISTS org_isolation_runs ON agent_runs;
DROP POLICY IF EXISTS org_isolation_decisions ON decisions;
DROP POLICY IF EXISTS org_isolation_grants ON access_grants;
DROP POLICY IF EXISTS org_isolation_evidence ON evidence;

-- Disable RLS on all tables that had it enabled.
ALTER TABLE agents DISABLE ROW LEVEL SECURITY;
ALTER TABLE agent_runs DISABLE ROW LEVEL SECURITY;
ALTER TABLE decisions DISABLE ROW LEVEL SECURITY;
ALTER TABLE access_grants DISABLE ROW LEVEL SECURITY;
ALTER TABLE evidence DISABLE ROW LEVEL SECURITY;

-- DROP OWNED removes all privileges granted to the role across every object
-- in the current database, which is more robust than enumerating tables.
DROP OWNED BY akashi_app;
DROP ROLE IF EXISTS akashi_app;
