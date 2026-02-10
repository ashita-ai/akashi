# Security Policy

## Reporting Vulnerabilities

If you discover a security vulnerability in Akashi, please report it responsibly.

**Email:** security@ashita.ai

Do not file public GitHub issues for security vulnerabilities. Include a description of the vulnerability, steps to reproduce, and the potential impact. We will acknowledge receipt within 48 hours and provide an initial assessment within 5 business days.

We request that you:

- Allow reasonable time for a fix before public disclosure.
- Avoid accessing or modifying data belonging to other users during testing.
- Test only against your own Akashi instance (local or self-hosted).

## Security Architecture

### Authentication

**JWT tokens** use Ed25519 (EdDSA) signing via `crypto/ed25519`. Tokens are not signed with RSA or HMAC -- only the EdDSA algorithm is accepted during validation. Tokens include issuer validation (`"akashi"`) and UUID-format subject validation. Token expiration is enforced at parse time. Key pairs are loaded from PEM-encoded PKCS#8 files; ephemeral keys are generated only in development when no key files are configured.

**API keys** are hashed with Argon2id (64 MB memory cost, 1 iteration, 4 threads, 32-byte key length, 16-byte random salt). Verification uses `crypto/subtle.ConstantTimeCompare` to prevent timing side-channels. A dummy Argon2id computation runs on authentication failure paths where no real hash was checked, preventing timing-based enumeration of valid agent IDs. API keys are never logged, never returned after creation, and stored only as hashes.

**Human user passwords** (used in the self-serve signup flow) are hashed with the same Argon2id parameters. Passwords must be at least 12 characters with uppercase, lowercase, and digit characters. A blocklist of common passwords is enforced. Passwords with excessive single-character repetition (any character exceeding 50% of the total length) are rejected.

### Authorization

Akashi implements a 5-tier role hierarchy:

| Role | Rank | Capabilities |
|------|------|-------------|
| `platform_admin` | 5 | Full system access across all organizations |
| `org_owner` | 4 | Full access within their organization |
| `admin` | 3 | Manage agents, grants, and all data within their org |
| `agent` | 2 | Read/write own data; read others only with explicit grant |
| `reader` | 1 | Read-only; requires explicit grant for all data access |

Role checks use `RoleAtLeast()` comparison against the hierarchy. Every API endpoint that reads decision data applies access filtering at the data level through the `authz` package, not just at the route level. The filtering logic loads the caller's granted agent IDs in a single query, then filters results in memory. Admin+ roles bypass grant checks (unrestricted access within their org). Agents can read their own data without a grant. Readers always require explicit grants.

Fine-grained access grants support:
- Scoping by resource type (`agent_traces`, `decision`, `run`) and optional resource ID.
- Time-bounded expiration (`expires_at`).
- Organization-scoped isolation (every grant query includes `org_id`).

### Transport Security

- **HSTS**: `Strict-Transport-Security: max-age=63072000; includeSubDomains` on all responses.
- **TLS**: Required for production deployments. The OTEL exporter uses TLS by default.
- **SMTP**: STARTTLS is enforced before sending credentials. The server refuses to authenticate over an unencrypted connection.

### Request Validation

- All JSON request bodies pass through `http.MaxBytesReader` (1 MB default limit).
- JSON decoders use `DisallowUnknownFields()` to reject unexpected fields.
- Agent IDs are validated against a strict ASCII pattern: 1-255 characters, alphanumeric plus `.`, `-`, `_`, `@`. Validation runs byte-by-byte; no regex.

### Security Headers

All responses include:

