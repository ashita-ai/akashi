-- 035: Conflict lifecycle — categories, severity, resolution (ADR-015).
--
-- category: what kind of disagreement (factual, assessment, strategic, temporal).
-- severity: impact level (critical, high, medium, low).
-- status: lifecycle state (open, acknowledged, resolved, wont_fix).
-- resolved_by/resolved_at/resolution_note: resolution metadata.

ALTER TABLE scored_conflicts
    ADD COLUMN IF NOT EXISTS category TEXT CHECK (category IN ('factual', 'assessment', 'strategic', 'temporal')),
    ADD COLUMN IF NOT EXISTS severity TEXT CHECK (severity IN ('critical', 'high', 'medium', 'low')),
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'open'
        CHECK (status IN ('open', 'acknowledged', 'resolved', 'wont_fix')),
    ADD COLUMN IF NOT EXISTS resolved_by TEXT,
    ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resolution_note TEXT;

-- Partial index for open conflicts — the hot path for akashi_check.
CREATE INDEX IF NOT EXISTS idx_scored_conflicts_status
    ON scored_conflicts(org_id, status) WHERE status = 'open';
