# Frequently asked questions

## General

### What is Akashi?

Akashi is a decision coordination layer for multi-agent AI systems. It records every decision an AI agent makes — what was decided, why, what alternatives were considered, and how confident the agent was — then makes that history searchable and auditable. When agents contradict each other, Akashi detects it automatically.

### What problem does Akashi solve?

In production multi-agent systems, agents operate independently and have no shared memory of prior decisions. This leads to three problems:

1. **Contradiction.** Agent A decides to use PostgreSQL; Agent B decides to use MySQL. Neither knows the other decided.
2. **Relitigation.** A question that was already settled gets re-debated from scratch because no agent remembers the prior decision.
3. **Opacity.** When something goes wrong, there is no trail to answer "who decided what, when, and why?"

Akashi solves all three by giving agents a shared, searchable decision history with automatic conflict detection.

### How is this different from a log aggregator or observability tool?

Log aggregators collect operational events (errors, request traces, metrics). Akashi collects *decisions* — structured records that include reasoning, alternatives considered, confidence levels, and supporting evidence. Decisions are semantically indexed so agents can find relevant precedents by meaning, not just keyword. Conflict detection compares decisions across agents to find genuine contradictions, which is not something log aggregators do.

### Is Akashi open source?

Yes. Akashi is released under the Apache 2.0 license.

### What AI agents and frameworks does Akashi work with?

Akashi is agent-agnostic. Any agent that can make HTTP calls or use MCP (Model Context Protocol) can integrate. SDKs are available for Go, Python, and TypeScript. The MCP interface works with Claude Code, Cursor, Windsurf, and any other MCP-compatible client. Framework integrations for LangChain, CrewAI, and Vercel AI SDK are planned.

---

## Setup and deployment

### What are the deployment options?

Three modes, from most to least infrastructure:

