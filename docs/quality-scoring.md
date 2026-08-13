# Quality Scoring

Akashi assigns two quality signals to every decision: a **completeness score** computed
at write time, and an **outcome score** derived from human assessments over time.

## Completeness score

The completeness score (0.0–1.0) measures how thoroughly a decision trace was filled out.
It is computed when the decision is created and does not change afterward.

### Scoring factors

Scoring is **uniform across all decision types**. A score of 0.55 means the same thing
whether it's an investigation or a security decision. This preserves cross-type
comparability and prevents stored score discontinuities.

| Factor | Max contribution | Scoring tiers |
|--------|-----------------|---------------|
| **Confidence** | 0.15 | 0.15 if mid-range (0.05 < c < 0.95); 0.10 at edges (0 < c ≤ 0.05 or 0.95 ≤ c < 1); 0.0 if exactly 0 or 1 |
| **Reasoning** | 0.30 | 0.30 if > 100 chars; 0.24 if > 50; 0.12 if > 20; 0.0 otherwise |
| **Alternatives** | 0.20 | Counts non-selected alternatives with substantive rejection reasons (> 20 chars). 0.20 for ≥ 3; 0.15 for 2; 0.10 for 1; 0.0 for none |
| **Evidence** | 0.15 | 0.15 for ≥ 2 items; 0.10 for 1; 0.0 for none |
| **Outcome** | 0.10 | 0.10 for 21–300 chars; 0.07 for 301–500; 0.04 above 500; 0.0 for ≤ 20 |
| **Precedent ref** | 0.10 | 0.10 if precedent_ref set; 0.0 otherwise |

**Maximum possible score: 1.00** (0.90 from content + 0.10 from precedent_ref)

The outcome factor is the one tier that goes **down** as the field gets longer. An `outcome`
is meant to be the decision stated as a fact; past ~300 characters it is almost always a
change log pasted into the wrong field, so it earns less. Put the narrative in `reasoning`,
which is rewarded for length.

### Per-type differentiation

Instead of changing the scoring formula per type (which would break comparability and
create stored score discontinuities), Akashi differentiates decision types in two ways:

#### 1. Profile-aware completeness tips

Agents get tips tailored to their decision type. An investigation agent won't be told
"add alternatives" because that's not meaningful for investigations. A security agent
will get evidence nudges because evidence matters for security decisions.

| Decision type | Evidence tips | Alternatives tips | Confidence warning |
|---|---|---|---|
| `investigation` | suppressed | suppressed | if > 0.90 without evidence |
| `planning` | suppressed | suppressed | if > 0.85 without evidence |
| `code_review` | shown (min 1 item) | shown | if > 0.85 without evidence |
| `architecture` | shown (min 2 items) | shown | if > 0.80 without evidence |
| `security` | shown (min 2 items) | shown | if > 0.75 without evidence |
| _any other type_ | shown (min 1 item) | shown | never |

