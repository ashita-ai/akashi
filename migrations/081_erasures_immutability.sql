-- 081: Add immutability triggers to decision_erasures.
-- Erasure records are compliance proof that GDPR obligations were met.
-- They must not be modified or deleted after creation, matching the
-- pattern established for integrity_violations in migration 079.

CREATE OR REPLACE FUNCTION prevent_erasure_update()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'decision_erasures rows are immutable — updates are prohibited';
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION prevent_erasure_delete()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'decision_erasures rows are immutable — deletes are prohibited';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_decision_erasures_no_update
    BEFORE UPDATE ON decision_erasures
    FOR EACH ROW
    EXECUTE FUNCTION prevent_erasure_update();

CREATE TRIGGER trg_decision_erasures_no_delete
    BEFORE DELETE ON decision_erasures
    FOR EACH ROW
    EXECUTE FUNCTION prevent_erasure_delete();
