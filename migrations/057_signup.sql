-- 057: Add email column to agents for self-serve signup (issue #314).
-- Nullable because existing agents were created without an email address.

ALTER TABLE agents ADD COLUMN email TEXT;

-- Enforce email uniqueness across all orgs to prevent duplicate signups.
-- Partial index excludes NULL so agents without email are unconstrained.
CREATE UNIQUE INDEX idx_agents_email ON agents (email) WHERE email IS NOT NULL;
