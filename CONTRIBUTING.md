# Contributing to Akashi

This guide is a quick architecture primer for contributors.

## Development Flow

- Validate changes locally: `make all`
- Run tests: `make test`
- Use real Postgres/Timescale in tests (no storage mocks)

## Request Path Overview

Akashi follows a layered flow:

1. `cmd/akashi` wires dependencies and starts background loops.
2. `internal/server` handles HTTP concerns (auth, middleware, request parsing).
3. `internal/service` contains business logic shared by HTTP and MCP.
4. `internal/storage` handles SQL and transactional persistence.

Keep domain decisions in service/storage layers, not handlers.

## Core Data Model Concepts

- **Bi-temporal decisions**:
  - `valid_from` / `valid_to` track business validity.
  - `transaction_time` tracks write-time history.
- **Revision chain**:
  - New revisions link via `supersedes_id`.
  - Historical rows remain queryable.
- **Evidence + alternatives**:
  - Stored separately but written atomically with decisions in trace paths.

## Search Pipeline

- PostgreSQL is source of truth.
- Qdrant is an optional accelerator.
- `search_outbox` provides eventual-consistency sync:
  - decision writes enqueue outbox rows in the same transaction
  - outbox worker upserts/deletes in Qdrant
  - text search fallback keeps queries functional if Qdrant is down

## Multi-Tenancy Rules

- Every query must scope by `org_id`.
- Admin/platform actions should still preserve tenant isolation unless explicitly global.
- Cross-org behavior must be justified and tested.

## Operational Safety Expectations

- Avoid startup with partial schema state.
- Prefer bounded loops and context-aware shutdown.
- Treat audit durability regressions as high priority.
