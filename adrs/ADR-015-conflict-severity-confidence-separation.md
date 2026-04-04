# ADR-015: Separate conflict severity from confidence scoring

## Status

Accepted

## Context

The conflict detection system produces a `significance` score:

    significance = topic_similarity × outcome_divergence × confidence_weight × temporal_decay

This measures **how likely** a pair of decisions constitutes a real conflict — detection confidence. The `severity` column (nullable TEXT: critical/high/medium/low) was intended to capture a different dimension: **how bad** a conflict is if it turns out to be real — impact severity.

In practice, severity was only populated by:

1. **LLM validator** — when an LLM-backed conflict validator was configured, it returned severity as part of the structured classification.
2. **Precedent escalation** — when a new conflict contradicted the winning side of a previously resolved conflict, severity was auto-set to "critical".
3. **LiteScorer** — derived severity directly from significance thresholds (≥ 0.6 → high, ≥ 0.35 → medium, else low).

The LiteScorer approach made the two dimensions correlated rather than independent. A high-significance conflict between two `investigation` decisions (low impact) was rated "high" severity, while a low-significance conflict between two `security` decisions (high impact) was rated "low". This inversion made it impossible for operators to prioritize conflicts effectively.

## Decision

Introduce a `ComputeSeverity` function that derives severity from metadata signals independent of the significance score. The function is called as a fallback when the LLM validator does not provide severity.

### Severity signals

**Decision type tier** (primary signal — uses the higher tier of the pair):

| Tier | Types | Base severity |
|------|-------|---------------|
| 4 | security | high |
| 3 | architecture, deployment | medium |
| 2 | trade_off, model_selection, data_source, error_handling | medium |
| 1 | code_review, feature_scope, investigation, planning, assessment | low |

Unknown/custom types default to Tier 1.

**Promotions** (at most one applies):

- Both decisions have confidence ≥ 0.7 AND effective tier ≥ 3 → promote one level. Rationale: two agents with high conviction disagreeing on architecture or security is harder to resolve and warrants escalation.
- Conflict category is "factual" AND effective tier ≥ 2 → promote one level. Rationale: factual conflicts (objective truth disagreements) are harder to reconcile than assessment or strategic ones.

**Demotion:**

- Either decision has confidence > 0 AND ≤ 0.3 → demote one level. Rationale: when one side is exploratory/uncertain, the conflict is less severe because the low-confidence side is likely to self-correct.
- Zero confidence is treated as "unknown" (no demotion), because the LiteScorer path has no access to decision confidence values.

**Critical is never returned.** That level is reserved for precedent escalation and explicit LLM judgment.

### Fallback chain

1. LLM/external scorer severity (if provided)
2. `ComputeSeverity` (metadata-based)
3. Precedent escalation override (always "critical", applied last)

### No schema changes

The `severity` column already exists. The `significance` field continues to serve as the confidence dimension. No new API fields are needed.

## Consequences

- Low-tier conflicts (investigation, planning) no longer inflate to "high" severity just because embedding math scores them as significant.
- High-tier conflicts (security, architecture) are always at least "medium" severity even when scored with low significance.
- Lite mode gets meaningful severity without requiring an LLM.
- Noop-validator conflicts (previously severity = NULL) now receive computed severity, which means auto-resolution tier classification works correctly for them.
- The auto-resolution tier system (P1 = critical/high + both-high-confidence) now correctly gates on impact rather than detection confidence.
- Existing LLM-produced severities are preserved — `ComputeSeverity` is only the fallback.
- Org settings (`auto_resolve_max_severity`, `never_auto_resolve_severities`) continue to work unchanged.

## References

- [Issue #538](https://github.com/ashita-ai/akashi/issues/538)
- `internal/conflicts/severity.go` — implementation
- `internal/conflicts/severity_test.go` — test coverage
