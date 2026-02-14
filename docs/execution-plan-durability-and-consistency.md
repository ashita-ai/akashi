# Execution Plan: Durability, Auditability, and Consistency Hardening

## Purpose

This plan defines how to complete the next hardening wave for:

1. Universal immutable audit logging for critical mutations
2. TimescaleDB retention and archival lifecycle
3. Restore game-day automation and verification
4. Qdrant tuning with explicit SLO targets
5. Postgres/Qdrant consistency reconciliation

The primary objective is zero untracked data loss and a complete paper trail for all critical operations.

## Scope and Non-Goals

### In Scope

- API, storage, and migration changes needed for durability/auditability.
- Operational scripts and runbooks for restore and reconciliation.
- Metrics, alerts, and acceptance checks tied to exit criteria.

### Out of Scope

- Major product feature additions unrelated to durability/auditability.
- Tenant-specific policy customization beyond documented defaults.

## Hard Requirements

- PostgreSQL remains source of truth.
- All destructive operations are explicitly gated and auditable.
- No silent data drops in retry/queue paths.
- Every phase must include:
  - migration safety checks
  - tests
  - runbook updates
  - measurable exit criteria

## Baseline SLO Targets

- `search` p95 latency:
  - <= 150ms for common semantic queries
  - <= 250ms for heavily filtered semantic queries
- Outbox freshness:
  - 95% of outbox entries processed within 30s
- Dead letters:
  - steady-state dead-letter count = 0
- Reconciliation:
  - unresolved Postgres/Qdrant drift = 0 at end of each daily cycle
- Restore drill:
  - RTO <= 60 minutes (staging baseline)
  - post-restore verification mismatch count = 0

## Phase Plan

## Phase 1: Universal Immutable Audit Log

### Specification

- Add a single append-only audit ledger table for critical mutations.
- Record at minimum:
  - `org_id`, actor identity, endpoint/method, request id
  - operation type (`create`, `update`, `delete`, `complete`, `grant`, `revoke`)
  - target entity type/id
  - `before_data` JSONB (nullable), `after_data` JSONB (nullable)
  - reason/source metadata (`delete_reason`, job name, automation source)
  - event timestamp
- Enforce immutability:
  - no `UPDATE`/`DELETE` on audit rows (trigger guard)
- Integrate writes in same transaction as primary mutation where possible.

### Exit Criteria

- All critical mutating endpoints write audit entries.
- Unit/integration tests validate entries and immutability guardrails.
- Runbook includes audit query recipes and incident forensics flow.

## Phase 2: TimescaleDB Retention and Archival Lifecycle

### Specification

- Define explicit lifecycle for `agent_events` and other high-volume logs:
  - hot retention window
  - compressed warm window
  - archive/export path before purge
- Add retention policy controls to config and docs.
- Implement safe archival workflow (snapshot/export verification + checksum).

### Exit Criteria

- Retention policies are active and validated in staging.
- Archive pipeline has verification checks and failure alerts.
- No purge executes unless archive verification passes.

## Phase 3: Restore Game-Day Automation

### Specification

- Add scripted game-day workflow:
  - restore Postgres from backup
  - verify `schema_migrations` state
  - repopulate outbox from current decisions when needed
  - verify counts/hashes/integrity proofs
  - verify API health and query correctness
- Produce machine-readable pass/fail summary.

### Exit Criteria

- One full staged restore drill passes with zero mismatches.
- Runbook includes exact command sequence and troubleshooting.
- CI or scheduled job runs restore verification checks at defined cadence.

## Phase 4: Qdrant Tuning and Capacity Envelope

### Specification

- Tune Qdrant collection/index parameters with conservative defaults:
  - quantization enabled if recall threshold is preserved
  - index/search params selected based on p95 latency and recall tests
  - shard/replica strategy documented for expected scale
- Add benchmark harness:
  - fixed replay dataset
  - latency + recall comparison against baseline

### Exit Criteria

- p95 and recall targets meet baseline SLO thresholds.
- Any quantization setting has explicit measured recall impact.
- Rollback procedure documented and tested.

## Phase 5: Consistency Reconciler

### Specification

- Implement periodic reconciliation job:
  - expected index set from Postgres (current decisions with embeddings)
  - actual index set from Qdrant
  - enqueue repair operations to `search_outbox`
  - emit metrics on drift and repair outcomes
- Add dead-letter and reconciliation alerting.

### Exit Criteria

- Daily reconciliation run reports zero unresolved drift.
- Repair retries are bounded and observable.
- Incident runbook includes reconciliation + replay procedure.

## Test Strategy

- Unit tests for audit write paths and immutability guards.
- Integration tests for:
  - outbox archive + cleanup lifecycle
  - delete gating and audit record generation
  - reconciliation repair loop
- Staging load test for Qdrant latency/recall targets.
- Restore drill test as release gate for durability-sensitive changes.

## Rollout and Safety Controls

- Use feature flags for high-impact behavior changes.
- Migrations forward-only with rollback runbook via restore.
- Deploy sequence:
  1. migrations
  2. service binary
  3. background jobs
  4. enable flags progressively
- Guardrail: destructive operations remain disabled by default.

## Ownership and Cadence

- Suggested cadence: one phase per PR series with clear sign-off.
- Required sign-off roles:
  - data/platform owner
  - reliability owner
  - security/compliance owner (for audit/retention phases)

## Definition of Done (Program-Level)

The program is complete when:

- all five phases meet their exit criteria,
- durability and paper-trail controls are validated in staging drills,
- runbook and alerting cover normal operation and failure recovery,
- and no critical or high severity findings remain open for data loss or missing audit trail risk.
