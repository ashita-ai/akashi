# GDPR Tombstone Erasure

Akashi supports GDPR Article 17 "right to be forgotten" via in-place tombstone erasure.
This scrubs PII from a decision without deleting the row, preserving the audit chain
while removing personal data.

## How it works

```
POST /v1/decisions/{id}/erase
```

**Required role:** `org_owner`

**Request body (optional):**

```json
{
  "reason": "GDPR erasure request from user@example.com"
}
```

### What gets scrubbed

| Field | Erased value |
|-------|-------------|
| `outcome` | `"[erased]"` |
| `reasoning` | `"[erased]"` |
| `embedding` | `NULL` |
| `outcome_embedding` | `NULL` |
| `content_hash` | Recomputed over scrubbed fields |
| Alternative `label` | `"[erased]"` |
| Alternative `rejection_reason` | `"[erased]"` |
| Evidence `content` | `"[erased]"` |
| Evidence `source_uri` | `NULL` |
| Evidence `embedding` | `NULL` |

### What is preserved

- Decision ID, timestamps, `valid_from`/`valid_to`, `transaction_time`
- Decision type, confidence score, agent ID, org ID
- The fact that a decision existed (structural metadata)
- The audit trail entry recording the erasure itself

### Audit trail

Each erasure creates:

1. A row in `decision_erasures` with the original content hash, erased content hash,
   the erasing agent, and the reason.
2. A `DecisionErased` event in the `agent_events` hypertable.
3. A mutation audit entry capturing before/after state.
4. A search outbox deletion (removes the decision from Qdrant).

## Legal holds

If a decision is covered by an active legal hold, erasure is blocked with `409 Conflict`.
Legal holds take precedence over erasure requests. Resolve the hold first, then retry.

```
POST /v1/retention/hold     # Create a legal hold (admin)
DELETE /v1/retention/hold/{id}  # Release a legal hold (admin)
```

## Error responses

| Status | Condition |
|--------|-----------|
| `404 Not Found` | Decision does not exist |
| `409 Conflict` | Decision already erased, or active legal hold exists |
| `403 Forbidden` | Caller does not have `org_owner` role |

Erasure is idempotent in the sense that a second call returns `409` with a clear message
rather than silently succeeding — the original erasure audit record is already in place.

## Verification

After erasure, `GET /v1/verify/{id}` returns:

```json
{
  "status": "erased",
  "original_hash": "sha256:abc...",
  "erased_hash": "sha256:def...",
  "verified": true
}
```

This confirms the erasure was applied correctly and the content hash chain is intact.

## Operational notes

- Erasure is a **single transaction** — either everything is scrubbed or nothing is.
  The transaction sets a session-local variable (`akashi.erasure_in_progress`) to bypass
  the immutability trigger that normally prevents decision mutation.
- Erased decisions are removed from Qdrant via the search outbox, so they no longer
  appear in semantic search results.
- Erased decisions still appear in `GET /v1/decisions/{id}` with `[erased]` placeholder
  text — the row is not deleted.
- The `reason` field is optional but recommended for compliance audit trails.
