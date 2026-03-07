# Self-Hosting Guide

This guide walks you through deploying Akashi on your own infrastructure. It covers four progressively richer configurations — from a minimal Postgres-only setup to a full stack with local embeddings and LLM conflict validation.

For the complete environment variable reference, see [configuration.md](configuration.md).

## Prerequisites

- **Docker and Docker Compose** (v2+)
- **PostgreSQL 18+** with the [pgvector](https://github.com/pgvector/pgvector) and [TimescaleDB](https://www.timescale.com/) extensions

If you use the provided `docker-compose` files, the database is included. If you bring your own Postgres, install both extensions before starting Akashi — the server checks for them at startup.

The init script that the Docker images use is at [`docker/init.sql`](../docker/init.sql):

```sql
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS timescaledb;
```

## 1. Minimal setup (Postgres only)

This is the simplest deployment. Search uses PostgreSQL full-text search (tsvector/tsquery). Semantic search, embeddings, and conflict detection are disabled.

Create a `.env` file:

```bash
DATABASE_URL=postgres://akashi:akashi@localhost:5432/akashi?sslmode=disable
AKASHI_ADMIN_API_KEY=changeme
AKASHI_EMBEDDING_PROVIDER=noop
```

Start the database and server:

```bash
# Start TimescaleDB
docker run -d --name akashi-pg \
  -e POSTGRES_USER=akashi -e POSTGRES_PASSWORD=akashi -e POSTGRES_DB=akashi \
  -v "$(pwd)/docker/init.sql:/docker-entrypoint-initdb.d/01-init.sql:ro" \
  -p 5432:5432 timescale/timescaledb-ha:pg18

# Build and start Akashi (from the repo root)
docker compose up -d
```

Setting `AKASHI_EMBEDDING_PROVIDER=noop` explicitly disables embeddings. Without it, the default `auto` mode probes for Ollama and OpenAI on every request.

## 2. With Qdrant (vector search)

Adding [Qdrant](https://qdrant.tech/) enables semantic vector search. You also need an embedding provider (see sections 3 and 4).

```bash
# Start Qdrant
docker run -d --name akashi-qdrant \
  -p 6333:6333 qdrant/qdrant:v1.13.6
```

Add to `.env`:

```bash
QDRANT_URL=http://localhost:6333
# QDRANT_COLLECTION=akashi_decisions    # default, change if running multiple instances
```

When `QDRANT_URL` is set, Akashi syncs decision embeddings via an outbox worker. Without Qdrant, search falls back to PostgreSQL full-text search automatically.

## 3. With Ollama (local embeddings)

[Ollama](https://ollama.com/) provides local embedding generation — no API keys or external network calls.

```bash
# Start Ollama
docker run -d --name akashi-ollama \
  -p 11434:11434 ollama/ollama

# Pull the default embedding model (~670MB)
docker exec akashi-ollama ollama pull mxbai-embed-large
```

Add to `.env`:

```bash
OLLAMA_URL=http://localhost:11434
OLLAMA_MODEL=mxbai-embed-large
AKASHI_EMBEDDING_PROVIDER=ollama
AKASHI_EMBEDDING_DIMENSIONS=1024
```

For LLM-based conflict validation (reduces false positives by ~93%), also pull an instruction-following model:

```bash
docker exec akashi-ollama ollama pull qwen3.5:9b
```

And add:

```bash
AKASHI_CONFLICT_LLM_MODEL=qwen3.5:9b
```

Alternatively, use `docker-compose.complete.yml` which wires all of this automatically:

```bash
docker compose -f docker-compose.complete.yml up -d
```

## 4. With OpenAI (cloud embeddings)

If you prefer cloud-hosted embeddings, set your OpenAI API key:

```bash
OPENAI_API_KEY=sk-...
AKASHI_EMBEDDING_PROVIDER=openai
AKASHI_EMBEDDING_MODEL=text-embedding-3-small
AKASHI_EMBEDDING_DIMENSIONS=1024
```

When `OPENAI_API_KEY` is set, LLM conflict validation uses `gpt-4o-mini` automatically — no additional configuration needed.

## 5. Persistent JWT signing keys

Without persistent keys, Akashi generates an ephemeral Ed25519 key pair on every startup. This invalidates all existing JWT tokens and browser sessions on each restart.

Generate persistent keys once:

```bash
mkdir -p data
openssl genpkey -algorithm ed25519 -out data/jwt_private.pem
openssl pkey -in data/jwt_private.pem -pubout -out data/jwt_public.pem
chmod 600 data/jwt_private.pem data/jwt_public.pem
```

Add to `.env`:

```bash
AKASHI_JWT_PRIVATE_KEY=/data/jwt_private.pem
AKASHI_JWT_PUBLIC_KEY=/data/jwt_public.pem
```

Both `docker-compose.yml` and `docker-compose.complete.yml` mount `./data` as `/data` inside the container. The `docker-compose.complete.yml` stack generates these keys automatically on first launch.

## 6. Verify it works

Check server health:

```bash
curl -s http://localhost:8080/health | jq .
```

Expected response:

```json
{"status":"healthy"}
```

Record a test decision:

```bash
# Get a token
TOKEN=$(curl -s -X POST http://localhost:8080/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"agent_id": "admin", "api_key": "changeme"}' \
  | jq -r '.data.token')

# Trace a decision
curl -s -X POST http://localhost:8080/v1/trace \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "agent_id": "admin",
    "decision": {
      "decision_type": "test",
      "outcome": "self-hosting guide verification",
      "confidence": 1.0,
      "reasoning": "verifying the deployment works"
    }
  }' | jq .
```

A `201` response with a decision ID confirms the full write path (HTTP -> auth -> event buffer -> Postgres) is working.

Open `http://localhost:8080` in a browser to see the audit dashboard.

## 7. Troubleshooting

### `connection refused` or `FATAL: role "akashi" does not exist`

The `DATABASE_URL` is wrong or Postgres is not running. Verify:

```bash
psql "$DATABASE_URL" -c "SELECT 1"
```

If using Docker, check the container is healthy:

```bash
docker inspect --format='{{.State.Health.Status}}' akashi-pg
```

### `extension "vector" is not available` or `extension "timescaledb" is not available`

The Postgres instance is missing required extensions. Akashi requires both `pgvector` and `timescaledb`. If you are running your own Postgres (not the provided Docker image), install the extensions and run:

```sql
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS timescaledb;
```

The provided `timescale/timescaledb-ha:pg18` image includes both extensions. Use it if you do not want to manage extension installation yourself.

### `AKASHI_ADMIN_API_KEY is required` at startup

The server refuses to start without a bootstrap admin key when no agents exist yet. Set `AKASHI_ADMIN_API_KEY` in your `.env` or environment.

### All tokens invalidated after restart

You are using ephemeral JWT signing keys (the default). Generate persistent keys as described in [section 5](#5-persistent-jwt-signing-keys) above.

### Embeddings stuck at zero vectors / semantic search returns nothing

Check which embedding provider is active in the startup logs:

```bash
docker compose logs akashi | grep -i embed
```

Common causes:

- **Ollama not reachable:** Verify `OLLAMA_URL` points to a running Ollama instance and the model has been pulled (`ollama list`).
- **OpenAI key not set:** `auto` mode falls through to `noop` when neither Ollama nor OpenAI is available. Set `OPENAI_API_KEY` or configure Ollama.
- **Dimension mismatch:** `AKASHI_EMBEDDING_DIMENSIONS` must match the model output. The default `1024` works for `mxbai-embed-large` and `text-embedding-3-small`.

### Qdrant sync not working

Check the outbox worker logs:

```bash
docker compose logs akashi | grep -i outbox
```

Verify Qdrant is reachable:

```bash
curl -s http://localhost:6333/collections | jq .
```

If the collection does not exist, Akashi creates it automatically on first sync. If the collection exists but has a different vector dimension, delete it and let Akashi recreate it.

### Port already in use

Set a different port in `.env`:

```bash
AKASHI_PORT=8081
```

Then restart:

```bash
docker compose up -d
```
