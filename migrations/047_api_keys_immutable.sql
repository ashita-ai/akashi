-- 047: Immutability trigger for api_keys core fields.
--
-- Prevents modification of identity and credential fields via direct SQL.
-- Mutable fields (label, revoked_at, last_used_at, expires_at) remain writable
-- because they represent lifecycle state, not the key's identity.
--
-- Pattern mirrors decisions_immutable_guard() (migration 036) and
-- mutation_audit_log_immutable_guard() (migration 031).

CREATE OR REPLACE FUNCTION api_keys_immutable_guard()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id         IS DISTINCT FROM OLD.id         OR
       NEW.key_hash   IS DISTINCT FROM OLD.key_hash   OR
       NEW.prefix     IS DISTINCT FROM OLD.prefix     OR
       NEW.agent_id   IS DISTINCT FROM OLD.agent_id   OR
       NEW.org_id     IS DISTINCT FROM OLD.org_id     OR
       NEW.created_at IS DISTINCT FROM OLD.created_at OR
       NEW.created_by IS DISTINCT FROM OLD.created_by
    THEN
        RAISE EXCEPTION
            'api_keys: immutable fields cannot be modified (id, key_hash, prefix, agent_id, org_id, created_at, created_by). Revoke and create a new key instead.';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_api_keys_immutable ON api_keys;
CREATE TRIGGER trg_api_keys_immutable
    BEFORE UPDATE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION api_keys_immutable_guard();
