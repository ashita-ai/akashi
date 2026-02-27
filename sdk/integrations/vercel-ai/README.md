# akashi-vercel-ai

Vercel AI SDK middleware that traces LLM calls to [Akashi](../../../README.md) -- the black box recorder for AI decisions.

Wraps any `LanguageModelV1`-compatible model so that every `generateText`, `generateObject`, `streamText`, and `streamObject` call automatically:
1. Calls `check()` before generation to surface relevant precedents.
2. Calls `trace()` after generation to record the LLM output as a decision.

All Akashi calls are **fire-and-forget**: errors are silently swallowed so they never interrupt the model call or the stream.

**Requirements:** Node.js 18+, `ai>=4.0.0`, `akashi>=0.1.0`

## Install

```bash
npm install akashi-vercel-ai
# peer dependencies (if not already installed):
npm install ai akashi
```

## Quick start

```typescript
import { generateText, streamText, wrapLanguageModel } from "ai";
import { openai } from "@ai-sdk/openai";
import { AkashiClient } from "akashi";
import { createAkashiMiddleware } from "akashi-vercel-ai";

const akashi = new AkashiClient({
  baseUrl: "https://your-akashi.example.com",
  agentId: "my-agent",
  apiKey: process.env.AKASHI_API_KEY!,
});

// Wrap a model once. The middleware applies to every call made with it.
const model = wrapLanguageModel({
  model: openai("gpt-4o"),
  middleware: createAkashiMiddleware(akashi),
});

// generateText — check() before, trace() after.
const { text } = await generateText({
  model,
  prompt: "What is the capital of France?",
});

// streamText — check() before, trace() after the stream closes.
const result = await streamText({
  model,
  prompt: "Summarize the latest AI news in three bullet points.",
});
for await (const chunk of result.textStream) {
  process.stdout.write(chunk);
}
```

## How it works

The middleware implements `LanguageModelV1Middleware` from the Vercel AI SDK:

| AI SDK call | Middleware hook | Akashi call | Timing |
|-------------|-----------------|-------------|--------|
| `generateText` / `generateObject` | `wrapGenerate` | `check()` | Before `doGenerate()` |
| `generateText` / `generateObject` | `wrapGenerate` | `trace()` | After `doGenerate()` resolves |
| `streamText` / `streamObject` | `wrapStream` | `check()` | Before `doStream()` |
| `streamText` / `streamObject` | `wrapStream` | `trace()` | After the stream closes (on `flush`) |

**Check query:** The last user message in the prompt is extracted and sent as the `check()` query (truncated to 200 characters), giving Akashi enough context to find relevant prior decisions.

**Stream tracing:** The original stream is piped through a `TransformStream` that accumulates `text-delta` parts. `trace()` is called in the `flush()` handler — exactly once, when the consumer finishes reading the stream. The stream itself is not buffered or delayed; chunks pass through immediately.

**Non-streaming outcome:** For `generateText`, `result.text` is used. For `generateObject` and other tool-call responses, the outcome falls back to a `tool:<name>(<args>)` summary.

## API

### `createAkashiMiddleware(client, options?)`

Returns a `LanguageModelV1Middleware` for use with `wrapLanguageModel`.

```typescript
import { wrapLanguageModel } from "ai";
import { createAkashiMiddleware } from "akashi-vercel-ai";

const model = wrapLanguageModel({
  model: baseModel,
  middleware: createAkashiMiddleware(client, {
    decisionType: "summarization",
    confidence: 0.8,
  }),
});
```

**`client`** — A configured `AkashiClient` instance (required).

**`options`** — All fields are optional:

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `decisionType` | `string` | `"llm_call"` | Label recorded for every trace and check |
| `confidence` | `number` | `0.7` | Confidence score for traces (0–1) |
| `checkBeforeGenerate` | `boolean` | `true` | Call `check()` before each generation |
| `traceGenerations` | `boolean` | `true` | Call `trace()` after non-streaming generations |
| `traceStreams` | `boolean` | `true` | Call `trace()` after streaming generations |

### Selective tracing

Disable specific hooks to reduce trace volume or latency:

```typescript
// Observe only — never trace, just check for precedents.
const model = wrapLanguageModel({
  model: baseModel,
  middleware: createAkashiMiddleware(client, {
    checkBeforeGenerate: true,
    traceGenerations: false,
    traceStreams: false,
  }),
});

// Trace but skip the pre-generation check.
const model = wrapLanguageModel({
  model: baseModel,
  middleware: createAkashiMiddleware(client, {
    checkBeforeGenerate: false,
  }),
});
```

### Multiple models, multiple decision types

Create one middleware instance per model or use case:

```typescript
const summaryModel = wrapLanguageModel({
  model: openai("gpt-4o-mini"),
  middleware: createAkashiMiddleware(akashi, { decisionType: "summarization" }),
});

const reasoningModel = wrapLanguageModel({
  model: openai("o1"),
  middleware: createAkashiMiddleware(akashi, {
    decisionType: "multi_step_reasoning",
    confidence: 0.9,
  }),
});
```

## Error handling

All Akashi calls are wrapped in `try/catch`. If the Akashi server is unreachable or returns an error:
- The `check()` failure is silently ignored and `doGenerate()` / `doStream()` proceeds.
- A `trace()` failure after generation does not affect the returned result or stream.
- The model call always completes normally regardless of Akashi status.

## Truncation

| Field | Limit |
|-------|-------|
| `outcome` (generated text, stream accumulation) | 500 characters |
| Check query (last user message) | 200 characters |

## TypeScript

The package is written in TypeScript and ships with full type declarations. No `@types` installation required.

```typescript
import type { AkashiMiddlewareOptions } from "akashi-vercel-ai";

const opts: AkashiMiddlewareOptions = {
  decisionType: "my_decision",
  confidence: 0.85,
  checkBeforeGenerate: true,
  traceGenerations: true,
  traceStreams: false,
};
```
