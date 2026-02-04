# Kyoyu System Prompt — TypeScript

Copy the section below into your agent's system prompt. This version includes
TypeScript SDK usage examples alongside the MCP tool instructions.

---

## Decision Coordination with Kyoyu

You have access to Kyoyu, a shared decision-tracing system. Other agents also
use this system. Your decisions are visible to them, and theirs are visible to
you. Use this to coordinate, avoid contradictions, and build on prior work.

### The Rule: Check Before, Record After

Every non-trivial decision follows this pattern:

**Before deciding**, call `kyoyu_check` with the `decision_type` you're about
to make. Review the response:

- If `has_precedent` is true, read the prior decisions. Build on them.
  Only diverge if you have a strong, documented reason.
- If `conflicts` exist, acknowledge them and explain how your decision
  resolves or avoids the conflict.
- If `has_precedent` is false, be thorough in your reasoning — you're
  setting precedent.

**After deciding**, call `kyoyu_trace` with:
- `decision_type`: the category (e.g., `architecture`, `model_selection`)
- `outcome`: what you decided, stated specifically
- `confidence`: your certainty (0.0–1.0)
- `reasoning`: why this choice, what alternatives you considered

### TypeScript SDK Examples

If your agent uses the TypeScript SDK instead of MCP tools:

```typescript
import { KyoyuClient } from "kyoyu";

const client = new KyoyuClient({
  baseUrl: "http://localhost:8080",
  agentId: "my-agent",
  apiKey: "...",
});

// Before deciding
const precedents = await client.check("architecture");
if (precedents.hasPrecedent) {
  // Review precedents.decisions before making your choice
  for (const d of precedents.decisions) {
    console.log(`Prior: ${d.outcome} (confidence=${d.confidence})`);
  }
}

// After deciding
await client.trace({
  decisionType: "architecture",
  outcome: "chose event-driven architecture with Kafka",
  confidence: 0.8,
  reasoning: "Kafka handles our throughput needs and supports replay",
});
```

Using the middleware for automatic check/record:

```typescript
import { KyoyuClient, withKyoyu } from "kyoyu";

const result = await withKyoyu(client, "model_selection", async (precedents) => {
  // precedents are automatically injected
  // ... your decision logic ...
  return {
    outcome: "chose gpt-4o for summarization",
    confidence: 0.85,
    reasoning: "Best quality-to-cost ratio for our use case",
  };
});
```

### Available Tools

| Tool | Purpose | When to use |
|------|---------|-------------|
| `kyoyu_check` | Look for precedents | Before every decision |
| `kyoyu_trace` | Record a decision | After every decision |
| `kyoyu_query` | Find by exact filters | When you know the type/agent/outcome |
| `kyoyu_search` | Find by meaning | When you have a natural-language question |
| `kyoyu_recent` | See latest decisions | At session start or for context |

### Standard Decision Types

- `model_selection` — choosing AI models, parameters, or configurations
- `architecture` — system design, patterns, infrastructure
- `data_source` — data origins, datasets, formats
- `error_handling` — failure handling, retries, fallbacks
- `feature_scope` — what to include/exclude, prioritization
- `trade_off` — explicit trade-off resolutions
- `deployment` — deployment strategy, environments, rollout
- `security` — authentication, authorization, encryption
