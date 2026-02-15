# ADR-015: Conflict lifecycle — categories, severity, and resolution

**Status:** Accepted
**Date:** 2026-02-15

## Context

Conflict detection now works: embedding pre-filter finds candidates, LLM validation confirms genuine contradictions (PR #93 reduced 89 false positives to 6 real conflicts). But detected conflicts are write-only — there's no way to categorize, prioritize, or resolve them. Every call to `akashi_check` surfaces the same conflicts forever, even after they've been addressed. This makes the conflict system noisy rather than useful.

Two open issues describe the gap: #79 (resolution workflow) and #86 (severity classification).

## Decision

### Conflict categories

Conflicts are categorized by what kind of disagreement they represent:

| Category | Meaning | Example |
|----------|---------|---------|
| `factual` | Incompatible claims about observable state | "all tests pass" vs "3 critical SDK bugs" |
| `assessment` | Opposing evaluations of the same system | "strong security" vs "pagination bug undermines integrity" |
| `strategic` | Incompatible positions on direction | "no license stronger than BUSL" vs "escrow replaces BUSL" |
| `temporal` | Stale decision contradicted by newer information | old architecture choice vs revised approach |

The LLM validator already sees both outcomes — extending the prompt to emit a category is a single-field addition to the existing structured response.

### Severity

Each conflict gets a severity level: `critical`, `high`, `medium`, `low`. Assigned by the LLM validator at detection time based on the decision types and the nature of the contradiction. Security-related conflicts default to critical/high regardless of LLM output.

### Resolution states

```
open → acknowledged → resolved
open → wont_fix
```

- **open** — default on insert. Surfaces in `akashi_check`.
- **acknowledged** — someone saw it. Still surfaces in `akashi_check` but marked.
- **resolved** — addressed. Hidden from `akashi_check` by default.
- **wont_fix** — intentional tension or accepted trade-off. Hidden from `akashi_check`.

### Schema additions

```sql
ALTER TABLE scored_conflicts
    ADD COLUMN category TEXT CHECK (category IN ('factual','assessment','strategic','temporal')),
    ADD COLUMN severity TEXT CHECK (severity IN ('critical','high','medium','low')),
    ADD COLUMN status TEXT NOT NULL DEFAULT 'open'
        CHECK (status IN ('open','acknowledged','resolved','wont_fix')),
    ADD COLUMN resolved_by TEXT,
    ADD COLUMN resolved_at TIMESTAMPTZ,
    ADD COLUMN resolution_note TEXT;

CREATE INDEX idx_scored_conflicts_status ON scored_conflicts(org_id, status)
    WHERE status = 'open';
```

### API changes

- `GET /v1/conflicts` — add `status`, `severity`, `category` query params. Default `status=open`.
- `PATCH /v1/conflicts/{id}` — new endpoint. Accepts `status`, `resolution_note`. Requires `agent+` role.
- `akashi_check` — filter to `status IN ('open','acknowledged')` by default. Include `category` and `severity` in response.
- SSE `akashi_conflicts` channel — include severity for client-side filtering.

### OSS / enterprise split

Per ADR-013: functionality that makes the core product work is OSS.

**OSS:** Detection, validation, categories, severity, basic resolution (acknowledge, resolve, dismiss with note). Without resolution, `akashi_check` degrades into noise. Without categories, agents can't prioritize. These close the feedback loop.

**Enterprise:** Conflict policies (auto-escalation rules, mandatory resolution gates before deployment), resolution approval workflows (multi-party sign-off), conflict analytics and trend dashboards, external integrations (Slack/PagerDuty/JIRA notifications on new conflicts).

## Consequences

- The LLM validator prompt grows by two fields (category, severity). Parsing adds two lines.
- `akashi_check` becomes useful over time instead of accumulating noise — resolved conflicts stop appearing.
- The `PATCH /v1/conflicts/{id}` endpoint is Akashi's first write endpoint that isn't decision tracing. Auth requires `agent+` role to prevent readers from dismissing conflicts they shouldn't.
- False positive feedback (marking conflicts as `wont_fix`) creates training signal for future threshold tuning (#80).
- Old conflicts from before this migration get `status='open'`, `category=NULL`, `severity=NULL`. Backfill via LLM re-scoring is optional.

## References

- PR #93: LLM-validated conflict detection
- Issue #79: Resolution workflow
- Issue #86: Severity classification
- ADR-013: Enterprise architecture and feature split
- Schema: `migrations/027_semantic_conflicts.sql`, `migrations/034_conflict_llm_validation.sql`
- Implementation: `internal/conflicts/scorer.go`, `internal/conflicts/validator.go`
- Storage: `internal/storage/conflicts.go`
