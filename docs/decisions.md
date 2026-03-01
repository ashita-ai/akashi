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

### Design: Two-Stage Pipeline

Conflict detection uses a two-stage pipeline that separates cheap candidate retrieval from expensive semantic validation:

```
POST /v1/trace (commit)
    │
    └─ (async goroutine)
         │
         ├─ Stage 1: Candidate retrieval (fast, cheap)
         │   └─ Qdrant ANN search: top 50 decisions by full embedding, same org
         │
         └─ Stage 2: Per-candidate scoring (slower, accurate)
              │
              ├─ Significance gate: topic_sim × outcome_div × conf_weight × decay ≥ threshold?
              │   └─ High topic-similarity pairs bypass gate → go directly to Stage 3
              │
              └─ Stage 3: LLM validation (when configured)
                   ├─ OllamaValidator or OpenAIValidator → relationship classification
                   ├─ Conflict? → insert into scored_conflicts
                   └─ Not a conflict? → skip (fail-safe: ambiguous responses are also skipped)
```

`decision_type` is **not** used during detection — cross-type conflicts ("architecture" vs "technology_choice") are found when embeddings are semantically similar.

---

### Stage 1: Candidate Retrieval

For each new decision, the scorer queries Qdrant for the top 50 most similar decisions in the same org, ranked by full embedding cosine similarity. This uses approximate nearest-neighbor search — fast and O(log N) in collection size.

Candidates are then hydrated from PostgreSQL to get `outcome_embedding`, reasoning, agent context, and other fields needed for scoring.

**Revision chain exclusion:** decisions linked via `supersedes_id` are excluded from scoring. An intentional revision is not a conflict.

---

### Stage 2: Significance Scoring

For each candidate, significance is computed as a product of four independent signals:

```
significance = topic_similarity × outcome_divergence × confidence_weight × temporal_decay
```

| Signal | Formula | Meaning |
|--------|---------|---------|
| `topic_similarity` | cosine(`embedding_a`, `embedding_b`) | Are these decisions about the same topic? |
| `outcome_divergence` | 1 − cosine(`outcome_embedding_a`, `outcome_embedding_b`) | Do they reach different conclusions? |
| `confidence_weight` | √(confidence_a × confidence_b) | Attenuate when either party is uncertain. |
| `temporal_decay` | e^(−λ × days_apart) | Older conflicts matter less. λ = `AKASHI_CONFLICT_DECAY_LAMBDA`. |

If `significance ≥ AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD` (default 0.30), the pair proceeds to LLM validation. If it does not, it is skipped — unless it passes the bi-encoder bypass below.

**Two passes — full-outcome and claim-level:** For pairs where `topic_similarity ≥ 0.70`, scoring runs a second pass at the claim level. Each decision's outcome is split into individual sentences and claims, and the most conflicting claim pair is found. If the claim-level significance exceeds the full-outcome significance, the claim pair is used as input to the LLM. This catches conflicts buried in otherwise similar decisions.

**Bi-encoder bypass:** Bi-encoder models (used for embeddings) cannot detect stance opposition — "X is the right approach" and "X is the wrong approach" embed close together because they share vocabulary, not because they agree. For high-topic-similarity pairs (`topic_similarity ≥ 0.70`), the significance gate is bypassed and the pair is sent directly to the LLM classifier, which can correctly identify that two things about the same topic take opposing stances.

---

### Stage 3: LLM Validation

The LLM classifier determines the **relationship** between two outcomes:

| Relationship | Meaning | Conflict? |
|--------------|---------|-----------|
| `contradiction` | Incompatible positions on the same specific question. Cannot both be true. | Yes |
| `supersession` | One explicitly replaces or reverses the other. | Yes |
| `complementary` | Different findings about different aspects. Both can be true. | No |
| `refinement` | One builds on the other without contradicting it. | No |
| `unrelated` | Different topics despite surface similarity. | No |

The classifier also assigns a **category** and **severity**:

| Dimension | Values |
|-----------|--------|
| `category` | `factual`, `assessment`, `strategic`, `temporal` |
| `severity` | `critical`, `high`, `medium`, `low` |

**Validation providers** (mirror the embedding provider chain):

