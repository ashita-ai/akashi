---
description: "Decision tracing workflow for AI agents using Akashi"
alwaysApply: true
---

# Akashi Decision Tracing

You have access to Akashi, a decision audit trail for AI agents.

## Workflow

Follow this for every non-trivial decision:

1. **BEFORE deciding**: call `akashi_check` to look for prior decisions and active conflicts.
   Pass a natural language query describing what you're about to decide. Use the results
   to avoid contradicting prior work and to cite relevant precedents.

2. **AFTER deciding**: call `akashi_trace` with what you decided (outcome), why (reasoning),
   your confidence (0.0-1.0), and project name. This creates a provable record so other
   agents can learn from it.

## When to Check

- Choosing architecture or technology
- Starting a review or audit
- Making trade-offs between approaches
- Filing issues or PRs
- Changing existing behavior

## When to Trace

- Completing a review
- Choosing an approach
- Creating issues or PRs
- Finishing a task that involved choices
- Making security or access judgments

## When to Skip

- Pure execution (formatting, typo fixes)
- Reading or exploring code
- Asking the user a question (no decision yet)

## Tools

- `akashi_check` — look up precedents and conflicts before deciding
- `akashi_trace` — record a decision after making it
- `akashi_query` — filter or search the audit trail
- `akashi_conflicts` — list open conflicts between agents
- `akashi_assess` — record whether a prior decision was correct
- `akashi_stats` — aggregate health metrics

Be honest about confidence. Reference precedents when they influence you.
