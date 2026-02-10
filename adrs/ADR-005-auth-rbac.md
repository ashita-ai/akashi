# ADR-005: Ed25519 JWT authentication with Argon2id API keys and tiered RBAC

**Status:** Accepted
**Date:** 2026-02-03

## Context

Akashi is a multi-tenant decision trace system where AI agents, human operators, and automated integrations all submit and query data. The authentication and authorization layer must satisfy several competing requirements:

1. **Machine-first callers.** The majority of Akashi clients are AI agent processes, not humans. Authentication must support long-lived programmatic credentials (API keys) alongside session-based tokens.
2. **Multi-tenancy isolation.** Every request must be scoped to an organization. A valid credential from org A must never read or write data belonging to org B.
3. **Granular visibility control.** Within a single org, agents should be able to grant other agents read access to their decision traces without requiring admin intervention. This is essential for multi-agent systems where a reviewer agent needs to inspect a coder agent's reasoning.
4. **Minimal operational burden.** Self-hosted deployments should work out of the box without a key management ceremony. Cloud deployments must use production-grade key material.

## Decision

### Authentication: dual-path credential validation

Akashi supports two credential types, both resolved to the same JWT claims format before reaching handler code:

**JWT tokens (session-based).** Issued via `POST /auth/token` after agent identity is verified. Tokens are signed with Ed25519 (EdDSA) using the `golang-jwt/jwt/v5` library. Claims carry `agent_id`, `org_id`, and `role`. Tokens have a configurable expiration (default: 24 hours). Each token has a unique `jti` (JWT ID) for revocation support.

**API keys (programmatic).** Agents may be provisioned with an API key. The raw key is shown once at creation time and never stored. The server persists only an Argon2id hash. On each request, the middleware hashes the presented key and compares via constant-time comparison (`crypto/subtle.ConstantTimeCompare`).

Both paths converge in `authMiddleware`: after successful validation, JWT claims are injected into the request context via `ctxutil.WithClaims`. All downstream code --- handlers, authorization checks, storage layer --- reads claims from context without knowing which credential type was used.

### Signing: Ed25519 (EdDSA)

JWT signing uses Ed25519 with PKCS#8 PEM key files. Key management has two modes:

- **Development mode.** When no key paths are configured, `NewJWTManager` calls `ed25519.GenerateKey(crypto/rand.Reader)` to produce an ephemeral key pair. Tokens are valid only for the lifetime of the process. This eliminates setup friction for local development and testing.
- **Production mode.** Operator provides `private_key_path` and `public_key_path` in configuration. Keys are loaded from PEM files at startup via `x509.ParsePKCS8PrivateKey` and `x509.ParsePKIXPublicKey`, with type assertions to ensure the keys are Ed25519. If parsing fails, the server refuses to start.

### API key hashing: Argon2id

API keys are hashed with Argon2id using the following parameters:

| Parameter | Value |
|-----------|-------|
| Time cost (iterations) | 1 |
| Memory cost | 64 MB |
| Parallelism (threads) | 4 |
| Output key length | 32 bytes |
| Salt length | 16 bytes (crypto/rand) |

The stored format is `base64(salt)$base64(hash)`. Verification recomputes the hash from the presented key and the stored salt, then compares with `subtle.ConstantTimeCompare`.

### Authorization: five-tier role hierarchy

Roles are ordered by privilege level:

| Role | Rank | Purpose |
|------|------|---------|
| `platform_admin` | 5 | Cross-org operations, billing management, system configuration |
| `org_owner` | 4 | Full control within their organization |
| `admin` | 3 | Manage agents, grants, and configuration within the org |
| `agent` | 2 | Submit traces, query own data, query granted data |
| `reader` | 1 | Read-only access to explicitly granted resources |

Ranks are sequential (1 through 5). Role comparison uses `RoleAtLeast(r, minRole)`, which compares numeric ranks via `RoleRank(r) >= RoleRank(minRole)`. Only relative ordering matters; if a new role is needed between existing ones, the ranks can be renumbered since they are internal to `RoleRank` and not persisted.

Route-level enforcement uses the `requireRole` middleware. For example, data export endpoints require `admin`, while agent registration requires `agent`. The middleware reads claims from context and returns 403 if the caller's role is below the required minimum.

### Authorization: fine-grained access grants

Below the role hierarchy, Akashi supports per-agent access grants for cross-agent visibility. An access grant specifies:

| Field | Type | Description |
|-------|------|-------------|
| `grantor_id` | UUID | Agent that created the grant |
| `grantee_id` | UUID | Agent receiving access |
| `org_id` | UUID | Organization scope (grants never cross org boundaries) |
| `resource_type` | enum | `agent_traces` (only value currently implemented) |
| `resource_id` | string (nullable) | Specific resource, or NULL for all resources of the type |
| `permission` | enum | `read` (only value currently implemented) |
| `expires_at` | timestamp (nullable) | Optional expiry; NULL means no expiration |

