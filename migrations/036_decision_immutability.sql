-- 036: Enforce decision row immutability via BEFORE UPDATE trigger.
--
-- Akashi is an audit trail: once a decision is recorded, its core content must
-- not change. Revisions create a new row via supersedes_id. This trigger
-- enforces that invariant at the database level, not just the application layer.
--
-- Immutable columns (raise exception if OLD != NEW):
--   outcome, reasoning, confidence, decision_type, agent_id, run_id,
--   org_id, content_hash, valid_from, created_at, transaction_time
--
-- Mutable columns (permitted updates):
--   valid_to              — set when decision is superseded
--   embedding             — async backfill by embedding provider
--   outcome_embedding     — async backfill for conflict scoring
--   precedent_ref         — cleared during agent deletion cleanup
--   supersedes_id         — cleared during agent deletion cleanup
--   search_vector         — maintained by FTS trigger (migration 022)
--   quality_score         — recomputed during scoring updates
--   session_id            — may be enriched post-creation
--   agent_context         — may be enriched post-creation
--   metadata              — may be enriched post-creation
--
-- DELETE is NOT blocked because agent deletion (GDPR compliance) requires
-- removing entire decision rows.
--
-- To run the one-time rehash script after this migration is applied, temporarily
-- disable the trigger: ALTER TABLE decisions DISABLE TRIGGER trg_decisions_immutable;
-- Then re-enable: ALTER TABLE decisions ENABLE TRIGGER trg_decisions_immutable;

CREATE OR REPLACE FUNCTION decisions_immutable_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- Check each immutable column. Use IS DISTINCT FROM to handle NULL correctly
    -- (NULL = NULL is NULL in SQL, but NULL IS NOT DISTINCT FROM NULL is TRUE).
    IF NEW.outcome IS DISTINCT FROM OLD.outcome THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify outcome (decision_id=%)', OLD.id;
    END IF;
    IF NEW.reasoning IS DISTINCT FROM OLD.reasoning THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify reasoning (decision_id=%)', OLD.id;
    END IF;
    IF NEW.confidence IS DISTINCT FROM OLD.confidence THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify confidence (decision_id=%)', OLD.id;
    END IF;
    IF NEW.decision_type IS DISTINCT FROM OLD.decision_type THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify decision_type (decision_id=%)', OLD.id;
    END IF;
    IF NEW.agent_id IS DISTINCT FROM OLD.agent_id THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify agent_id (decision_id=%)', OLD.id;
    END IF;
    IF NEW.run_id IS DISTINCT FROM OLD.run_id THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify run_id (decision_id=%)', OLD.id;
    END IF;
    IF NEW.org_id IS DISTINCT FROM OLD.org_id THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify org_id (decision_id=%)', OLD.id;
    END IF;
    IF NEW.content_hash IS DISTINCT FROM OLD.content_hash THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify content_hash (decision_id=%)', OLD.id;
    END IF;
    IF NEW.valid_from IS DISTINCT FROM OLD.valid_from THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify valid_from (decision_id=%)', OLD.id;
    END IF;
    IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify created_at (decision_id=%)', OLD.id;
    END IF;
    IF NEW.transaction_time IS DISTINCT FROM OLD.transaction_time THEN
        RAISE EXCEPTION 'decisions row is immutable: cannot modify transaction_time (decision_id=%)', OLD.id;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_decisions_immutable ON decisions;
CREATE TRIGGER trg_decisions_immutable
BEFORE UPDATE ON decisions
FOR EACH ROW
EXECUTE FUNCTION decisions_immutable_guard();
