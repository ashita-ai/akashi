# ADR-011: Proof leaves table for retention/erasure-safe integrity verification

## Status

Accepted

## Context

The integrity proof subsystem builds Merkle trees from decision content hashes and stores the root hash in `integrity_proofs`. Periodic audits re-fetch content hashes from the `decisions` table, recompute the Merkle root, and compare it to the stored root.

Two legitimate operations break this verification:

1. **Retention purge**: `BatchDeleteDecisions` deletes decision rows. The leaf hashes disappear, so the recomputed root has fewer (or zero) leaves — producing a false `merkle_root_mismatch` violation.

2. **GDPR erasure**: `EraseDecision` scrubs PII fields in-place and recomputes `content_hash` over the erased content. The decision row survives, but its hash changes — again producing a false mismatch.

Both cases cause `verifyProofsForOrg` to report integrity violations for legitimately modified data. This undermines the entire integrity subsystem: operators cannot distinguish genuine tampering from expected data lifecycle operations.

## Decision

Snapshot the Merkle leaf hashes at proof-creation time in a new `proof_leaves` table. Verification reads from this table instead of re-querying the `decisions` table.

### Why this approach over alternatives

Three options were evaluated:

1. **Epoch-based proofs** (seal and archive trees before retention runs): Requires coordinating proof generation with retention scheduling, adds operational complexity, and still fails for ad-hoc GDPR erasures that can happen at any time.

2. **Retention-aware verification** (check `deletion_audit_log` for purged leaves): Requires reconstructing which hashes *would have been* in the proof from archived JSONB records, is fragile (depends on archive fidelity), and doesn't handle GDPR erasure where the row still exists with a different hash.

3. **Preserve leaf hashes** (this ADR): Store the bare content hashes (no PII) in a lightweight join table at proof-creation time. Verification becomes self-contained — no dependency on the mutable `decisions` table. Works for both retention purge and GDPR erasure. The stored hashes are SHA-256 hex digests (64 bytes each), not PII, so they can safely outlive the decision content.

Option 3 was chosen for its simplicity, correctness, and independence from the retention/erasure execution path.

### Backward compatibility

Proofs created before migration 084 have no rows in `proof_leaves`. The verification code falls back to `GetDecisionHashesForBatch` for these proofs, preserving existing behavior. Over time, as old proofs age out of the audit window, all verified proofs will use snapshotted leaves.

## Consequences

- Integrity verification no longer produces false violations after retention purge or GDPR erasure.
- Storage cost: ~64 bytes per leaf hash per proof. For an org generating 100 decisions/hour with hourly proofs, this is ~6.4 KB/proof — negligible.
- `proof_leaves` rows are deleted via `ON DELETE CASCADE` when a proof is deleted, so no orphan cleanup is needed.
- The `proof_leaves` table contains only cryptographic hashes, not PII. It does not need GDPR erasure handling.
- No changes to the Merkle tree construction or verification algorithms — the fix is purely in the data access layer.
