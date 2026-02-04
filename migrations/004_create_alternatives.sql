-- 004_create_alternatives.sql
-- Alternatives considered for each decision. Immutable.

CREATE TABLE alternatives (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id     UUID NOT NULL REFERENCES decisions(id),
    label           TEXT NOT NULL,
    score           REAL,
    selected        BOOLEAN NOT NULL DEFAULT false,
    rejection_reason TEXT,
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_alternatives_decision_id ON alternatives(decision_id);
CREATE INDEX idx_alternatives_selected ON alternatives(decision_id) WHERE selected = true;