| Header | Value |
|--------|-------|
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Cache-Control` | `no-store` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Content-Security-Policy` | `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'` |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=(), payment=()` |

### Rate Limiting

Rate limiting uses a Redis-backed sliding window algorithm implemented as an atomic Lua script. Each request is scored by timestamp in a sorted set; expired entries are pruned before counting.

| Category | Prefix | Limit | Window | Key |
|----------|--------|-------|--------|-----|
| Auth endpoints | `auth` | 20 | 1 min | Client IP (`RemoteAddr`) |
| Ingest (writes) | `ingest` | 300 | 1 min | Agent ID |
| Query (reads) | `query` | 300 | 1 min | Agent ID |
| Search | `search` | 100 | 1 min | Agent ID |

Admin+ roles (`admin`, `org_owner`, `platform_admin`) are exempt from agent-keyed rate limits. Auth endpoint rate limits apply to all clients by IP. Rate limit headers (`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`) are included on all rate-limited responses. If Redis is unavailable, requests are allowed through (fail-open) with a warning logged.

The IP key function uses `RemoteAddr` only. `X-Forwarded-For` is not trusted because the server may not be behind a proxy that sanitizes it. Deployments behind a trusted reverse proxy should configure the proxy to set `RemoteAddr` directly.

### Panic Recovery

A recovery middleware wraps all HTTP handlers. If a handler panics, the stack trace is logged server-side and the client receives a generic 500 response. The server process is not terminated.

## Multi-Tenancy Isolation

All data is scoped by `org_id`. Every database query includes an `org_id` WHERE clause. SSE event subscriptions are org-scoped. The storage layer enforces org boundaries independently of the handler layer (defense-in-depth).

PostgreSQL row-level security (RLS) policies exist on tenant-scoped tables as an additional layer. Application-level WHERE clauses are the primary enforcement mechanism.

Access grants are org-scoped: a grant in one organization cannot authorize access to data in another organization. The `access_grants` table includes `org_id` in all queries, and grant lookups enforce both `org_id` and `grantee_id`.

## Data Protection

- **Append-only event model**: Decision data uses a bi-temporal model (`valid_from`/`valid_to` plus `transaction_time`). Records are soft-deleted by setting `valid_to`; the underlying data is retained for audit purposes.
- **Event log immutability**: The event log does not support UPDATE or DELETE in normal application operation.
- **Credential management**: Database credentials, API keys, and SMTP credentials are provided via environment variables. No credentials are stored in code. `.env` files are gitignored.
- **API key confidentiality**: API keys are returned exactly once at creation time. They are stored only as Argon2id hashes. They are excluded from JSON serialization (`json:"-"` tag on `APIKeyHash`). They are not included in log output.
- **Request IDs**: Every request is assigned a UUID for correlation across logs. Client-provided `X-Request-ID` headers are accepted; otherwise a server-generated UUID is used.

## Threat Model

### In Scope

- API authentication bypass (forged JWTs, stolen API keys, timing attacks on auth).
- Authorization escalation (role manipulation, grant bypass, cross-agent data access without grants).
- Cross-tenant data access (org_id filter bypass, SSE subscription leaking events from other orgs).
- Injection attacks (SQL injection via query parameters, JSON payload manipulation).
- Denial of service (rate limit bypass, request body amplification, resource exhaustion).

### Out of Scope

- Physical access to the host machine.
- Operating system or kernel-level compromise.
- Supply chain attacks on Go module dependencies (mitigated by `govulncheck` in CI; see Dependencies).
- Compromise of the PostgreSQL or Redis instances themselves (the application assumes these are access-restricted).
- Social engineering of organization administrators.

### Assumptions

- TLS termination occurs at a reverse proxy in front of the Akashi server.
- PostgreSQL is not directly accessible from untrusted networks.
- Redis is not directly accessible from untrusted networks.
- Ed25519 key files are stored with appropriate filesystem permissions (readable only by the application user).
- The deployment environment provides adequate entropy for `crypto/rand`.

## Dependencies

Akashi maintains a minimal dependency surface:

- **HTTP server**: Go standard library `net/http`. No third-party HTTP framework.
- **Cryptography**: `crypto/ed25519` (JWT signing), `golang.org/x/crypto/argon2` (API key and password hashing), `crypto/subtle` (constant-time comparison).
- **JWT**: `github.com/golang-jwt/jwt/v5`.
- **PostgreSQL**: `github.com/jackc/pgx/v5`. No ORM. All queries are parameterized.
- **Redis**: `github.com/redis/go-redis/v9` (rate limiting only).
- **Observability**: `go.opentelemetry.io/otel` (tracing and metrics).
- **Vulnerability scanning**: `govulncheck` runs in CI on every pull request and in the local `make ci` pipeline.

## Supported Versions

Security fixes are applied to the latest release only. There is no long-term support for older versions. Users should run the most recent version.
