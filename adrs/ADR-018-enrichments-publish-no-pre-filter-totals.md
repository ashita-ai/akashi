# ADR-018: Decision enrichments publish no pre-access-filter totals

**Status:** Accepted
**Date:** 2026-08-11

## Context

`GET /v1/runs/{run_id}?include=enrichments` returns, for every decision in the run, its revision chain, lineage, conflicts, and integrity status. Two of those sub-objects carried a `total` field:

- `revisions.total` — the length of the revision chain **before** `filterDecisionsByAccess` ran.
- `conflicts.total` — the per-decision conflict count **before** `filterConflictsByAccess` ran.

Three facts make that combination an authorization-boundary information disclosure rather than a documentation wart:

1. The route is `readRole` (`internal/server/server.go`), so any reader-or-above token reaches it.
2. `authz.LoadGrantedSet` returns `nil` ("unrestricted") for admin and above, so `total != count` is reachable **only** for sub-admin callers — precisely the callers whose grants are hiding rows.
3. `HandleGetRun` authorizes the request against `run.AgentID` alone, while revision chains and conflicts span other agents. A conflict is visible only when the caller has access to *both* sides.

Together those give a restricted reader an exact oracle: `revisions.total > revisions.count` means "N revisions exist on this decision that you may not read," and `conflicts.total` counts conflicts between agents the caller holds no grant for. No further request is needed to extract the number.

A previous pass over this code (commit `a9416f7`) rewrote the field comments to describe `total` as post-filter without changing the computation, which left the disclosure in place while making the code read as if it had been fixed.

## Decision

**Enrichment responses publish no quantity derived from a pre-access-filter set.** The `total` field is removed from both `enrichmentRevisions` and `enrichmentConflicts`. What remains:

- `revisions.count` — accessible revisions returned. The revision list is uncapped, so a post-filter total would be identically equal to `count`; there is nothing left to publish.
- `conflicts.count` — accessible conflicts returned, capped at 50 per decision.
- `conflicts.has_more` — whether the **accessible** set filled that cap, computed by `capEnrichmentConflicts` from the filtered slice alone.

This matches the rule `computePagination` (`internal/server/handlers.go`) already applies to every list endpoint: when access filtering hid rows, the total is not knowable to the caller and is not published. `TestComputePagination` pins that behaviour.

`cited_by_has_more` carried the identical defect and is fixed in the same change. Storage computes `CitedByMore` against the pre-filter citation set (`internal/storage/decisions.go`), and `authz.FilterLineage` rebuilt `CitedBy` without touching the flag — so a restricted caller could read `cited_by_has_more = true` beside an unfilled list and learn that citations exist which they may not read. `FilterLineage` now clears the flag whenever filtering removed an entry. That is deliberately conservative and can under-report; a truthful post-filter value cannot be published without re-opening the disclosure, and the same reasoning that removes `total` forbids publishing it.

One residual is knowingly accepted: storage trims its `citedByLimit+1` probe row before returning, so when all fetched entries are accessible but the discarded probe row was not, one bit still escapes. Closing it requires storage to return the uncapped probe and cap post-filter, mirroring `capEnrichmentConflicts`. Recorded rather than silently left.

### Alternatives rejected

- **Recompute `total` post-filter.** Provably carries zero information. Revisions are uncapped, so post-filter total ≡ `count`. Conflicts are over-fetched at `perDecisionLimit+1` = 51 (`internal/storage/conflicts.go`), so a post-filter total differs from `count` only at exactly 51 — the over-fetch sentinel, not a real total. A field that can only be redundant or meaningless exists only to be re-broken later.
- **Gate the pre-filter total to admin+.** Keeps the disclosure primitive in the struct and makes its safety depend on a role check staying correct forever, inside a handler that already scopes authorization to `run.AgentID` only.
- **Rename the field.** Renaming a leaked quantity does not un-leak it. This is the class of change that left the defect open once already.

### Accepted cost

`conflicts.has_more` can under-report: when a decision has ≥51 raw conflicts *and* access filtering hides some of them, the accessible slice may not fill the cap even though more accessible conflicts exist beyond the fetched window. Correcting that would require a post-filter counting query whose result could not be published without reopening the disclosure this decision closes. The bound is documented at `capEnrichmentConflicts`.

## Consequences

- `api/openapi.yaml` drops `total` from `EnrichmentRevisions` and `EnrichmentConflicts`, and from their `required` lists. This is a breaking response-shape change for any client reading those fields.
- The UI (`ui/src/types/api.ts`, `ui/src/pages/DecisionDetail.tsx`) no longer renders "N of M"; conflicts render as `N+` when `has_more`, matching the existing `cited_by_has_more` idiom. `tsc -b` in the `build-ui` CI job fails on any surviving reference.
- The SDKs are unaffected: none of them model the enrichment types.
- `TestEnrichmentResponses_PublishNoTotals` (`internal/server/handlers_runs_test.go`, untagged) marshals both structs and fails if a `total` key reappears, so a future "consistency" pass cannot silently reverse this.
