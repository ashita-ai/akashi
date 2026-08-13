# How Decisions Work

This document describes the decision model, trace flow, and embeddings.

---

## Decision Model

A decision records what an agent decided and why:

| Field | Purpose |
|-------|---------|
| `decision_type` | Free-form label (e.g. `"architecture"`, `"code_review"`). Used for **filtering and UX**, not as a structural constraint. |
| `outcome` | What was decided (e.g. `"microservices"`, `"approve"`). |
| `reasoning` | Optional explanation. |
| `confidence` | 0–1 score. |
| `alternatives` | Other options considered (labels, scores, rejection reasons). |
| `evidence` | References used (URIs, content, relevance). |
| `bindings` | Named parameters this decision *sets*, and the values it sets them to. The one field where conflict detection is an exact join rather than an inference — two decisions binding the same parameter to different values conflict by lookup, no model involved. |

Decisions are bi-temporal: `valid_from`/`valid_to` (business time) and `transaction_time` (when recorded). Revising a decision sets `valid_to` on the old row and inserts a new row with `supersedes_id` pointing to it.

---

## Trace Flow

`POST /v1/trace` records a decision, in this order:

1. **Type canonicalization** — `decision_type` is lowercased, then mapped through the org's
   alias table or Levenshtein-matched to a standard type. The submitted spelling is kept in
   `metadata.original_decision_type`. See [quality-scoring.md](quality-scoring.md#type-canonicalization-this-rewrites-your-input).

2. **Completeness score** — Structural heuristic over reasoning, alternatives, evidence,
   confidence, outcome, and precedent ref. Computed on what the agent submitted.

3. **Ingest gate** (optional, off by default) — Refuses or warns when the score is below the
   configured floor for that type. Runs before any embedding work so a reject costs nothing.

4. **Embeddings** — Two vectors computed concurrently (full + outcome-only), plus one per
   evidence item. See [subsystems.md](subsystems.md#what-gets-embedded).

5. **Confidence deflation** — Self-reported confidence is reduced when unsupported by
   evidence, alternatives, or reasoning; the original is kept in metadata. See
   [quality-scoring.md](quality-scoring.md#confidence-deflation).

6. **Transactional write** — Decision, alternatives, evidence, bindings, and the search
   outbox entry in one transaction.

7. **Notifications** — `akashi_decisions` (LISTEN/NOTIFY) for real-time subscribers.

8. **Claims and conflict scoring** — Async, post-commit. Claims are extracted and embedded,
   then the scorer finds similar decisions and inserts into `scored_conflicts` when
   significance ≥ threshold and the judge confirms.

---

## Embeddings

Two embeddings are computed per decision (`embedding` and `outcome_embedding`). Both are nullable — when the embedder is noop or fails, they are NULL and backfilled at next startup. See [subsystems.md § Embedding Provider](subsystems.md#embedding-provider) for input construction, truncation, and provider details.

---

## Conflict Detection

For the full conflict detection pipeline, scoring, LLM validation, resolution methods, analytics, and observability, see [conflicts.md](conflicts.md).

Key points relevant to the decision model:

- `decision_type` is **not** used during detection — cross-type conflicts are found when embeddings are semantically similar. It is available only as a query filter.
- Decisions linked via `supersedes_id` (intentional revisions) are excluded from conflict scoring.
- Two embeddings per decision enable independent measurement of topic similarity (full embedding) and outcome divergence (outcome-only embedding).

---

## Storage

- **decisions** — Current-state table read by every query path; `embedding`,
  `outcome_embedding` nullable. Derived from `agent_events`, which is the append-only source
  of truth (see [ADR-003](../adrs/ADR-003-event-sourced-bitemporal-model.md)).
- **alternatives**, **evidence** — Child rows written in the same transaction.
- **decision_bindings** — Named parameter/value pairs the decision sets, with canonicalized
  join keys.
- **decision_claims** — Sentence-level claims extracted from decision outcomes, with per-claim embeddings.
- **scored_conflicts** — Detected conflict pairs. See [conflicts.md](conflicts.md) for schema details.
- **decision_supersedes** — Confirmed and detector-suggested supersession links.
- **decision_type_aliases** — Per-org mapping from submitted type spellings to canonical types.
- **search_outbox** — Syncs decisions to Qdrant for semantic search (when configured).
