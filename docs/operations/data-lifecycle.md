# Data Lifecycle Operations

This document defines how Akashi manages data growth, archival, reconciliation, and verification over time.

It is durability-first: no silent deletion, clear paper trail, and explicit operator controls for destructive actions.

## Objectives

- Keep PostgreSQL and Qdrant consistent with PostgreSQL as source of truth.
- Control storage growth for high-volume event data.
- Preserve auditability and forensic traceability.
- Provide repeatable operational procedures with machine-verifiable outcomes.

## Core Principles

- PostgreSQL is the source of truth.
- Archive before purge for historical event data.
- Destructive operations require explicit opt-in.
- Every lifecycle action should be observable and repeatable.

## Data Classes

- **Hot transactional data**: `decisions`, `agent_runs`, `agents`, `access_grants`, `idempotency_keys`
- **High-volume event data**: `agent_events` (Timescale hypertable)
- **Archive data**: `agent_events_archive`, `deletion_audit_log`
- **Search sync buffers**: `search_outbox`, `search_outbox_dead_letters`
- **Audit ledgers**: `mutation_audit_log`, `integrity_proofs`

## Lifecycle Flows

### 1) Event Retention and Archival

Tool: `scripts/archive_agent_events.sh`

Behavior:

1. Select one bounded historical window from `agent_events` older than `RETAIN_DAYS`.
2. Insert matching rows into `agent_events_archive` (idempotent on `(id, occurred_at)`).
3. By default, stop after archive (`DRY_RUN=true`, `ENABLE_PURGE=false`).
4. If explicitly enabled, purge archived rows from hot table only when archive rows exist.

Safety controls:

- Default non-destructive mode.
- Purge requires `DRY_RUN=false ENABLE_PURGE=true`.
- Join-based purge prevents deleting rows not present in archive.

### 2) Postgres/Qdrant Reconciliation

Tool: `scripts/reconcile_qdrant.py`

Behavior:

1. Read expected current decision IDs from PostgreSQL (`valid_to IS NULL`, embedding present).
2. Read actual point IDs from Qdrant via scroll API.
3. Report:
   - missing in Qdrant
   - extra in Qdrant
4. Optional repair mode queues missing entries into `search_outbox` for replay.

Safety controls:

- Repair mode only enqueues upserts; no automatic hard deletes of extra points.
- Optional org scoping (`--org-id`) for controlled remediation.

### 3) Restore Verification

Tool: `scripts/verify_restore.sh`

Behavior:

1. Verify migration state (`schema_migrations` sample).
2. Verify core table presence/counts.
3. Run orphan integrity checks.
4. Optional outbox rebuild from current decisions (`REBUILD_OUTBOX=true`).

Safety controls:

- Fails fast on SQL errors.
- No destructive action unless operator opts into outbox rebuild.

### 4) Exit Criteria Verification

Tool: `scripts/verify_exit_criteria.py`

Behavior:

- Produces structured JSON and exits non-zero on failure.
- Verifies:
  - orphan integrity
  - dead-letter threshold
  - oldest outbox age threshold
  - optional strict retention window
  - optional Qdrant reconciliation (when `QDRANT_URL` is set)

This is the main machine-checkable lifecycle quality gate.

## Recommended Schedules

Use Kubernetes CronJobs or equivalent scheduler.

- `archive-events-dry-run`: daily
- `archive-events` (purge enabled): daily or weekly after dry-run confidence
- `reconcile-qdrant`: every 15-60 minutes
- `reconcile-qdrant-repair`: event-driven or hourly when drift exists
- `verify-exit-criteria`: nightly and pre-release
- `verify-restore`: monthly/quarterly restore drill

## Operational Commands

```sh
# Archive lifecycle
DATABASE_URL=postgres://... make archive-events-dry-run
DATABASE_URL=postgres://... DRY_RUN=false ENABLE_PURGE=true make archive-events

# Consistency
DATABASE_URL=postgres://... QDRANT_URL=https://...:6333 make reconcile-qdrant
DATABASE_URL=postgres://... QDRANT_URL=https://...:6333 make reconcile-qdrant-repair

# Restore and quality gates
DATABASE_URL=postgres://... make verify-restore
DATABASE_URL=postgres://... make verify-exit-criteria
```

## Alerting Recommendations

- Dead letters (`attempts >= 10`) > 0: critical
- Outbox oldest pending age above threshold: warning/critical by duration
- Reconciliation drift > 0 for sustained interval: warning
- Exit criteria failure in scheduled run: critical
- Archive dry-run reveals sustained backlog growth: warning

## Ownership and Review Cadence

- **Primary owner**: platform/data engineering
- **Review cadence**: monthly lifecycle review, quarterly restore drill review
- **Change control**: any retention/purge policy change requires runbook + checklist update

## Related Documents

- `docs/runbook.md`
- `docs/configuration.md`
- `docs/execution-plan-durability-and-consistency.md`
