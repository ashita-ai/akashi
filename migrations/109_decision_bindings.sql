-- 109: Bindings — the named thing a decision sets, and the value it sets it to.
--
-- Conflict detection between prose decisions is a screening problem at a ~3%
-- base rate, and the judge is the only component that discriminates. Bindings
-- are the exception: when two decisions bind the SAME named parameter to
-- DIFFERENT values, the conflict is a join, not an inference — no model, no
-- threshold, no false-positive rate.
--
-- Classifying all 93 gold contradictions in the corpus by shape found 27% have
-- exactly this form (a named parameter with two incompatible values). The other
-- 73% are mutually-exclusive actions (34%), incompatible factual claims (30%)
-- and incompatible strategic direction (9%), which have no key to join on and
-- still need the judge. So this covers roughly a quarter of real contradictions
-- exactly, and is worthless for the rest — both halves of that are the point.
--
-- Why capture rather than extract: four separate attempts to recover this
-- structure from decision prose after the fact were measured on the corpus and
-- all failed (typed-artifact token join 1.30x lift, rejected-alternative token
-- join 1.03x, rejected-alternative semantic join AUC 0.576, logprob tension
-- score AUC 0.669). 59% of contradictions never name the artifact in their
-- outcome text at all. The identity has to be declared at write time; it cannot
-- be reconstructed. This mirrors what every system that ships exact conflict
-- detection does — Kubernetes server-side apply keys on a JSON field path,
-- Terraform on attribute paths, OPA on a document path — none of them infer the
-- referent, and Cedar was deliberately restricted so comparison stays decidable.

CREATE TABLE IF NOT EXISTS decision_bindings (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    decision_id  UUID NOT NULL REFERENCES decisions(id) ON DELETE CASCADE,
    org_id       UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,

    -- parameter is what the agent wrote, preserved for display.
    parameter    TEXT NOT NULL,
    -- parameter_key is the canonical form the join runs on. Canonicalization is
    -- deliberately shallow (case, surrounding whitespace, separator style):
    -- aggressive normalization would merge genuinely different parameters and
    -- manufacture conflicts, which is a worse failure than missing one.
    parameter_key TEXT NOT NULL,

    -- value as written, and its canonical form. Values are compared, never
    -- interpreted: "5m" and "300s" are different values here, because deciding
    -- they are the same requires knowing the parameter's type, which we do not.
    value        TEXT NOT NULL,
    value_key    TEXT NOT NULL,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT decision_bindings_parameter_not_blank CHECK (length(btrim(parameter)) > 0),
    CONSTRAINT decision_bindings_value_not_blank     CHECK (length(btrim(value)) > 0),
    -- One decision cannot bind the same parameter twice; the later write would
    -- otherwise silently conflict with itself.
    CONSTRAINT decision_bindings_unique_per_decision UNIQUE (decision_id, parameter_key)
);

-- The detection query is: same org, same parameter_key, different value_key.
-- org_id leads because every query in this codebase is org-scoped.
CREATE INDEX IF NOT EXISTS idx_decision_bindings_lookup
    ON decision_bindings (org_id, parameter_key, value_key);

CREATE INDEX IF NOT EXISTS idx_decision_bindings_decision
    ON decision_bindings (decision_id);

COMMENT ON TABLE decision_bindings IS
    'Named parameter a decision sets and the value it sets it to. Two decisions binding the same parameter_key to different value_keys are in exact conflict, detected by join rather than by the LLM judge.';