| Provider | Configuration | Notes |
|----------|--------------|-------|
| `OllamaValidator` | `AKASHI_OLLAMA_VALIDATOR_MODEL` (e.g. `qwen2.5:3b`) | On-premises; warms up on startup to avoid cold-start delay. |
| `OpenAIValidator` | `OPENAI_API_KEY` | Uses `gpt-4o-mini` by default. |
| `NoopValidator` | No LLM configured | Falls back to embedding-only detection; higher false positive rate. |

**Fail-safe:** On LLM error, timeout, or unparseable response, the candidate is skipped. A conflict is never inserted without validation when an LLM is configured.

---

### Conflict Kinds

| `conflict_kind` | When |
|-----------------|------|
| `cross_agent` | Decision A and Decision B were recorded by different agents. |
| `self_contradiction` | Both decisions were recorded by the same agent. |

---

## Conflict Lifecycle

Every conflict progresses through a defined set of states. Transitions are recorded in the audit log.

```
open
 │
 ├─ acknowledge → acknowledged
 │                    │
 │                    └─ resolve → resolved  (optionally with winning_decision_id)
 │                    └─ wont_fix → wont_fix
 │
 └─ resolve → resolved
 └─ wont_fix → wont_fix
```

| Status | Meaning |
|--------|---------|
| `open` | Detected but not yet reviewed. |
| `acknowledged` | A human or agent has seen it and is considering it. |
| `resolved` | A decision has been made on which side prevails (or how to reconcile). |
| `wont_fix` | Intentionally left as-is (e.g. different contexts, valid disagreement). |

---

## Resolving Conflicts

### Option A: Simple status transition

`PATCH /v1/conflicts/{id}` transitions status and optionally records a winner:

```json
{
  "status": "resolved",
  "winning_decision_id": "<uuid of decision_a or decision_b>",
  "resolution_note": "decision_a prevails; decision_b was based on stale requirements"
}
```

- `winning_decision_id` must be `decision_a_id` or `decision_b_id` of the conflict.
- Only valid when `status` is `"resolved"`.
- `resolution_note` is free-form prose that appears in the audit log and UI.
- `wont_fix` is appropriate when the conflict is real but not worth acting on.

### Option B: Adjudication trace

`POST /v1/conflicts/{id}/adjudicate` records a new decision that formally resolves the conflict and links it to both sides. Use this when you want the resolution itself to be a traceable, searchable decision in the audit trail:

```json
{
  "outcome": "Adopt the microservices approach from decision_a; decision_b's monolith recommendation was for a different scale requirement.",
  "reasoning": "After review, the microservices design handles the projected load ...",
  "winning_decision_id": "<uuid>"
}
```

This creates a decision of type `"conflict_resolution"` (unless overridden), stores it with embeddings and quality scoring, and marks the conflict as `"resolved"`. The resolution decision is linked via `resolution_decision_id` on the conflict record.

---

## API Endpoints

| Endpoint | Method | Role |
|----------|--------|------|
| `GET /v1/conflicts` | read | List conflicts with filters (status, decision_type, agent_id, etc.) |
| `PATCH /v1/conflicts/{id}` | write | Transition status; optionally set winner and resolution note. |
| `POST /v1/conflicts/{id}/adjudicate` | write | Record a formal resolution decision linked to the conflict. |
| `POST /v1/check` | read | Returns recent conflicts alongside precedents for a given decision type. |

`decision_type` is available as a query filter on `GET /v1/conflicts` and as a body field on `POST /v1/check`. It is not used during detection.

---

## Storage

- **decisions** — Source of truth; `embedding`, `outcome_embedding` nullable.
- **scored_conflicts** — Detected conflict pairs. Columns include `topic_similarity`, `outcome_divergence`, `significance`, `scoring_method`, `relationship`, `category`, `severity`, `status`, `resolved_by`, `resolved_at`, `resolution_note`, `winning_decision_id`, `resolution_decision_id`.
- **decision_claims** — Sentence-level claims extracted from decision outcomes, with per-claim embeddings. Used for claim-level conflict scoring.
- **search_outbox** — Syncs decisions to Qdrant for semantic search (when configured).
