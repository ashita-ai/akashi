-- 055: GDPR tombstone erasure — decision_erasures table and trigger bypass.
--
-- GDPR Article 17 requires scrubbing PII from decision rows without deleting
-- the row itself (the audit chain must survive). The existing immutability
-- trigger (migration 036) blocks updates to outcome, reasoning, content_hash.
-- This migration:
--
-- 1. Creates a decision_erasures table that records which decisions were erased,
--    by whom, and preserves the original content_hash for forensic verification.
--
-- 2. Modifies the decisions_immutable_guard() trigger function to check the
--    session-scoped variable akashi.erasure_in_progress. When set to 'true'
--    via SET LOCAL (transaction-scoped), the trigger permits updates to
--    outcome, reasoning, and content_hash — the three fields scrubbed during
--    erasure. SET LOCAL auto-resets on commit/rollback, so the bypass cannot
--    leak across transactions.
--
-- The erasure transaction must:
--   SET LOCAL akashi.erasure_in_progress = 'true';
--   UPDATE decisions SET outcome='[erased]', reasoning='[erased]', content_hash=<recomputed>;
--   UPDATE alternatives SET label='[erased]', rejection_reason='[erased]';
--   UPDATE evidence SET content='[erased]', source_uri=NULL;
--   INSERT INTO decision_erasures (...);

CREATE TABLE decision_erasures (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id     UUID        NOT NULL REFERENCES decisions(id),
    org_id          UUID        NOT NULL REFERENCES organizations(id),
    erased_by       TEXT        NOT NULL,
    original_hash   TEXT        NOT NULL,
    erased_hash     TEXT        NOT NULL,
    reason          TEXT        NOT NULL DEFAULT '',
    erased_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(decision_id)
);

CREATE INDEX idx_decision_erasures_org ON decision_erasures(org_id, erased_at DESC);
CREATE INDEX idx_decision_erasures_decision ON decision_erasures(decision_id);

-- Modify the immutability trigger to allow erasure bypass via session variable.
-- current_setting('akashi.erasure_in_progress', true) returns NULL when the
-- variable is not set (the second argument = true means "missing_ok"). Only
-- when explicitly set to 'true' within a transaction are the PII fields unlocked.
CREATE OR REPLACE FUNCTION decisions_immutable_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- GDPR erasure bypass: when the session variable is set within a transaction
    -- via SET LOCAL, permit updates to the three PII/hash fields only.
    IF current_setting('akashi.erasure_in_progress', true) = 'true' THEN
        -- Even during erasure, protect structural fields that are NOT PII.
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
        IF NEW.valid_from IS DISTINCT FROM OLD.valid_from THEN
            RAISE EXCEPTION 'decisions row is immutable: cannot modify valid_from (decision_id=%)', OLD.id;
        END IF;
        IF NEW.created_at IS DISTINCT FROM OLD.created_at THEN
            RAISE EXCEPTION 'decisions row is immutable: cannot modify created_at (decision_id=%)', OLD.id;
        END IF;
        IF NEW.transaction_time IS DISTINCT FROM OLD.transaction_time THEN
            RAISE EXCEPTION 'decisions row is immutable: cannot modify transaction_time (decision_id=%)', OLD.id;
        END IF;
        IF NEW.confidence IS DISTINCT FROM OLD.confidence THEN
            RAISE EXCEPTION 'decisions row is immutable: cannot modify confidence (decision_id=%)', OLD.id;
        END IF;
        -- outcome, reasoning, content_hash are permitted during erasure.
        RETURN NEW;
    END IF;

    -- Normal path: full immutability enforcement (unchanged from migration 036).
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
