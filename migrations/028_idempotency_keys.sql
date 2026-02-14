-- 028: Idempotency keys for write APIs.
--
-- Supports safe retries for POST endpoints by storing request hash and
-- canonical response payload per (org, agent, endpoint, key).

CREATE TABLE IF NOT EXISTS idempotency_keys (
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id        TEXT NOT NULL,
    endpoint        TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('in_progress', 'completed')),
    status_code     INTEGER,
    response_data   JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, agent_id, endpoint, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created_at
    ON idempotency_keys (created_at);
