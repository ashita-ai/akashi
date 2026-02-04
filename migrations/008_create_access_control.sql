-- 008_create_access_control.sql
-- Agent identity, role assignment, and fine-grained access grants.

CREATE TABLE agents (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id        TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT 'agent'
                    CHECK (role IN ('admin', 'agent', 'reader')),
    api_key_hash    TEXT,
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE access_grants (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    grantor_id      UUID NOT NULL REFERENCES agents(id),
    grantee_id      UUID NOT NULL REFERENCES agents(id),
    resource_type   TEXT NOT NULL
                    CHECK (resource_type IN ('agent_traces', 'decision', 'run')),
    resource_id     TEXT,
    permission      TEXT NOT NULL
                    CHECK (permission IN ('read', 'write')),
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ,
    UNIQUE(grantee_id, resource_type, resource_id, permission)
);

CREATE INDEX idx_access_grants_grantee ON access_grants(grantee_id, resource_type);
CREATE INDEX idx_agents_agent_id ON agents(agent_id);
