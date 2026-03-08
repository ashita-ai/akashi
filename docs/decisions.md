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

1. **Embeddings** — Two vectors are computed:
   - **Full embedding:** `decision_type + ": " + outcome + " " + reasoning` → `decisions.embedding`
   - **Outcome embedding:** `outcome` only → `decisions.outcome_embedding`

2. **Quality score** — Completeness heuristic (alternatives, evidence, reasoning length).

3. **Transactional write** — Decision, alternatives, evidence, and search outbox entry in one transaction.

4. **Conflict scoring** — Async goroutine finds similar decisions and inserts into `scored_conflicts` when significance ≥ threshold.

5. **Notifications** — `akashi_decisions` (LISTEN/NOTIFY) for real-time subscribers.

---

## Embeddings

| Column | Input Text | Use |
|--------|------------|-----|
| `embedding` | `type + outcome + reasoning` | Semantic search, conflict **topic similarity** |
| `outcome_embedding` | `outcome` only | Conflict **outcome divergence** (Option B) |

When the embedder is noop or fails, both are NULL. Backfill runs at startup for unembedded decisions and unembedded outcomes.

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
