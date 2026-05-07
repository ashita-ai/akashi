# ADR-016: Synthesis decisions and multi-target supersedes

## Status

Accepted

## Context

Two existing primitives cover most of the conflict-resolution space:

1. `akashi_resolve` (`PATCH /v1/conflicts/{id}`) picks a winner from {A, B} or marks the conflict false-positive. Mutates conflict state; does not create a decision record. The winner is constrained to one of the two decisions in the conflict (`internal/server/handlers_conflicts.go:159`, `storage.ErrWinningDecisionNotInConflict`).
2. `akashi_trace(supersedes_id=X)` creates a new decision that explicitly replaces X. The trace pipeline auto-invalidates X (`valid_to`) and cascade-resolves X's open conflicts via `AutoResolveSupersededConflictsTx` (`internal/storage/trace.go:222`).

A third primitive exists at the HTTP boundary but is not exposed via MCP: `POST /v1/conflicts/{id}/adjudicate` creates an adjudication decision atomically with conflict resolution via `CreateTraceAndAdjudicateConflictTx`. It does **not** set `supersedes_id` on either side of the conflict — the adjudication trace lives parallel to A and B rather than replacing them.

There is no clean primitive for the **synthesis** case: when the right answer to a conflict between decisions A and B is a third decision C that integrates parts of both, with A and B both formally retired. Operators today either pick a false binary or fall back to unstructured `resolution_note` prose — both of which erode the audit trail's utility. The pattern surfaces in akashi's own decision trail: open conflict `cd205a26` is a self-reversal where `claude-code` replaced its Go+Kong choice for the local Supabase orchestrator (ARD-847) with TypeScript+Caddy. The system can model "we picked TS+Caddy" or "we superseded Go+Kong" in isolation but not the structural fact that the second decision retires the first as a strategic course-correction.