Access checks follow a short-circuit evaluation in `canAccessAgent`:

1. If caller's role is `admin` or above: **allow** (admins see everything within their org).
2. If caller is an `agent` accessing their own data (`claims.AgentID == targetAgentID`): **allow**.
3. Otherwise: query `access_grants` for a valid, non-expired grant matching the org, grantee, resource type, and permission. A grant with `resource_id IS NULL` matches all resources of that type (wildcard grant).

Read-path filtering applies these rules to query results. `filterDecisionsByAccess` and `filterSearchResultsByAccess` apply per-agent access checks with an in-memory cache to avoid repeated database queries within a single request. `filterConflictsByAccess` requires the caller to have access to both agents involved in a conflict before the conflict is visible.

## Rationale

### Why Ed25519 over RS256

- **Performance.** Ed25519 signing is approximately 20x faster than RSA-2048. For an infrastructure service that issues tokens on every agent startup, this matters.
- **Key size.** Ed25519 keys are 32 bytes (private) + 32 bytes (public). RSA-2048 keys are 2048 bits. Smaller keys mean smaller JWTs, which reduces per-request overhead on every API call.
- **Security margin.** Ed25519 provides 128-bit security. RSA-2048 provides approximately 112-bit security. Ed25519 is also immune to padding oracle attacks that have historically affected RSA.
- **Deterministic signatures.** Ed25519 does not require a random nonce per signature, eliminating an entire class of implementation bugs (cf. Sony PS3 ECDSA nonce reuse).

### Why Argon2id over bcrypt

- **Memory-hard.** Argon2id's 64 MB memory requirement makes GPU-based brute force attacks significantly more expensive than bcrypt, which is compute-hard but memory-cheap.
- **Hybrid resistance.** Argon2id combines Argon2i (side-channel resistant) and Argon2d (GPU resistant), providing defense against both attack vectors.
- **Configurable parameters.** Time, memory, and parallelism are independently tunable. The current parameters (1 iteration, 64 MB, 4 threads) are calibrated for API key verification latency under 100ms on commodity hardware.
- **OWASP recommendation.** Argon2id is the primary recommendation in the OWASP Password Storage Cheat Sheet for new applications.

### Why a five-tier hierarchy instead of flat RBAC

Akashi serves three distinct personas with different trust boundaries:

- **Platform operators** (`platform_admin`) need cross-org visibility for billing, support, and system health. Collapsing this into `admin` would leak cross-org access to organization-level administrators.
- **Organization owners** (`org_owner`) manage their team's agents and configuration but must not see other organizations' data. Separating this from `admin` allows delegation of day-to-day management without transferring ownership.
- **Agents and readers** need fine-grained, grant-based access rather than blanket read/write. A flat admin/user split would force a choice between over-permissioning agents (admin) and under-permissioning them (requiring explicit grants for their own data).

### Why access grants are stored in the database, not encoded in JWTs

- **Revocability.** Grants can be revoked instantly by deleting the row. JWT-encoded grants would remain valid until the token expires.
- **Cardinality.** An agent may have grants from dozens of other agents across multiple resource types. Encoding all grants in the JWT would inflate token size.
- **Auditability.** The `access_grants` table provides a queryable log of who granted what to whom and when, with `granted_at` timestamps.

## Consequences

- Every API request (except `/health`, `/auth/token`, `/auth/signup`, `/auth/verify`, `/billing/webhooks`) requires a valid Bearer token in the Authorization header.
- Operators must generate and securely distribute Ed25519 PEM key files for production deployments. Development mode auto-generates ephemeral keys.
- API key verification incurs Argon2id computation cost (~50-100ms) on every request authenticated via API key. This is acceptable for programmatic access patterns but would be prohibitive for high-frequency polling; such callers should use JWT tokens instead.
- Access grants are checked per-agent, not per-decision. This means granting `agent_traces` read access to an agent grants access to all of that agent's decisions, not individual ones. Per-decision grants are supported by the `resource_id` field but are not yet used by the authorization middleware.
- The `platform_admin` role operates outside org boundaries. Any bug in org-scoping logic that fails to check role rank could inadvertently leak data across organizations.
- Adding new roles requires updating the `RoleRank` function and any `requireRole` middleware invocations. Ranks are 1 through 5; inserting a new role between existing ones requires renumbering, but ranks are internal to `RoleRank` and not persisted, so this is safe.

## References

- ADR-004: MCP and framework integrations (MCP server shares the same auth middleware chain)
- Implementation: `internal/auth/auth.go` (JWT signing/validation, ephemeral keys), `internal/server/middleware.go` (authMiddleware, requireRole), `internal/authz/authz.go` (access grant checks, filtering), `internal/model/agent.go` (role hierarchy, RoleRank)
- OWASP Password Storage Cheat Sheet: cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
