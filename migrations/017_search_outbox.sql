-- Migration 017: Search outbox table for Qdrant sync.
--
-- Adds an outbox table that drives asynchronous upsert/delete of decision embeddings
-- into the external vector search index (Qdrant). Entries are inserted inside the same
-- transaction that writes the decision, guaranteeing at-least-once delivery.
--
-- Also drops the pgvector HNSW index on decisions.embedding — Qdrant replaces it.
-- The embedding column stays as source of truth for outbox worker backfill.
-- Evidence HNSW index stays (evidence search remains pgvector, low volume).

DROP INDEX IF EXISTS idx_decisions_embedding;

CREATE TABLE search_outbox (
    id           BIGSERIAL PRIMARY KEY,
    decision_id  UUID NOT NULL,
    org_id       UUID NOT NULL,
    operation    TEXT NOT NULL CHECK (operation IN ('upsert', 'delete')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT,
    locked_until TIMESTAMPTZ
);

-- Pending entries: ordered by creation time for the poll query.
-- The worker's WHERE clause filters by locked_until at query time;
-- a partial index with now() is not possible (not IMMUTABLE).
CREATE INDEX idx_search_outbox_pending
    ON search_outbox (created_at ASC)
    WHERE locked_until IS NULL;

-- Dedup: at most one pending operation per decision per operation type.
CREATE UNIQUE INDEX idx_search_outbox_decision_op
    ON search_outbox (decision_id, operation);

-- Org lookup for bulk deletion (GDPR).
CREATE INDEX idx_search_outbox_org
    ON search_outbox (org_id);