Issue [#703](https://github.com/ashita-ai/akashi/issues/703) proposed three changes: (1) relax `akashi_resolve.winning_decision_id` to admit any decision, (2) add an `akashi_reconcile` primitive, (3) make `supersedes_id` multi-valued. After working through the codebase the design diverges from the issue in two places.

## Decision

### Preserve the `winning_decision_id` constraint

Migration 046 introduced `winning_decision_id` with a deliberate split from `resolution_decision_id`, noted in the migration comment: *"Distinct from `resolution_decision_id` (the narrative trace created to document the resolution)."* The first column answers "which side prevailed head-to-head?"; the second points at the trace narrating the resolution.

`winning_decision_id` stays strictly ∈ {A, B, NULL}. The synthesis pointer goes in `resolution_decision_id` (which already has the right semantics) plus rows in a new `decision_supersedes` table. This preserves the queryable head-to-head signal that a relaxed field would lose, and avoids overloading one column with three meanings.

This supersedes the issue's step 1.

### Multi-target supersedes via a join table, not an array column

Add a new table:

```sql
CREATE TABLE decision_supersedes (
    superseding_id UUID NOT NULL REFERENCES decisions(id) ON DELETE RESTRICT,
    superseded_id  UUID NOT NULL REFERENCES decisions(id) ON DELETE RESTRICT,
    org_id         UUID NOT NULL REFERENCES organizations(id),
    relationship   TEXT NOT NULL DEFAULT 'supersedes',  -- supersedes | reconciles
    is_primary     BOOLEAN NOT NULL DEFAULT FALSE,
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (superseding_id, superseded_id)
);
CREATE INDEX idx_decision_supersedes_superseded ON decision_supersedes(superseded_id);
CREATE INDEX idx_decision_supersedes_org ON decision_supersedes(org_id);
```

`decisions.supersedes_id` is retained as a denormalized "primary" pointer. An invariant — enforced in storage or by trigger — keeps the row marked `is_primary=TRUE` for a given `superseding_id` in sync with `decisions.supersedes_id`. Backfill is one `INSERT … SELECT` from existing non-null `supersedes_id` values (each becomes one row with `is_primary=TRUE`, `relationship='supersedes'`).

Rejected alternative: turn `supersedes_id` into `supersedes_ids UUID[]`. Reasons:

- `decisions` is hot, append-only, and protected by hard immutability triggers (migration 036). ALTER on every row plus carving out the trigger is materially riskier than an additive table.
- The existing reverse-lookup index (`idx_decisions_supersedes ON decisions(supersedes_id) WHERE supersedes_id IS NOT NULL`) maps directly to a btree on `decision_supersedes(superseded_id)`. Arrays force GIN, which is strictly slower for the "given X, who supersedes it?" direction.
- Any future per-relationship metadata (relationship type, reason, recorded-by-agent, recorded-at) becomes columns on the join table. With arrays it becomes parallel arrays kept in sync, or JSONB-stuffing — i.e. reinventing the join table badly.
- Concurrent extension of supersedes (any future "retroactive deprecation" case) is independent inserts on the join table vs lost-update-prone read-modify-write on an array.
- `EXPLAIN` output for standard JOINs is more readable than for `ANY()` on arrays — operational debuggability matters.

The cost is the two-source-of-truth concern between `decisions.supersedes_id` and the join table. It is bounded by the strict invariant: fixed maintenance, not growing. `integrity.ComputeContentHash` does **not** include `supersedes_id`, so neither approach forces hash recomputation.

### Reconciliation as a distinct decision type

Introduce `decision_type='reconciliation'`. Migration 091 established `decision_type` as a queryable health-signal axis; a flag would create cross-products with every existing type (architecture+reconcile_flag, security+reconcile_flag, …) that complicate aggregation. Reconciliations also have categorically different downstream behavior (≥2 supersedes targets, conflict_id linkage), enough divergence to deserve a name. `decision_type='conflict_resolution'` (already used by adjudicate's default) stays for the case where one of {A, B} wins and the trace narrates *why*; `reconciliation` is the case where neither wins outright and a synthesis retires both.

Reconciliations are assessable via `akashi_assess` like any other decision. They can be wrong (politically convenient mush is a real failure mode); aggregate signal is reconciliation rate per area, correctness stays per-decision.

### Extend adjudicate as substrate; expose reconcile via MCP

`CreateTraceAndAdjudicateConflictTx` is the right substrate. Extend the request to accept `supersedes` (a list of decision IDs); when set, the same transaction writes one `decision_supersedes` row per target with `relationship='reconciles'`, sets `valid_to=now()` on each target, and skips the embedding cascade for the explicit pair (cascade still fires on sibling conflicts in the same group — the explicit pair is already resolved directly).

`HandleAdjudicateConflict` remains the HTTP/API substrate for formal adjudications. It is useful for dashboards and administrative flows that need a linked `resolution_decision_id`, but exposing the generic operation directly to agents would make the MCP surface ask agents to choose between two overlapping "create a decision while resolving a conflict" tools. That is not the gap #703 set out to close.

`akashi_reconcile` is the MCP primitive: it calls the extended adjudicate substrate with `supersedes=[A, B]` and `decision_type='reconciliation'`. `akashi_resolve` stays narrow (governance bookkeeping) — the issue's "What NOT to do" section argues correctly that resolutions and reconciliations are distinct primitives at distinct grains.

### Sequencing

#566 (explicit supersedes suppresses conflict detection) ships **before** the reconcile work. This is a hard precondition, not a parallel ticket. A synthesis decision C with `supersedes_id=A` (and a `decision_supersedes` row for B) embeds close to both A and B by construction; without scorer-side suppression, the next conflict-scoring pass detects new C↔A and C↔B conflicts, semantically undoing the reconcile.

Order:

1. **#566** — conflict scorer reads `decision_supersedes` (and existing `supersedes_id`) and skips pairs in an explicit supersedes relationship.
2. `decision_supersedes` migration + backfill + invariant trigger.
3. Extend `CreateTraceAndAdjudicateConflictTx` and `HandleAdjudicateConflict` to write multi-target supersedes.
4. Add `akashi_reconcile` as the MCP tool for synthesis.

## Consequences

- The audit graph gains a first-class shape for synthesis decisions. Queries like "every conflict resolved by reconciliation" become expressible as `decision_type='reconciliation'` filtered by `resolution_decision_id` join.
- `winning_decision_id` retains a single, queryable meaning (head-to-head winner). The "who wins head-to-head?" signal stays clean.
- The single-target supersedes case is unchanged — the existing `supersedes_id` paths keep working with no SDK or storage churn. Only `decision_supersedes` is new.
- Schema-level evolution of supersedes relationships (per-relationship metadata, future relationship types like `deprecates`) is easy — add columns to `decision_supersedes`.
- The denormalization invariant between `decisions.supersedes_id` and `decision_supersedes(is_primary=TRUE)` adds a small write-path cost: every insert/update of `supersedes_id` requires a paired write to the join table. Enforced in `internal/storage/trace.go` and `internal/storage/decisions.go` (and the SQLite mirrors), with triggers mirroring the primary pointer as a safety net.
- The conflict scorer becomes aware of explicit supersedes relationships (#566). This is a behavior change for any agent that today relies on the scorer re-detecting a pair after one side is superseded — none in-tree, but documented in the #566 changelog.
- A high reconciliation rate in a project area becomes a project-health signal — synthesis-prone domains may warrant out-of-band discussion (ADRs, design reviews). This is opt-in observation; not built into auto-actions.
- The cascade-resolve embedding heuristic (cosine ≥ 0.80 on outcome embeddings) is **not** removed. It still applies for sibling conflicts in the same group; only the explicit pair is skipped.

## References

- [Issue #703](https://github.com/ashita-ai/akashi/issues/703) — original proposal (this ADR supersedes the issue's step 1)
- [Issue #566](https://github.com/ashita-ai/akashi/issues/566) — explicit supersedes suppression in conflict detection (precondition)
- akashi decision `12849064-1c68-4581-98ab-7fe28112a1d8` — design call traced in the audit log
- `migrations/046_winning_decision_id.sql` — establishes the `winning_decision_id` vs `resolution_decision_id` distinction this ADR preserves
- `migrations/038_conflict_precision.sql` — adds `resolution_decision_id` to `scored_conflicts`
- `migrations/036_decision_immutability.sql` — immutability triggers that motivate the join-table choice
- `migrations/091_decision_type_aliases.sql` — establishes `decision_type` as a queryable axis
- `internal/storage/trace.go` — `CreateTraceAndAdjudicateConflictTx`, the substrate that gets extended
- `internal/server/handlers_conflicts.go` — `HandleAdjudicateConflict`, the HTTP/API surface extended with multi-target supersedes
- `internal/conflicts/scorer.go` — where #566 lands
