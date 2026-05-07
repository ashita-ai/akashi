-- 104: Add multi-target decision supersedes relationship table.
CREATE TABLE decision_supersedes (
    superseding_id UUID NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
    superseded_id  UUID NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
    org_id         UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    relationship   TEXT NOT NULL DEFAULT 'supersedes',
    is_primary     BOOLEAN NOT NULL DEFAULT FALSE,
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (superseding_id, superseded_id),
    CONSTRAINT decision_supersedes_not_self CHECK (superseding_id <> superseded_id),
    CONSTRAINT decision_supersedes_relationship_check CHECK (relationship IN ('supersedes', 'reconciles'))
);

CREATE INDEX idx_decision_supersedes_superseded
    ON decision_supersedes(org_id, superseded_id);

CREATE INDEX idx_decision_supersedes_superseding
    ON decision_supersedes(org_id, superseding_id);

CREATE UNIQUE INDEX idx_decision_supersedes_primary
    ON decision_supersedes(superseding_id)
    WHERE is_primary;

INSERT INTO decision_supersedes (superseding_id, superseded_id, org_id, relationship, is_primary)
SELECT id, supersedes_id, org_id, 'supersedes', TRUE
FROM decisions
WHERE supersedes_id IS NOT NULL
ON CONFLICT (superseding_id, superseded_id) DO NOTHING;

CREATE OR REPLACE FUNCTION sync_decision_supersedes_primary()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.supersedes_id IS NULL THEN
        UPDATE decision_supersedes
        SET is_primary = FALSE
        WHERE superseding_id = NEW.id
          AND org_id = NEW.org_id
          AND is_primary;
        RETURN NEW;
    END IF;

    UPDATE decision_supersedes
    SET is_primary = FALSE
    WHERE superseding_id = NEW.id
      AND org_id = NEW.org_id
      AND superseded_id <> NEW.supersedes_id
      AND is_primary;

    INSERT INTO decision_supersedes (superseding_id, superseded_id, org_id, relationship, is_primary)
    VALUES (NEW.id, NEW.supersedes_id, NEW.org_id, 'supersedes', TRUE)
    ON CONFLICT (superseding_id, superseded_id) DO UPDATE
    SET is_primary = TRUE;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_decisions_sync_supersedes ON decisions;
CREATE TRIGGER trg_decisions_sync_supersedes
AFTER INSERT OR UPDATE OF supersedes_id ON decisions
FOR EACH ROW
EXECUTE FUNCTION sync_decision_supersedes_primary();
