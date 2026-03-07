-- 060: Ground truth labels for conflict detection precision/recall measurement.
-- Separation of concerns: labels live outside scored_conflicts so the scorer
-- pipeline is unaware of evaluation data.

CREATE TABLE conflict_labels (
    scored_conflict_id uuid NOT NULL REFERENCES scored_conflicts(id) ON DELETE CASCADE,
    org_id             uuid NOT NULL,
    label              text NOT NULL CHECK (label IN ('genuine', 'related_not_contradicting', 'unrelated_false_positive')),
    labeled_by         text NOT NULL,
    labeled_at         timestamptz NOT NULL DEFAULT now(),
    notes              text,
    PRIMARY KEY (scored_conflict_id)
);

CREATE INDEX idx_conflict_labels_org ON conflict_labels (org_id);
CREATE INDEX idx_conflict_labels_label ON conflict_labels (label);

COMMENT ON TABLE conflict_labels IS 'Ground truth labels for conflict detection evaluation. Each row maps a scored_conflict to a human-verified label.';
COMMENT ON COLUMN conflict_labels.label IS 'genuine = true contradiction or supersession; related_not_contradicting = same topic, no real conflict; unrelated_false_positive = should not have been detected';
