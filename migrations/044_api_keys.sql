-- 044: Decoupled API key management.
--
-- Moves API keys from a single hash on the agents table to a dedicated
-- api_keys table supporting multiple keys per agent, key rotation,
-- expiration, revocation, and per-key attribution on decisions.
--
-- Design: keys inherit the agent's role (no independent role on keys).
-- The prefix column stores the first 8 characters of the raw key for
-- identification in logs/UI without exposing the full credential.

CREATE TABLE api_keys (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    prefix        TEXT NOT NULL,
    key_hash      TEXT NOT NULL,
    agent_id      TEXT NOT NULL,
    org_id        UUID NOT NULL REFERENCES organizations(id),
    label         TEXT NOT NULL DEFAULT '',
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);

-- Active keys lookup by org + agent (most common query path during auth).
CREATE INDEX idx_api_keys_org_agent ON api_keys(org_id, agent_id) WHERE revoked_at IS NULL;

-- Prefix lookup for key identification in admin UI.
CREATE INDEX idx_api_keys_prefix ON api_keys(prefix);

-- Add api_key_id FK to decisions for per-key attribution.
ALTER TABLE decisions ADD COLUMN api_key_id UUID REFERENCES api_keys(id) ON DELETE SET NULL;
CREATE INDEX idx_decisions_api_key ON decisions(api_key_id) WHERE api_key_id IS NOT NULL;
