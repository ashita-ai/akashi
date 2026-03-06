# ADR-009: Distribution tiers — local-lite and cloud-hosted MCP

**Status:** Accepted
**Date:** 2026-03-06
**Amends:** ADR-004 (MCP and framework integrations as primary distribution channels)

## Context

ADR-004 described MCP as "one line of config." In practice the self-hosted setup requires Docker Compose, TimescaleDB, Qdrant, and Ollama. First launch takes 15–25 minutes as Ollama pulls a 670 MB embedding model and a 6.6 GB LLM. That is not one line of config — it is a meaningful infrastructure commitment before a user has had any opportunity to evaluate whether the check-before/trace-after workflow is useful to them.

The most-used MCP tools share a property: they work in under 60 seconds with zero infrastructure. Akashi cannot compete on that axis with full self-hosted as the only entry point.

Two concrete gaps compound the problem:

**Gap 1 — No zero-infra local path.** There is no way to use Akashi without running a database. The full stack is the only OSS option.

**Gap 2 — JWT auth in MCP config files.** JWTs expire in 24h and are invalidated on server restart (with the default ephemeral signing key). `ApiKey` credentials (non-expiring, wired in the middleware since the spec 37 security hardening) were never documented for MCP use. Users who followed the README were putting credentials in their MCP config that would silently break overnight.

## Decision

### Two new distribution tiers (amending ADR-004)

| Layer | Target | Integration effort | Status |
|-------|--------|--------------------|--------|
| **Local-lite** (`npx @akashi/local`) | Individuals evaluating the workflow | Zero config — no server, no auth | Planned (#312) |
| MCP server (self-hosted) | Teams running their own infra | Docker Compose + API key | Shipped |
| **Cloud-hosted MCP** | Teams without infra | One API key | Planned (#313, #314) |
| Framework integrations | Agent builders | One import + decorator | Shipped |
| SDKs (Go, Python, TypeScript) | Programmatic integrators | Import, instantiate, call | Shipped |
| HTTP API | Everything else | Raw HTTP | Shipped |

### Local-lite

A self-contained binary published to npm as `@akashi/local`. Writes to SQLite at `~/.akashi/akashi.db`. No Docker, no Postgres, no Qdrant, no Ollama. Starts in under 3 seconds.

MCP config:
```json
{
  "mcpServers": {
    "akashi": {
      "command": "npx",
      "args": ["@akashi/local"]
    }
  }
}
```

No URL, no token, no auth. Same trust model as other local MCP servers.

**Included in local-lite:**
- All 6 MCP tools with identical signatures to the full server
- SQLite storage — decisions, events, assessments, basic conflicts
- Text search via SQLite FTS5

**Excluded from local-lite:**
- Semantic search (no embedding model)
- LLM conflict validation
- Conflict groups
- Multi-tenancy
- SSE / real-time notifications
- Audit dashboard

The tools are complete and useful without semantics. The workflow — check before deciding, trace after deciding — is identical; the intelligence is lower.

**Architecture constraints:**
- New `storage/sqlite` package implementing the same storage interface as the Postgres backend. SQLite schema is created on first run; no Atlas migrations.
- New `cmd/akashi-local` binary wired to the existing MCP server with stdio transport (no HTTP, no port).
- npm wrapper using `optionalDependencies` per OS/arch (same pattern as esbuild). Targets: `darwin-arm64`, `darwin-amd64`, `linux-arm64`, `linux-amd64`. Windows is out of scope for the initial release.
- `cmd/akashi-local` must not import Postgres, Qdrant, or Ollama packages. This is a hard dependency boundary enforced at compile time. Any capability added to the MCP tools must have a functional fallback for local-lite or must be gated behind feature detection.
- No changes to the MCP tool handlers — local-lite plugs in at the storage layer only.

**Upgrade path:** Users who need semantic search, conflict detection, or multi-agent sharing switch to the cloud tier (change `command` config to a URL + API key). Local-lite data does not migrate automatically; that is a follow-up.

### Cloud-hosted MCP

A hosted endpoint where the complete setup is one API key in the MCP config. No Docker, no database, no JWT renewal. Implementation is infrastructure work outside this repo; the OSS server changes required are:

1. `GET /mcp/info` (unauthenticated) — returns server version, transport, and supported auth schemes. **Shipped in PR #315.**
2. `POST /auth/signup` — creates org, agent, and managed `ak_`-format API key atomically. Returns a ready-to-paste MCP config snippet. Tracked in issue #314.

### `ApiKey` as the canonical MCP auth scheme

`ApiKey <agent_id>:<key>` is the correct credential for MCP config files. It does not expire and survives server restarts. `Bearer <jwt>` remains supported but is appropriate for programmatic use, not persistent config.

The README MCP section now leads with `ApiKey`. **Shipped in PR #315.** For self-hosted deployments, `ApiKey admin:<AKASHI_ADMIN_API_KEY>` works directly against the legacy key path — no managed key creation needed.

## Consequences

- ADR-004's "one line of config" claim is corrected: it is accurate for local-lite and cloud-hosted; self-hosted is "Docker Compose + API key."
- `cmd/akashi-local` is a new binary target in this repo. It must stay dependency-clean — no Postgres, Qdrant, or Ollama imports.
- The SQLite storage interface must stay in sync with the Postgres storage interface. Any new storage method needed by an MCP tool must be implemented in both backends, or the tool must degrade gracefully when the method is unavailable.
- Local-lite and self-hosted are the evaluation and team funnel respectively. Cloud is the conversion target.
- ADRs in this repo have their own numbering sequence starting from ADR-001. Next ADR: ADR-010.

## References

- ADR-004: MCP and framework integrations as primary distribution channels (amended by this ADR)
- Issue #312: local-lite mode (SQLite backend, `cmd/akashi-local`, npm packaging)
- Issue #313: cloud MCP endpoint — README + `GET /mcp/info` (complete)
- Issue #314: self-serve org signup with API key issuance
- PR #315: `ApiKey` auth docs and `GET /mcp/info` endpoint (merged)