> **`AKASHI_COMPLETENESS_PROFILES` is currently inert.** The variable is parsed at startup
> into `config.CompletenessProfilesJSON` and then never read: the only caller of
> `quality.ProfileFor` passes `nil` overrides (`internal/mcp/tools.go`). Setting it changes
> nothing today. Tracked in [#765](https://github.com/ashita-ai/akashi/issues/765) — either
> wire it through or delete it. Until then, the table above is the whole story.

#### 2. Per-type health thresholds

The `completeness_by_type` breakdown in trace-health enriches each type with an
`expected_min` threshold and `status` ("healthy" or "needs_attention"). This surfaces
which decision types fall below expectations for that type's importance level.

| Decision type | Expected minimum |
|---|---|
| `investigation` | 0.30 |
| `planning` | 0.30 |
| `assessment` | 0.30 |
| `code_review` | 0.45 |
| `architecture` | 0.55 |
| `trade_off` | 0.55 |
| `security` | 0.60 |

A security decision averaging 0.35 completeness shows as "needs_attention" while an
investigation averaging 0.35 shows as "healthy." Same score, different expectation.

### Standard decision types

The following types are considered "standard" for suggestion purposes: `model_selection`,
`architecture`, `data_source`, `error_handling`, `feature_scope`, `trade_off`, `deployment`,
`security`, `code_review`, `investigation`, `planning`, `assessment`.

Custom types are fully supported with no scoring penalty. Override the standard list per org
via `AKASHI_STANDARD_DECISION_TYPES` (comma-separated).

### Type canonicalization (this rewrites your input)

Before scoring, `decision_type` is lowercased, trimmed, and then canonicalized:

1. If the org has a stored alias for the value (`decision_type_aliases`), the canonical type
   replaces it.
2. Otherwise, if the value is within Levenshtein distance 2 of a standard type, the standard
   type replaces it **and a durable alias is created** once the trace commits, so every later
   trace using that spelling is redirected the same way.

Either way the original string is preserved on the decision as
`metadata.original_decision_type`. So `trade-off` is not merely flagged — it is stored as
`trade_off`. If you want a genuinely distinct custom type, keep it more than two edits away
from every entry in the standard list above, or add it to `AKASHI_STANDARD_DECISION_TYPES`.

A suggestion tip is separately shown for near-misses at distance ≤ 3, which is wider than the
rewrite threshold: distance-3 types are named in a tip but left alone.

### Anti-gaming measures

The scoring formula includes deliberate anti-gaming rules:

- **Rejection reasons required**: Alternatives only count toward the score if their
  rejection reason is > 20 characters. Padding with "n/a" or empty strings doesn't help.
- **Selected alternatives ignored**: Only non-selected alternatives count — selecting
  everything is not rewarded.
- **Confidence boundaries penalized**: Exactly 0.05 or 0.95 falls to a lower tier than
  the mid-range, discouraging mechanical boundary values.
- **Outcome length capped**: Past 300 characters the outcome factor decreases, so pasting a
  commit message into `outcome` costs score rather than earning it.
- **Whitespace trimmed**: All character counts apply after trimming.

### Calibration status

All weights are currently uncalibrated — chosen by hand without empirical basis. A future
iteration will fit weights against assessed decision data. See the factor table as a
guide to what Akashi values in a decision trace, not as a precise quality metric.

## Confidence deflation

Completeness is not the only thing computed at write time. The `confidence` you send is
**adjusted before it is stored**, because agents systematically over-report it (mean 0.867
across the 824 decisions the rule was fit on). Three rules apply independently and stack:

| Condition | Adjustment |
|---|---|
| `confidence ≥ 0.80` and zero evidence items | −0.15 |
| `confidence ≥ 0.85` and zero alternatives | −0.10 |
| `confidence ≥ 0.80` and reasoning under 50 chars | −0.10 |

The result is floored at 0.30, so a sparse trace lands at 0.30 rather than 0.00. Values
outside `[0, 1]` pass through untouched so the database CHECK constraint catches them.

Nothing is lost: the submitted value is preserved on the decision as
`metadata.original_confidence`, with `metadata.confidence_adjustment_reasons` naming which
rules fired. The MCP `akashi_trace` response also returns `confidence_adjusted`,
`original_confidence`, `stored_confidence`, and `confidence_reasons` when an adjustment
happened, so an agent can see it in the same turn.

Two consequences worth knowing:

- Deflation runs **after** the completeness score and the ingest gate below, so neither sees
  the deflated value — the gate judges what you submitted.
- Downstream consumers read the **stored** value, so they see the deflated one — the
  conflict scorer's `confidence_weight` term, the `confidence_min` query filter, and the
  Qdrant `confidence` payload index. (Search re-ranking does not use confidence at all; see
  below.)

The only way to keep a high confidence is to support it: attach evidence, list alternatives,
and write more than 50 characters of reasoning.

## Ingest gate (#715)

The completeness score also feeds an optional ingest gate. When enabled, the
gate refuses or warns on traces whose score falls below a configured
threshold for their decision type. The gate is purely structural — it consumes
the score above, no new logic — and is disabled by default.

| Mode | Behavior on score below threshold |
|------|-----------------------------------|
| `off` (default) | No effect; pre-#715 behavior preserved |
| `warn` | Trace is persisted; a string is appended to the response `warnings` array |
| `reject` | Trace is refused; HTTP `422 Unprocessable Entity` with `COMPLETENESS_BELOW_THRESHOLD` error code and `details` carrying score/threshold/type. The MCP `akashi_trace` tool returns the same information as a tool-error message. The rejected attempt is recorded in `mutation_audit_log` with operation `trace_decision_rejected` |

Per-type overrides take precedence over the global floor. Configure via
`AKASHI_MIN_COMPLETENESS`, `AKASHI_MIN_COMPLETENESS_MODE`, and
`AKASHI_MIN_COMPLETENESS_BY_TYPE` — see [configuration.md](configuration.md#completeness-ingest-gate).

Gate activity is observable via two OTel counters labeled by `decision_type`:

- `akashi.trace.completeness_gate_rejects`
- `akashi.trace.completeness_gate_warns`

## Outcome score

The outcome score (0.0–1.0, or `null`) measures how correct a decision turned out to be
based on human assessments recorded via `POST /v1/decisions/{id}/assess`.

### Formula

```
outcome_score = (correct + 0.5 × partially_correct) / total_assessments
```

- Returns `null` when no assessments exist (not 0.0 — absence of feedback is different
  from negative feedback).
- Updated each time a new assessment is recorded.
- Partially correct assessments contribute half weight, preserving nuance.

### How it influences search

The outcome score feeds into the search re-ranking formula as the **assessment signal**, the
largest of the five outcome signals at 40%. Decisions assessed as correct rank higher in
`akashi_check` results, creating a feedback loop: decisions that turned out to be right
surface more often as precedents. An unassessed decision contributes 0 here rather than a
neutral score — absence of feedback is not a middling grade.

**Completeness does not affect ranking.** It was deliberately removed from the relevance
formula: filling in evidence and alternatives says how thorough a trace is, not whether the
decision was correct, and rewarding it in search would have let a well-formatted wrong answer
outrank a terse right one. The authoritative formula is the doc comment on `ReScore` in
`internal/search/search.go`.

## Recording assessments

```
POST /v1/decisions/{id}/assess
```

```json
{
  "assessment": "correct",
  "note": "Approach worked well in production"
}
```

Valid assessment values: `correct`, `partially_correct`, `incorrect`.

Assessments are append-only — multiple assessments from different agents accumulate to
form the outcome score. This allows diverse perspectives (the implementing agent, a
reviewer, a post-mortem analysis) to all contribute.
