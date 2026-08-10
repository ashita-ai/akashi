-- 107: Blind gold labels for conflict-detector evaluation.
--
-- Distinct from conflict_labels, which records adjudication outcomes produced
-- while resolving the queue. Those labels inherit the resolver's context and
-- workflow (bulk clears, cascades, auto-supersession), which makes them
-- unusable as ground truth: a 2026-08-10 audit found the resolved-with-winner
-- set was only 11% genuine contradictions, and 2,296 rows came from untriaged
-- bulk clears.
--
-- Gold labels are assigned blind — the rater sees the two decision texts and
-- their structural metadata, never the pipeline's verdict or any prior label —
-- so they can falsify the detector rather than echo it. The four-way taxonomy
-- separates supersession (one work stream evolving) from contradiction (two
-- live incompatible positions); conflating them was the detector's dominant
-- error, at 22.6% of the corpus.

-- ON DELETE CASCADE is correct for a genuine deletion: a label describing a
-- pair that no longer exists is meaningless. It is NOT safe for the bulk clears
-- that run at startup after a prompt change, which would take the labelled
-- corpus with them and leave no trace, since the mutation-audit row counts
-- scored_conflicts rather than labels. Those two paths therefore exclude
-- labelled rows explicitly -- see ClearUnvalidatedConflicts and
-- ClearAllConflicts in internal/conflicts/scorer.go. If a third bulk-delete
-- path is ever added, it needs the same exclusion.
CREATE TABLE IF NOT EXISTS conflict_gold_labels (
    scored_conflict_id UUID PRIMARY KEY REFERENCES scored_conflicts(id) ON DELETE CASCADE,
    org_id             UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    label              TEXT NOT NULL,
    rationale          TEXT,
    -- Provenance of the label, e.g. 'blind_llm_fullcorpus_v1' or 'human_v1'.
    -- Required so a later run can weight or exclude a labeling method without
    -- guessing from timestamps, which is the failure that made conflict_labels
    -- unusable.
    method             TEXT NOT NULL,
    labeled_by         TEXT NOT NULL,
    labeled_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT conflict_gold_labels_label_check
        CHECK (label IN ('contradiction', 'supersession', 'related_not_contradicting', 'unrelated', 'insufficient'))
);

-- Eval loads run per org and filter out 'insufficient'.
CREATE INDEX IF NOT EXISTS idx_conflict_gold_labels_org_label
    ON conflict_gold_labels (org_id, label);

-- Reporting label-set composition by provenance.
CREATE INDEX IF NOT EXISTS idx_conflict_gold_labels_method
    ON conflict_gold_labels (org_id, method);
