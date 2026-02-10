-- Add tags to agents for group-based access control.
-- Tags are string arrays; agents sharing at least one tag can see each other's decisions.
-- Coexists with the existing per-agent grant system in access_grants.

ALTER TABLE agents ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';

-- GIN index enables efficient && (overlap) queries for tag-based access lookups.
CREATE INDEX idx_agents_tags ON agents USING GIN (tags);
