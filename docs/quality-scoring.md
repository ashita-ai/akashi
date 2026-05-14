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
| **Outcome** | 0.10 | 0.10 if > 20 chars; 0.0 otherwise |
| **Precedent ref** | 0.10 | 0.10 if precedent_ref set; 0.0 otherwise |

**Maximum possible score: 1.00** (0.90 from content + 0.10 from precedent_ref)

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
| `code_review` | shown | shown | if > 0.85 without evidence |
| `architecture` | shown | shown | if > 0.80 without evidence |
| `security` | shown | shown | if > 0.75 without evidence |

Override via `AKASHI_COMPLETENESS_PROFILES` env var (JSON map):
```
AKASHI_COMPLETENESS_PROFILES='{"security":{"min_evidence":3,"alternatives_expected":true,"max_confidence_no_evidence":0.70}}'
```

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

Custom types are fully supported with no scoring penalty. When a non-standard type is close
to a standard one (e.g. `trade-off` vs `trade_off`), a suggestion tip is shown.

Override the standard list per org via `AKASHI_STANDARD_DECISION_TYPES` (comma-separated).

### Anti-gaming measures

The scoring formula includes deliberate anti-gaming rules:

- **Rejection reasons required**: Alternatives only count toward the score if their
  rejection reason is > 20 characters. Padding with "n/a" or empty strings doesn't help.
- **Selected alternatives ignored**: Only non-selected alternatives count — selecting
  everything is not rewarded.
- **Confidence boundaries penalized**: Exactly 0.05 or 0.95 falls to a lower tier than
  the mid-range, discouraging mechanical boundary values.
- **Whitespace trimmed**: All character counts apply after trimming.

### Calibration status

All weights are currently uncalibrated — chosen by hand without empirical basis. A future
iteration will fit weights against assessed decision data. See the factor table as a
guide to what Akashi values in a decision trace, not as a precise quality metric.

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

The outcome score feeds into the search re-ranking formula as the **assessment signal**
(weight: 40%). Decisions assessed as correct rank higher in `akashi_check` results,
creating a feedback loop: good decisions surface more often as precedents.

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
