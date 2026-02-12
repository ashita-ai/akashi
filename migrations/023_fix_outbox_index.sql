-- Migration 023: Fix search_outbox partial index predicate.
--
-- The worker queries WHERE (locked_until IS NULL OR locked_until < now())
-- AND attempts < $1, but the partial index used WHERE locked_until IS NULL.
-- Rows with expired locks (locked_until < now()) bypassed the index entirely,
-- causing sequential scans after Qdrant outages. Replace with a predicate on
-- attempts which is both immutable-enough for the index and matches the
-- worker's actual filter condition.

DROP INDEX IF EXISTS idx_search_outbox_pending;

CREATE INDEX idx_search_outbox_pending
    ON search_outbox (created_at ASC)
    WHERE attempts < 10;
