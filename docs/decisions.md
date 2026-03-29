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

Decisions are bi-temporal: `valid_from`/`valid_to` (business time) and `transaction_time` (when recorded). Revising a decision sets `valid_to` on the old row and inserts a new row with `supersedes_id` pointing to it.

---

## Trace Flow

`POST /v1/trace` records a decision:

1. **Embeddings** — Two vectors computed (full + outcome-only). See [subsystems.md](subsystems.md#what-gets-embedded).

2. **Quality score** — Completeness heuristic (alternatives, evidence, reasoning length).

3. **Transactional write** — Decision, alternatives, evidence, and search outbox entry in one transaction.

4. **Conflict scoring** — Async goroutine finds similar decisions and inserts into `scored_conflicts` when significance ≥ threshold.

5. **Notifications** — `akashi_decisions` (LISTEN/NOTIFY) for real-time subscribers.

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

- **decisions** — Source of truth; `embedding`, `outcome_embedding` nullable.
- **scored_conflicts** — Detected conflict pairs. See [conflicts.md](conflicts.md) for schema details.
- **decision_claims** — Sentence-level claims extracted from decision outcomes, with per-claim embeddings.
- **search_outbox** — Syncs decisions to Qdrant for semantic search (when configured).
