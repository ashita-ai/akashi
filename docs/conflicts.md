# Conflict Detection and Resolution

Akashi automatically detects when AI agents make contradictory decisions. This document
covers the full conflict pipeline: how conflicts are found, how they're scored, and how
to resolve them.

## Overview

When a decision is recorded via `POST /v1/trace`, Akashi asynchronously:

1. **Retrieves candidates** — finds semantically similar past decisions via Qdrant (or
   brute-force cosine similarity in local-lite mode — coming soon, see issue #312).
2. **Scores significance** — computes a composite score from topic similarity, outcome
   divergence, confidence, and temporal decay.
3. **Validates via LLM** (optional) — classifies the relationship as contradiction,
   supersession, complementary, refinement, or unrelated.
4. **Persists conflicts** — only contradictions and supersessions are stored.
5. **Notifies** — fires a `LISTEN/NOTIFY` event on the `akashi_conflicts` channel.

## Significance scoring

Every candidate pair receives a significance score:

```
significance = topic_similarity × outcome_divergence × confidence_weight × temporal_decay
```

| Component | Formula | Meaning |
|-----------|---------|---------|
| Topic similarity | `cosine(embedding_A, embedding_B)` | How related the decisions are |
| Outcome divergence | `1 - cosine(outcome_emb_A, outcome_emb_B)` | How much the conclusions differ |
| Confidence weight | `sqrt(confidence_A × confidence_B)` | Penalizes low-confidence decisions |
| Temporal decay | `exp(-lambda × days_between)` | Older conflicts matter less |

The default significance threshold is **0.30** — pairs below this are not persisted.

## Claim-level scoring

When two decisions have high topic similarity (≥ 0.70), Akashi also performs claim-level
analysis. Each outcome is split into individual claims, and claim pairs are scored
independently. If claim-level scoring produces a higher significance than full-outcome
scoring, the claim-level result wins.

### Claim extraction

Claims are extracted using one of two strategies:

- **Regex extraction** (default): Splits on markdown lists, sentence boundaries, numbered
  items, colons, and semicolons. Filters boilerplate phrases ("LGTM", "all tests pass",
  etc.) and deduplicates. Minimum claim length: 20 characters.

- **LLM extraction** (opt-in via `AKASHI_CLAIM_EXTRACTION_LLM=true`): Uses the configured
  conflict LLM (Ollama or OpenAI) to extract structured claims with categories:

  | Category | Participates in conflict scoring? |
  |----------|-----------------------------------|
  | `finding` | Yes |
  | `assessment` | Yes |
  | `recommendation` | No (informational only) |
  | `status` | No (informational only) |

  LLM extraction produces higher-quality claims but adds latency and cost. Use it when
  precision matters more than throughput.

### Winning claim pairs

When claim-level scoring wins, the specific `claim_text_a` and `claim_text_b` that
produced the highest score are persisted on the conflict record. These appear in the UI
to help reviewers understand exactly which statements are in tension.

## LLM validation

When an LLM validator is configured (Ollama or OpenAI), candidate pairs that pass the
significance threshold are classified into relationship types:

| Relationship | Meaning | Creates conflict? |
|-------------|---------|-------------------|
| `contradiction` | Incompatible positions on the same question | Yes |
| `supersession` | One decision explicitly replaces the other | Yes |
| `complementary` | Different findings about different aspects | No |
| `refinement` | One deepens the other without contradicting | No |
| `unrelated` | Different topics despite surface similarity | No |

The validator also assigns:

- **Category**: `factual`, `assessment`, `strategic`, or `temporal`
- **Severity**: `critical`, `high`, `medium`, or `low`

### False positive reduction

The LLM prompt includes contextual signals that reduce false positives:

- **Cross-project**: Decisions from different projects are flagged as almost certainly unrelated.
- **Same session**: Sequential decisions in the same session (e.g., review → fix) are
  hinted as likely refinement or complementary.
- **Workflow pairs**: Known cause-and-effect patterns (code_review → bug_fix, assessment →
  implementation) are flagged.

## Performance optimizations

### Early exit

When `AKASHI_CONFLICT_EARLY_EXIT_FLOOR` > 0 (default: 0.25), candidates are sorted by
pre-computed significance in descending order. Candidates below the floor are skipped
unless they qualify for "direct-to-scorer" bypass (high topic similarity pairs that only
an LLM can properly classify).

### Cross-encoder reranking

An optional cross-encoder can pre-filter candidates before LLM validation, reducing LLM
calls by 50–80%. Configure with:

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_CROSS_ENCODER_URL` | _(empty)_ | Cross-encoder service endpoint. Empty = disabled. |
| `AKASHI_CROSS_ENCODER_THRESHOLD` | `0.50` | Minimum cross-encoder score to proceed to LLM. |

Pairs below the threshold are skipped without an LLM call. If the cross-encoder service
is unreachable, the system falls open to LLM validation (no silent suppression).

### Revision chain exclusion

Decisions linked by `supersedes_id` (intentional revisions) are excluded from conflict
detection. The scorer walks both forward and backward through the revision chain (up to
100 hops) to find all related decisions.

## Conflict lifecycle

| Status | Meaning | Transitions to |
|--------|---------|----------------|
| `open` | Detected, awaiting action | `resolved`, `false_positive` |
| `resolved` | Resolved with declared winner | Terminal |
| `false_positive` | Dismissed as not a real conflict | Terminal |

## Resolution methods

### Simple status update

```
PATCH /v1/conflicts/{id}
```

```json
{
  "status": "resolved",
  "resolution_note": "Team agreed on approach A",
  "winning_decision_id": "uuid-of-winning-decision"
}
```

### Adjudication trace

```
POST /v1/conflicts/{id}/adjudicate
```

```json
{
  "outcome": "Chose microservices based on team capacity",
  "reasoning": "Monolith would require rewrite of auth layer",
  "winning_decision_id": "uuid-of-winning-decision"
}
```

This atomically creates a new decision (the adjudication trace) and resolves the conflict.
The adjudication itself becomes part of the audit trail.

### Batch group resolution

Conflicts between the same agent pair, decision type, and conflict kind are grouped.
Resolve all open conflicts in a group with one call:

```
PATCH /v1/conflict-groups/{id}/resolve
```

```json
{
  "status": "resolved",
  "resolution_note": "Cleaning up stale conflicts after architecture review",
  "winning_agent": "planner-agent"
}
```

When `winning_agent` is provided, the winning decision for each conflict is automatically
derived from the agent columns.

### Auto-resolve on revision

When a decision is revised (superseded by a new version), all open conflicts involving
the old decision are automatically resolved. This prevents stale conflicts from
accumulating when agents correct themselves.

## Resolution recommendations

For unresolved conflicts, `GET /v1/conflicts/{id}` returns an optional `recommendation`
field computed from four heuristic signals:

| Signal | Weight | Logic |
|--------|--------|-------|
| Confidence delta | 0.35 | Higher-confidence decision favored |
| Recency | 0.25 | More recent decision favored (tanh-normalized over 1 week) |
| Agent win rate | 0.25 | Agent's historical success rate on this decision type (requires ≥ 3 prior resolutions) |
| Revision depth | 0.15 | Deeper revision chain = more refined position (tanh-normalized at depth 3) |

Recommendations are computed on-demand (not stored) and only returned when the composite
signal exceeds 0.10. Each recommendation includes human-readable reasons sorted by
contribution magnitude.

The UI surfaces recommendations in the adjudicate dialog with a one-click "Accept
recommendation" button.

## Analytics

```
GET /v1/conflicts/analytics
```

Returns aggregated conflict metrics over a time window:

- **Summary**: Total detected, total resolved, mean time to resolution, false-positive rate
- **Breakdowns**: By agent pair, decision type, severity
- **Trend**: Daily detected-vs-resolved counts

### Query parameters

| Param | Values | Default | Description |
|-------|--------|---------|-------------|
| `period` | `7d`, `30d`, `90d` | `7d` | Convenience time window |
| `from` / `to` | RFC 3339 timestamps | — | Custom range (overrides `period`, max 365 days) |
| `agent_id` | string | — | Filter by agent |
| `decision_type` | string | — | Filter by type |
| `conflict_kind` | `cross_agent`, `self_contradiction` | — | Filter by kind |

## Observability

When OpenTelemetry is configured, the conflict pipeline emits these metrics:

### Counters

| Metric | Attributes | Description |
|--------|------------|-------------|
| `akashi.conflicts.detected` | scoring_method, relationship, conflict_kind, severity | Conflicts found |
| `akashi.conflicts.resolved` | status, conflict_kind | Conflicts resolved |
| `akashi.conflicts.llm_calls` | result (success/error/timeout), validator | LLM validation calls |
| `akashi.conflicts.candidates_evaluated` | — | Candidate pairs evaluated |
| `akashi.conflicts.claim_level_wins` | — | Times claim scoring beat full-outcome scoring |

### Histograms

| Metric | Unit | Description |
|--------|------|-------------|
| `akashi.conflicts.scoring_duration_ms` | ms | Per-decision conflict scoring latency |
| `akashi.conflicts.llm_call_duration_ms` | ms | Per-call LLM validation latency |
| `akashi.conflicts.significance_distribution` | — | Significance scores of detected conflicts |
| `akashi.conflicts.candidates_examined` | — | Candidates examined after early exit |

### Gauges

| Metric | Description |
|--------|-------------|
| `akashi.conflicts.open_total` | Current open conflicts |
| `akashi.conflicts.backfill_remaining` | Decisions with embeddings not yet conflict-scored |

### Alerting recommendations

- **`akashi.conflicts.open_total` > 50**: Conflicts are accumulating faster than resolution. Review triage workflow.
- **`akashi.conflicts.llm_calls{result=error}` sustained**: LLM service degraded. Check Ollama/OpenAI connectivity.
- **`akashi.conflicts.scoring_duration_ms` p99 > 5000**: Scoring is slow. Consider enabling early exit or cross-encoder.
- **`akashi.conflicts.backfill_remaining` not decreasing**: Backfill stalled. Check embedding provider health.

## Configuration reference

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD` | `0.30` | Minimum significance to persist a conflict |
| `AKASHI_CONFLICT_CANDIDATE_LIMIT` | `20` | Max candidates retrieved from Qdrant per decision |
| `AKASHI_CONFLICT_EARLY_EXIT_FLOOR` | `0.25` | Min pre-LLM significance for early exit (0 disables) |
| `AKASHI_CONFLICT_DECAY_LAMBDA` | `0.01` | Temporal decay rate (0 disables; ~70 days to half significance) |
| `AKASHI_CONFLICT_BACKFILL_WORKERS` | `4` | Parallel workers for batch scoring |
| `AKASHI_CONFLICT_LLM_MODEL` | _(empty)_ | LLM model for validation (e.g., `qwen3.5:9b`) |
| `AKASHI_CONFLICT_LLM_THREADS` | `floor(NumCPU/3)` | CPU threads for Ollama |
| `AKASHI_CONFLICT_CLAIM_TOPIC_SIM_FLOOR` | `0.60` | Min cosine similarity for claim pairs |
| `AKASHI_CONFLICT_CLAIM_DIV_FLOOR` | `0.15` | Min outcome divergence for claim pairs |
| `AKASHI_CONFLICT_DECISION_TOPIC_SIM_FLOOR` | `0.70` | Min decision similarity to activate claim scoring |
| `AKASHI_CROSS_ENCODER_URL` | _(empty)_ | Cross-encoder reranking endpoint |
| `AKASHI_CROSS_ENCODER_THRESHOLD` | `0.50` | Min cross-encoder score for LLM validation |
| `AKASHI_CLAIM_EXTRACTION_LLM` | `false` | Use LLM for structured claim extraction |
| `AKASHI_FORCE_CONFLICT_RESCORE` | `false` | Clear and re-score all conflicts at startup |

## Admin tools

### Test the LLM validator

```
POST /v1/admin/conflicts/validate-pair
```

```json
{
  "outcome_a": "Use microservices for the payment system",
  "outcome_b": "Use a monolith for the payment system",
  "type_a": "architecture",
  "type_b": "architecture"
}
```

Returns the relationship, category, severity, and explanation without persisting anything.

### Evaluate validator accuracy

```
POST /v1/admin/conflicts/eval
```

Runs the built-in evaluation dataset and returns precision, recall, F1, and accuracy.
Use this after changing the LLM model or tuning thresholds to verify detection quality.
