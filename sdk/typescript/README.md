# Akashi TypeScript SDK

TypeScript client for the [Akashi](../../README.md) decision audit API -- the black box recorder for AI decisions. Uses native `fetch` with zero runtime dependencies.

**Requirements:** Node.js 18+ or any runtime with global `fetch` (Deno, Bun, Cloudflare Workers)

## Install

```bash
npm install akashi
# or from source:
cd sdk/typescript && npm install && npx tsc
```

## Quick start

```typescript
import { AkashiClient } from "akashi";

const client = new AkashiClient({
  baseUrl: "http://localhost:8080",
  agentId: "my-agent",
  apiKey: "my-api-key",
});

// Check for precedents before making a decision.
const check = await client.check("model_selection");
if (check.has_precedent) {
  console.log(`Found ${check.decisions.length} prior decisions`);
}

// Record a decision.
const resp = await client.trace({
  decisionType: "model_selection",
  outcome: "chose gpt-4o for summarization",
  confidence: 0.85,
  reasoning: "Best quality-to-cost ratio",
  alternatives: [
    { label: "gpt-4o", selected: true, score: 0.92 },
    { label: "claude-3-haiku", selected: false, score: 0.78 },
  ],
});
console.log(`Recorded decision ${resp.decision_id}`);
```

## API

### `new AkashiClient(config)`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `baseUrl` | `string` | yes | | Server URL |
| `agentId` | `string` | yes | | Agent identifier |
| `apiKey` | `string` | yes | | Secret for JWT acquisition |
| `timeoutMs` | `number` | no | `30000` | Request timeout in ms |

### Methods

| Method | Returns | Description |
|--------|---------|-------------|
| `check(decisionType, query?, options?)` | `CheckResponse` | Precedent lookup |
| `trace(request)` | `TraceResponse` | Record a decision |
| `query(filters?, options?)` | `QueryResponse` | Structured query |
| `search(query, limit?)` | `SearchResponse` | Semantic search |
| `recent(options?)` | `Decision[]` | Recent decisions |

## Middleware

The `withAkashi` function wraps a decision-making function with automatic check and trace:

```typescript
import { AkashiClient, withAkashi } from "akashi";
import type { CheckResponse, TraceRequest } from "akashi";

const client = new AkashiClient({ baseUrl: "...", agentId: "...", apiKey: "..." });

const result = await withAkashi(client, "model_selection", async (precedents) => {
  // Use precedents.decisions to inform your choice...
  const model = precedents.has_precedent ? precedents.decisions[0].outcome : "gpt-4o";

  return {
    model,
    toTrace: (): TraceRequest => ({
      decisionType: "model_selection",
      outcome: `chose ${model}`,
      confidence: 0.85,
      reasoning: "Based on precedent analysis",
    }),
  };
});

console.log(result.model);
```

The return value must implement the `Traceable` interface (a `toTrace()` method returning `TraceRequest`).

## Error handling

All errors extend `AkashiError` and include `statusCode` and `message`:

```typescript
import {
  AkashiError,           // Base class
  AuthenticationError,   // 401
  AuthorizationError,    // 403
  NotFoundError,         // 404
  ValidationError,       // 400
  ConflictError,         // 409
  ServerError,           // 5xx
} from "akashi";

try {
  await client.check("...");
} catch (err) {
  if (err instanceof NotFoundError) {
    // handle 404
  }
}
```

## Types

All types are exported from the main entry point:

```typescript
import type {
  Decision, Alternative, Evidence, DecisionConflict,
  TraceRequest, TraceAlternative, TraceEvidence,
  TraceResponse, CheckResponse, QueryResponse,
  SearchResult, SearchResponse, QueryFilters,
  Traceable, AkashiConfig,
} from "akashi";
```
