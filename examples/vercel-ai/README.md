# Vercel AI SDK + Akashi Example

Automatic decision tracing for Vercel AI SDK calls using `createAkashiMiddleware`. Every `generateText` and `streamText` call is recorded to the Akashi audit trail — no manual `trace()` calls needed.

## What it demonstrates

1. Wrapping an OpenAI model with `createAkashiMiddleware`
2. Non-streaming generation (`generateText`) with automatic check + trace
3. Streaming generation (`streamText`) with automatic check + trace on stream close
4. Querying the audit trail to verify decisions were recorded

## Prerequisites

- A running Akashi instance (local or remote)
- An OpenAI API key

Start the local stack from the repo root:

```sh
docker compose -f docker-compose.complete.yml up -d
```

## Setup

```sh
cd examples/vercel-ai
npm install
```

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `AKASHI_URL` | `http://localhost:8080` | Akashi server URL |
| `AKASHI_ADMIN_API_KEY` | `admin` | Admin API key (matches docker-compose) |
| `OPENAI_API_KEY` | *(required)* | OpenAI API key for `gpt-4o-mini` |

## Run

```sh
OPENAI_API_KEY=sk-... npm start
```

## Expected output

```
==> Connected to Akashi v0.x.x (postgres: ok)
==> Created agent 'vercel-ai-example'

--- generateText (non-streaming) ---

Response: The CAP theorem states that a distributed system can only...

--- streamText (streaming) ---

Response: Decision audit trails provide accountability by...

--- Verifying audit trail ---

Found 2 auto-traced decision(s):
  - [llm_call] Decision audit trails provide accountability by...
    confidence=0.80, agent=vercel-ai-example
  - [llm_call] The CAP theorem states that a distributed system can only...
    confidence=0.80, agent=vercel-ai-example

==> Done. No explicit trace() calls were made — the middleware handled it.
==> View your decisions at http://localhost:8080
```
