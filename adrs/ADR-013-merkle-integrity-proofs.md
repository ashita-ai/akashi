# ADR-013: Merkle integrity proof system

**Status:** Accepted
**Date:** 2026-04-03

## Context

Akashi is an audit trail for AI decisions. Tampering with recorded decisions — silently editing an outcome, deleting inconvenient entries, or reordering the log — undermines the system's core value proposition. We need a mechanism that detects unauthorized modifications to the decision history, producing cryptographic evidence of integrity that operators can verify independently.

## Decision

Build a Merkle tree integrity proof system with three properties: **content binding** (each decision's fields are hashed into a content hash), **batch binding** (content hashes within a time window are aggregated into a Merkle root), and **chain binding** (each proof links to the previous proof's root, forming an append-only chain).

### Content hashing

Each decision receives a SHA-256 content hash computed over its identity and semantic fields (`internal/integrity/integrity.go`):

```
v2: + SHA-256(
    len(id) || id ||
    len(decision_type) || decision_type ||
    len(outcome) || outcome ||
    len(confidence) || confidence ||
    len(valid_from) || valid_from ||
    len(reasoning) || reasoning
)
```

The `v2:` prefix distinguishes the current format from the legacy v1 format (pipe-delimited). Field lengths are encoded as 4-byte big-endian integers, preventing boundary ambiguity attacks where `hash("ab", "c") == hash("a", "bc")` under naive concatenation. The `valid_from` timestamp is truncated to microsecond precision to match PostgreSQL's `TIMESTAMPTZ` resolution.

Legacy v1 hashes (pipe-delimited) remain verifiable — `VerifyContentHash` auto-detects the version prefix and dispatches accordingly.

### Merkle tree construction

`BuildMerkleRoot` constructs a deterministic binary Merkle tree from a sorted slice of content hashes:

1. **Sort enforcement**: input must be lexicographically sorted; unsorted input returns `ErrUnsortedLeaves`. Sorting is the caller's responsibility (the storage query uses `ORDER BY content_hash ASC`).
2. **RFC 6962 domain separators**: internal nodes are computed as `SHA-256(0x01 || len(left) || left || right)`. The `0x01` prefix byte distinguishes internal nodes from leaf content hashes, preventing second-preimage attacks where an attacker substitutes a leaf value that happens to equal an internal node hash.
3. **Odd-node handling**: when a tree level has an odd number of nodes, the last node is hashed with itself. This preserves structural binding — the tree shape is deterministic for a given leaf count.
4. **Edge cases**: empty input returns an empty root; a single leaf returns itself as root.

### Epoch boundaries and batch windowing

Proofs are generated periodically (default: every 5 minutes, configurable via `AKASHI_INTEGRITY_PROOF_INTERVAL`). Each proof covers a non-overlapping time window:

- `batch_start`: set to the previous proof's `batch_end` (exclusive lower bound)
- `batch_end`: set to the current time (inclusive upper bound)
- Decisions are selected by `created_at` (insertion time), not `valid_from` (business time), ensuring deterministic batching independent of bi-temporal lifecycle changes.
- Both current and superseded decisions (`valid_to IS NOT NULL`) are included — the proof covers the complete write history within the epoch.

### Chain linkage

Each proof stores a `previous_root` pointer to the prior proof's `root_hash`. Verification checks three invariants:

1. If an older proof exists, `previous_root` must not be nil (`chain_linkage_nil_previous` violation).
2. `previous_root` must equal the immediately older proof's `root_hash` (`chain_linkage_broken` violation).
3. Recomputed Merkle root must match stored `root_hash` (`merkle_root_mismatch` violation).

This chain structure means that tampering with any historical proof invalidates all subsequent proofs' linkage, making silent history rewriting detectable.

### Verification

Two verification modes run on independent schedules:

**Sampling audit** (default: every 15 minutes): picks one org per tick via round-robin offset selection and audits the 10 most recent proofs. Each org is audited roughly every `N_orgs × 15 minutes`. Jitter is applied to avoid thundering-herd effects.

**Full audit** (default: every 24 hours): exhaustive verification across all orgs, checking up to 50 proofs per org with per-org timeouts to prevent starvation.

Both modes record results (passes and failures) in an immutable `integrity_audit_results` table. Detected violations are written to a separate append-only `integrity_violations` table (protected by triggers preventing UPDATE/DELETE) with up to 3 write retries and exponential backoff to ensure evidence survives even under database pressure.

### Interaction with retention and GDPR erasure

Retention purge deletes decision rows; GDPR erasure scrubs PII and recomputes `content_hash`. Both operations break naive Merkle verification by altering or removing the leaf hashes that the proof was built from.

The solution (detailed in ADR-011) is a `proof_leaves` table that snapshots the content hashes at proof-creation time. Verification reads from this snapshot instead of re-querying the mutable `decisions` table. For proofs created before this table existed (pre-migration 084), verification falls back to `GetDecisionHashesForBatch`. The stored hashes are SHA-256 hex digests (64 bytes each), not PII, so they safely outlive the decision content they represent.

## Rationale

**Why Merkle trees over simple aggregate hashing?**

A single aggregate hash (e.g., `SHA-256(sorted_concat(all_hashes))`) detects tampering but provides no localization — you know *something* changed but not *what*. Merkle trees enable O(log n) inclusion proofs for individual decisions, which is valuable for future per-decision verification APIs and for narrowing investigation scope during incident response.

**Why time-based epochs over event-count-based?**

Fixed-size batches (e.g., every 1000 decisions) create variable-latency proof generation — a quiet org might wait hours for a proof while a busy org generates them every second. Time-based epochs ensure predictable proof cadence across all orgs, simplifying audit scheduling and SLA reasoning.

**Why `created_at` over `valid_from` for batch selection?**

`valid_from` is business time controlled by the caller. An agent could set `valid_from` to a past date, causing the decision to land in an already-sealed epoch. Using `created_at` (server insertion time) ensures decisions are assigned to the epoch in which they were actually written, maintaining the monotonic append property.

**Why chain linkage?**

Without chain linkage, an attacker who compromises a single epoch could replace all its decisions and recompute a valid Merkle root. Chain linkage forces the attacker to also update all subsequent proofs' `previous_root` pointers, making the attack detectable at the first proof boundary that wasn't rewritten.

## Consequences

- Integrity violations surface as structured records with violation type, proof ID, and org ID — operators can query them programmatically.
- The proof chain is append-only. Modifying already-applied migrations to alter the integrity tables would itself be detectable via Atlas migration checksums.
- Content hash computation adds negligible overhead to the trace ingestion path (single SHA-256 per decision).
- Proof generation is O(n log n) in the number of decisions per epoch, bounded by the epoch interval.
- The `proof_leaves` table adds ~64 bytes per decision per proof in storage cost, which is negligible relative to the decision rows themselves.
- Legacy v1 content hashes remain verifiable indefinitely; no migration required for existing data.

## References

- ADR-003: Event-sourced bi-temporal model (content hash inputs, `valid_from`/`valid_to` semantics)
- ADR-011: Proof leaves table for retention/erasure-safe verification (the solution to the retention/GDPR interaction)
- RFC 6962: Certificate Transparency (domain separator convention)
