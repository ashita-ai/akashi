# Akashi Framework Integrations

Drop-in integrations that wire [Akashi](../../README.md) decision tracing into popular AI frameworks. Each package is a thin adapter — it maps the framework's native lifecycle events to Akashi `check()` and `trace()` calls, with zero changes to your existing agent code.

## Packages

| Package | Framework | Language | Description |
|---------|-----------|----------|-------------|
| [`akashi-langchain`](langchain/) | LangChain | Python | Callback handler for agent tool use and final answers |
| [`akashi-crewai`](crewai/) | CrewAI | Python | Task and step hooks for multi-agent crews |
| [`akashi-vercel-ai`](vercel-ai/) | Vercel AI SDK | TypeScript | `LanguageModelV1` middleware for `generateText` and `streamText` |

## Design

All integrations follow the same pattern:

1. **`check()` before acting** — Before the agent selects a tool, runs a generation, or kicks off a crew, the integration calls `check()` with the current context as the query. This surfaces any relevant prior decisions from the audit trail, giving the agent (or the system around it) the opportunity to factor in precedent.

2. **`trace()` after completing** — After each significant event (tool result, task completion, stream close), the integration calls `trace()` to record what happened. The trace captures the outcome, reasoning, and metadata so the decision is preserved in the audit trail.

3. **Fire-and-forget** — All Akashi calls are wrapped in `try/except` or `try/catch`. If the Akashi server is unreachable or slow, the failure is logged at `DEBUG` level and the framework execution continues normally. The integrations are designed to add zero latency to the unhappy path.

## Common setup

All integrations use the same Akashi client credentials:

```python
# Python
from akashi import AkashiSyncClient

client = AkashiSyncClient(
    base_url="http://localhost:8080",   # or your Akashi Cloud URL
    agent_id="my-agent",
    api_key="your-api-key",
)
```

```typescript
// TypeScript
import { AkashiClient } from "akashi";

const client = new AkashiClient({
  baseUrl: "http://localhost:8080",
  agentId: "my-agent",
  apiKey: process.env.AKASHI_API_KEY!,
});
```

See the individual package READMEs for framework-specific setup and options.