| Mode | Infrastructure | Status |
|------|---------------|--------|
| **Complete local stack** | Docker Compose with TimescaleDB, Qdrant, Ollama | Available |
| **Binary only** | Bring your own PostgreSQL (with pgvector + TimescaleDB) | Available |
| **Local-lite** | Zero-dependency binary with SQLite | Coming soon ([#312](https://github.com/ashita-ai/akashi/issues/312)) |

### How do I get started quickly?

```bash
docker compose -f docker-compose.complete.yml up -d
```

This starts everything — database, vector search, embedding model, LLM for conflict validation, and the Akashi server. No API keys or external accounts needed. First launch takes 15–25 minutes to download models; subsequent starts take seconds.

### What are the system requirements?

For the complete local stack: Docker with at least 8GB of available memory (the Ollama models need RAM). The server itself is a single Go binary (~50MB container image) and is lightweight. PostgreSQL 18 with pgvector and TimescaleDB extensions is required for the binary-only deployment.

### How do I connect my AI agent to Akashi?

**MCP (recommended for Claude Code, Cursor, Windsurf):**

```bash
claude mcp add --transport http akashi http://localhost:8080/mcp \
  --header "Authorization: ApiKey admin:$AKASHI_ADMIN_API_KEY"
```

**SDK (recommended for programmatic integration):**

```python
# Python
from akashi import AsyncAkashiClient

client = AsyncAkashiClient(base_url="http://localhost:8080", api_key="admin:your-key")
await client.trace(
    decision_type="architecture",
    outcome="Use PostgreSQL for persistence",
    reasoning="Mature ecosystem, pgvector for embeddings",
    confidence=0.85,
)
```

**HTTP API (works with any language):**

```bash
curl -X POST http://localhost:8080/v1/trace \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"decision_type":"architecture","outcome":"Use PostgreSQL","confidence":0.85}'
```

---

## Core concepts

### What is a decision in Akashi?

A decision is a structured record with:

- **outcome** — what was decided
- **reasoning** — why it was decided (optional but scored)
- **decision_type** — a label like `architecture`, `code_review`, or `security`
- **confidence** — a 0.0–1.0 score indicating the agent's certainty
- **alternatives** — other options considered, with scores and rejection reasons
- **evidence** — references and supporting facts

Decisions are immutable once recorded. Revisions create new records that supersede the original.

### What does "check before deciding, trace after deciding" mean?

This is the core workflow:

1. **Before** making a decision, the agent calls `akashi_check` with a description of what it is about to decide. Akashi returns any relevant precedents and known conflicts, so the agent can avoid contradicting prior work.
2. **After** making a decision, the agent calls `akashi_trace` with the full decision record. Akashi stores it, computes embeddings, runs conflict detection, and makes it searchable for future agents.

### How does conflict detection work?

When a new decision is traced, Akashi:

1. Finds ~20 semantically similar past decisions via vector search
2. Computes a significance score for each pair based on topic similarity, outcome divergence, confidence levels, and temporal decay
3. Optionally validates candidates with an LLM to classify the relationship (contradiction, supersession, refinement, complementary, or unrelated)
4. Stores genuine contradictions and supersessions as conflicts

Conflicts have a lifecycle: `open` → `resolved` (with a declared winner) or `false_positive`. They can be resolved via the API, the dashboard, or automatically when a decision is revised.

### What is the bi-temporal model?

Every decision has two time dimensions:

- **Business time** (`valid_from` / `valid_to`) — when the decision was valid in the real world. Active decisions have `valid_to IS NULL`. When a decision is revised, the old record gets a `valid_to` timestamp and a new record is created.
- **System time** (`transaction_time`) — when the decision was recorded in Akashi. This never changes.

This lets you answer both "what was the active decision at time T?" and "what did we know when we recorded this?"

### What is a completeness score?

Every traced decision receives a quality score (0.0–1.0) based on how much context the agent provided:

| Factor | Max weight |
|--------|-----------|
| Reasoning length and detail | 0.30 |
| Alternatives with rejection reasons | 0.20 |
| Confidence (penalizes extremes of 0 or 1) | 0.15 |
| Evidence items | 0.15 |
| Outcome length | 0.10 |
| Precedent reference | 0.10 |

Higher-quality decisions surface higher in precedent search results. The scoring has anti-gaming measures — padding fields with filler text does not help.

### What is `akashi_assess`?

After a decision plays out, an agent (or human) can call `akashi_assess` to record whether the decision turned out to be correct. This feedback is incorporated into search re-ranking so that decisions with better track records surface higher as precedents over time.

---

## Authentication and access control

### How does authentication work?

Two mechanisms:

- **API keys** — `ApiKey <agent_id>:<key>` header. Never expire, survive server restarts. Recommended for configuration files and long-running integrations. Keys are hashed with Argon2id (memory-hard, GPU-resistant).
- **JWT tokens** — `Bearer <token>` header. Ed25519-signed, expire after 24 hours by default. Obtained by exchanging an API key at `POST /auth/token`.

### What are the RBAC roles?

Five roles in descending privilege order:

| Role | Level | Can do |
|------|-------|--------|
| `platform_admin` | 5 | Cross-org operations, system config |
| `org_owner` | 4 | Full control within their org |
| `admin` | 3 | Manage agents, grants, and config within org |
| `agent` | 2 | Submit traces, query own and granted data |
| `reader` | 1 | Read-only access to explicitly granted resources |

### How does multi-tenancy work?

Every query is scoped by `org_id`. Agents belong to exactly one organization, and all data access is filtered by the agent's org. There are over 230 org_id filters across the storage layer. Within an org, fine-grained access grants let one agent share its decisions with specific other agents.

---

## Integrity and auditing

### How does Akashi ensure decisions haven't been tampered with?

Three layers:

1. **Content hashing** — every decision, alternative, and evidence record gets a SHA-256 hash computed from its content.
2. **Merkle tree proofs** — periodically (every 5 minutes by default), Akashi builds a Merkle tree from recent decision hashes and stores the root. An auditor can verify any individual decision against the tree.
3. **Event audit trail** — every mutation is recorded as an immutable event, including erasures (for GDPR compliance).

### Does Akashi support GDPR erasure?

Yes. Decisions can be erased via tombstone erasure, which scrubs the content but preserves the structural record and a snapshot of the content hash (so integrity proofs remain valid). Legal holds can prevent erasure of specific decisions. See [docs/erasure.md](erasure.md) for details.

---

## Operations

### What happens if Qdrant goes down?

Akashi degrades gracefully. Semantic vector search falls back to PostgreSQL full-text search (BM25-style). Decisions are still stored and queryable — you lose semantic similarity ranking until Qdrant recovers. Failed Qdrant syncs retry with exponential backoff and dead-letter after 10 attempts.

### What happens if the embedding provider is unavailable?

Akashi tries providers in order: Ollama → OpenAI → noop. If all are unavailable, decisions are still stored but without embeddings. Semantic search and conflict detection are disabled until a provider recovers.

### Where can I find operational runbooks?

See [docs/runbook.md](runbook.md) for health checks, monitoring, alerting, and troubleshooting procedures.

### How do I configure Akashi?

All configuration is via environment variables with the `AKASHI_` prefix. See [docs/configuration.md](configuration.md) for the full reference. The minimum required variables are:

- `DATABASE_URL` — PostgreSQL connection string
- `AKASHI_ADMIN_API_KEY` — bootstrap API key for the admin agent

The Docker Compose setup handles both automatically.

---

## SDKs and integration

### Which SDKs are available?

| Language | Install | Notes |
|----------|---------|-------|
| Go | `go get github.com/ashita-ai/akashi/sdk/go/akashi` | Zero external dependencies beyond `google/uuid` |
| Python | `pip install akashi` | Async and sync clients, requires Python 3.10+ |
| TypeScript | `npm install akashi` | Zero runtime dependencies, native `fetch` |

All three SDKs expose the same methods: `Check`, `Trace`, `Query`, `Search`, `Recent`, and `Assess`.

### Can I use Akashi without an SDK?

Yes. The HTTP API at `/v1/...` is fully documented in the OpenAPI spec served at `/openapi.yaml`. Any HTTP client works.

### How do I use Akashi with Claude Code?

```bash
# Add the MCP server
claude mcp add --transport http akashi http://localhost:8080/mcp \
  --header "Authorization: ApiKey admin:$AKASHI_ADMIN_API_KEY"

# Install the post-commit hook (optional, reminds you to trace after commits)
make install-hooks
```

Once configured, Claude Code has access to `akashi_check`, `akashi_trace`, `akashi_query`, `akashi_conflicts`, `akashi_resolve`, `akashi_assess`, and `akashi_stats` as MCP tools.

---

## Architecture

### Why Go?

Go compiles to a single static binary, has excellent concurrency primitives for the event ingestion pipeline, and a mature standard library for HTTP servers. See [ADR-001](../adrs/ADR-001-server-language.md) for the full rationale.

### Why PostgreSQL instead of a dedicated event store?

PostgreSQL with pgvector and TimescaleDB provides event storage (hypertables), vector search (pgvector), and relational queries in a single dependency. This avoids the operational cost of running separate systems for each concern. Qdrant is optional and additive. See [ADR-002](../adrs/ADR-002-unified-postgresql.md).

### Why Ed25519 for JWTs instead of RS256?

Ed25519 (EdDSA) is ~20x faster than RSA-2048 for signing and verification, produces smaller keys and signatures, and has no known timing side-channels. See [ADR-005](../adrs/ADR-005-auth-rbac.md).

### How does the event-sourced architecture work?

The `agent_events` table is an append-only TimescaleDB hypertable — the source of truth. Events (`DecisionMade`, `DecisionRevised`, `DecisionErased`, etc.) are flushed in batches via `COPY` for high throughput. Materialized tables (`decisions`, `alternatives`, `evidence`) are derived from events for efficient querying. See [ADR-003](../adrs/ADR-003-event-sourced-bitemporal.md).
