# How Decisions Work

This document describes the decision model, trace flow, embeddings, conflict detection, and the role of `decision_type`.

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

### Detection Logic (No decision_type Matching)

Conflicts are found by **semantic similarity**, not by `decision_type`:

1. On each new decision, the scorer loads its embeddings.
2. **Candidates:** pgvector KNN on `embedding` — top 50 most similar decisions in the same org.
3. **Scoring:** For each candidate with `outcome_embedding`:
   - `topic_similarity` = cosine similarity of full embeddings
   - `outcome_divergence` = 1 − cosine similarity of outcome embeddings
   - `significance` = topic_similarity × outcome_divergence
4. If significance ≥ `AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD` (default 0.30), insert into `scored_conflicts`.
5. `conflict_kind`: `cross_agent` (different agents) or `self_contradiction` (same agent).

Time windows and `decision_type` equality are **not** used. Cross-type conflicts (e.g. "architecture" vs "technology_choice") can appear when the embeddings are similar.

### decision_type as a Filter Only

`decision_type` is **not** part of detection. It is used only when **querying** conflicts:

- `GET /v1/conflicts?decision_type=architecture` — return conflicts where either decision has that type.
- `POST /v1/check` — passes `decision_type` to filter conflicts shown alongside precedents.

This lets callers scope results (e.g. "show conflicts involving architecture decisions") without restricting how conflicts are discovered.

---

## API Endpoints

| Endpoint | decision_type | Role |
|----------|---------------|------|
| `POST /v1/trace` | Required in body | Stored on decision; used for embeddings. |
| `POST /v1/check` | Required in body | Filters precedents and conflicts by type. |
| `GET /v1/conflicts` | Optional query param | Filters returned conflicts. |
| `GET /v1/decisions` | Optional in filters | Structured query filter. |
| `POST /v1/search` | Optional in filters | Semantic search filter. |

---

## Storage

- **decisions** — Source of truth; `embedding`, `outcome_embedding` nullable.
- **scored_conflicts** — Semantic conflict pairs with `topic_similarity`, `outcome_divergence`, `significance`.
- **search_outbox** — Syncs decisions to Qdrant for semantic search (when configured).
