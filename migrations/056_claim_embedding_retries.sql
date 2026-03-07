-- 056: Track and retry failed claim embedding generation.
--
-- Claim embeddings are generated asynchronously after a decision is traced.
-- If embedding fails (network issues, provider downtime, timeout), the decision
-- permanently falls back to full-outcome conflict scoring. These columns enable
-- a background retry loop with exponential backoff and a retry cap.
--
-- claim_embeddings_failed_at: set to NOW() on failure, cleared on success.
-- claim_embedding_attempts: incremented on each failure, used for backoff and cap.
--
-- Neither column is in the immutability trigger's blocklist (migration 036),
-- so they are mutable by default — no trigger modification needed.

ALTER TABLE decisions
    ADD COLUMN IF NOT EXISTS claim_embeddings_failed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claim_embedding_attempts SMALLINT NOT NULL DEFAULT 0;

-- Partial index for the retry query: find decisions with failed claim embeddings
-- that are eligible for retry. Ordered by failure time so oldest failures are
-- retried first.
CREATE INDEX IF NOT EXISTS idx_decisions_claim_retry
    ON decisions (claim_embeddings_failed_at ASC)
    WHERE claim_embeddings_failed_at IS NOT NULL
      AND valid_to IS NULL
      AND embedding IS NOT NULL;
