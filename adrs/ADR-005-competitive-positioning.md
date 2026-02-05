# ADR-005: Competitive positioning — decision traces, not memory

**Status:** Accepted
**Date:** 2026-02-03

## Context

The agent memory infrastructure space has multiple players. We need a clear positioning that avoids competing head-on with well-funded incumbents.

## Decision

Akashi is a **decision trace layer**, not a general memory layer. We capture structured decision context: what was decided, what alternatives were considered, what evidence supported it, and how confident the agent was.

We do NOT build:
- General-purpose agent memory (Mem0's space)
- Temporal knowledge graphs (Zep's space)
- Orchestration (LangGraph's space)
- Observability dashboards (Langfuse/Phoenix's space)

## The Gap

No existing product answers: "Why did the agent decide this, what alternatives were considered, what evidence supported it, and has this situation come up before?"

| Product | Multi-agent memory | Decision traces | Evidence/confidence | Reasoning lineage |
|---------|--------------------|----------------|---------------------|-------------------|
| Zep | Strong (temporal KG) | Weak | No | No |
| Mem0 | Good (universal) | No | No | No |
| LangGraph | Within-graph only | Checkpoints only | No | No |
| Letta | Single-agent | No | No | No |
| Langfuse | No (observability) | Traces, not decisions | No | No |
| **Akashi** | **Yes** | **Yes** | **Yes** | **Yes** |

## Rationale

**Why not compete on general memory:**

- Mem0 raised $24M (Oct 2025), Zep has Y Combinator backing. They have head starts on general memory.
- Decision traces are structurally harder to retrofit — deep modeling of alternatives, evidence, and confidence is not a feature you bolt on.

**Why decision traces are the wedge:**

- Regulatory demand: AI audit trails are becoming mandatory (EU AI Act, SEC guidance).
- Debugging demand: "why did the agent do that?" is the #1 question in multi-agent systems.
- Coordination demand: agents sharing structured decisions (not just facts) makes multi-agent systems measurably better.

**Differentiation from OTEL:**

- OTEL shows *that* something happened and *how long* it took.
- Akashi shows *why* it happened, *what evidence* supported it, and *how confident* the agent was.

## Consequences

- Data model is optimized for decisions, alternatives, evidence, reasoning chains.
- API surface (`trace`, `query`, `search`) is decision-centric, not memory-centric.
- Marketing and docs emphasize "decision traces" and "agent audit trail", not "agent memory".
- We complement Mem0/Zep (they handle facts, we handle decisions), not compete.

## References

- Research: `ventures/specs/01-competitive-landscape.md`
- Research: `ventures/specs/06-architecture-synthesis.md`
