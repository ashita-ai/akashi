# ADR-010: ReScore weight calibration via regression

## Status

Deferred

## Context

Issue #264 identified that ReScore's five outcome signal weights (assessment 0.40, citation 0.25, stability 0.15, agreement 0.10, conflict win rate 0.10) were chosen by intuition. The issue asks whether to calibrate them via logistic regression over assessment data.

Calibration via regression requires two things:

1. **A ground-truth ranking** — either explicit relevance judgments (user click-through, "this result was helpful" feedback) or a proxy (e.g., assessment correctness predicting search utility). Neither exists today.

2. **Sufficient signal density** — most decisions currently have zero outcome signals (no assessments, no citations, no conflict history). Regression over a dataset dominated by zeros produces degenerate weights that default to the similarity × recency baseline anyway.

## Decision

Defer automated weight calibration until both preconditions are met:

- At least 30% of decisions in the median org have one or more non-zero outcome signals.
- A ground-truth ranking source exists (click-through data from the dashboard, explicit "helpful" feedback, or agent-labeled relevance).

In the meantime, we address the most impactful calibration gap — distribution mismatch — by:

1. **Percentile-normalizing citation counts** within each org (issue #264). This replaces the arbitrary `log1p(n)/log(6)` saturation formula with empirical percentile mapping, making citation weight proportional to the org's actual citation distribution.

2. **Emitting per-signal contribution metrics** (`akashi.search.rescore_signal_contribution` with `signal` attribute). Operators can observe whether any signal is consistently dead weight and adjust weights manually if needed.

3. **Preserving Qdrant rank as tie-breaker**. Equal-score ties now fall back to Qdrant's semantic ordering instead of arbitrary row order.

## Consequences

- Weights remain hardcoded for now. This is acceptable because the percentile normalization makes them distribution-aware, and the contribution metrics provide observability.
- When the preconditions are met, a weekly offline job fitting logistic regression coefficients against user feedback would replace the hardcoded weights. The infrastructure (per-signal contribution recording, percentile normalization) laid down in this change makes that transition straightforward.
