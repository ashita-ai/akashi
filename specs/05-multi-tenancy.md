# Spec 05: Multi-Tenancy & SaaS Billing

**Status**: Draft
**Author**: Elenchus session `session-mlab6cno-vFYaG6cLy8PG`
**Date**: 2026-02-06

## Problem

Akashi has zero tenant isolation. All agents, decisions, events, and access grants exist in a flat global namespace. The `admin` role sees all data. This makes it impossible to serve multiple independent customers from a single deployment.

## Decisions (from interrogation)

| Question | Decision |
|----------|----------|
| How does org_id reach the server? | **JWT claims.** Embedded at token issuance. SDKs unchanged. |
| Org creation flow? | **Self-serve signup** (Free/Pro) + **platform admin** (Enterprise). |
| Role model? | **4 roles**: org_owner, admin, agent, reader. All org-scoped. |
| Billing scope? | **Full stack**: Stripe Checkout, webhooks, subscription tiers, metering. |
| Tiers? | Free (1K decisions/mo, 1 agent), Pro ($149/mo, 50K decisions/mo, unlimited agents), Enterprise (custom). |
| Migration? | Pre-launch system. Schema correctness, not data volume. Default org for existing data. |
| SDK compatibility? | All existing SDK test suites must pass without modification. Additive response fields OK. |

## Scope

### In Scope

1. **Organizations table + management**
2. **org_id column** on all tenant-scoped tables
3. **PostgreSQL Row-Level Security** policies as defense-in-depth
4. **4-role RBAC** (org_owner, admin, agent, reader) replacing current 3-role model
5. **JWT claims extension** with `org_id`
6. **Self-serve signup** with email verification
7. **Stripe integration** (Checkout, webhooks, subscription management)
8. **Per-org usage metering** (decision counter)
9. **Quota enforcement** (reject writes when limit exceeded)
10. **MCP authorization fix** (CRITICAL security finding F-3/F-13)
11. **SSE org-scoped filtering**
12. **statusWriter Flusher fix** (SSE broken)
13. **Handler error logging** (errors currently discarded at boundary)
14. **QueryDecisions valid_to IS NULL default filter**

### Out of Scope

- UI / dashboard (separate effort)
- Custom domains
- Kubernetes/Terraform IaC
- Data retention policies per org
- Multi-region deployment
- SSO / SAML

---

## 1. Database Schema

### New Tables

#### `organizations`
```sql
CREATE TABLE organizations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    slug            TEXT NOT NULL UNIQUE,  -- URL-safe identifier
    plan            TEXT NOT NULL DEFAULT 'free' CHECK (plan IN ('free', 'pro', 'enterprise')),
    stripe_customer_id   TEXT UNIQUE,
    stripe_subscription_id TEXT UNIQUE,
    decision_limit  INTEGER NOT NULL DEFAULT 1000,  -- per month
    agent_limit     INTEGER NOT NULL DEFAULT 1,
    email           TEXT NOT NULL,
    email_verified  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_organizations_slug ON organizations (slug);
CREATE INDEX idx_organizations_stripe_customer ON organizations (stripe_customer_id) WHERE stripe_customer_id IS NOT NULL;
```

#### `org_usage`
```sql
CREATE TABLE org_usage (
    org_id          UUID NOT NULL REFERENCES organizations(id),
    period          TEXT NOT NULL,  -- 'YYYY-MM' format
    decision_count  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, period)
);
```

#### `email_verifications`
```sql
CREATE TABLE email_verifications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    token       TEXT NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Modified Tables

Add `org_id UUID NOT NULL REFERENCES organizations(id)` to:

| Table | Index | Notes |
|-------|-------|-------|
| `agents` | `(org_id, agent_id)` UNIQUE | agent_id unique within org, not globally |
| `agent_runs` | `(org_id, id)` | |
| `agent_events` | `(org_id)` | TimescaleDB hypertable -- FK not supported, enforce in app |
| `decisions` | `(org_id, agent_id, valid_from DESC) WHERE valid_to IS NULL` | Partial index for current decisions |
| `alternatives` | inherited via decision FK | No direct org_id needed -- joined through decision |
| `evidence` | inherited via decision FK | No direct org_id needed -- joined through decision |
| `access_grants` | `(org_id, grantee_id)` | |

**Key**: `alternatives` and `evidence` do NOT get org_id directly. They are always accessed through their parent `decision`, which has org_id. This avoids redundant columns and keeps COPY operations simpler.

### Row-Level Security

```sql
-- Enable RLS on tenant-scoped tables
ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_grants ENABLE ROW LEVEL SECURITY;

-- Policy: application sets current_setting('app.org_id') per transaction
CREATE POLICY org_isolation ON agents
    USING (org_id = current_setting('app.org_id')::uuid);
-- (repeat for other tables)
```

The application sets `SET LOCAL app.org_id = '<uuid>'` at the start of each transaction/query. RLS is defense-in-depth -- the application already filters by org_id in WHERE clauses, but RLS prevents bugs from leaking data.

**Note**: RLS does NOT apply to the connection pool owner (typically `akashi` user). The app must use `SET ROLE` or a restricted role for RLS to be effective. This is a deployment concern documented in the migration.

### Migration Strategy

Migration 014:
1. Create `organizations` table
2. Insert default org: `INSERT INTO organizations (id, name, slug, plan, email, email_verified, decision_limit, agent_limit) VALUES ('00000000-0000-0000-0000-000000000000', 'Default', 'default', 'enterprise', 'admin@localhost', true, 2147483647, 2147483647)`
3. Add `org_id` column to each table with `DEFAULT '00000000-...'`, then `ALTER COLUMN org_id DROP DEFAULT`
4. Create new indexes
5. Create `org_usage` and `email_verifications` tables
6. Enable RLS policies
7. Drop old unique constraint on `agents.agent_id`, add new `(org_id, agent_id)` unique constraint

---

## 2. Role Model

### Roles

| Role | Scope | Permissions |
|------|-------|-------------|
| `platform_admin` | Global (internal only) | Create/delete orgs, impersonate, view all data. NOT exposed via API. CLI/internal only. |
| `org_owner` | Single org | Billing, member management, API key management, all admin + agent + reader permissions |
| `admin` | Single org | Create/delete agents, manage access grants, all agent + reader permissions |
| `agent` | Single org | Write traces, append events, create/complete runs |
| `reader` | Single org | Read decisions, query, search, check |

### Role Hierarchy

`org_owner` > `admin` > `agent` > `reader`

Each higher role inherits all permissions of lower roles.

### Model Change

```go
// internal/model/agent.go
type AgentRole string

const (
    RolePlatformAdmin AgentRole = "platform_admin"  // new, internal only
    RoleOrgOwner      AgentRole = "org_owner"        // new
    RoleAdmin         AgentRole = "admin"
    RoleAgent         AgentRole = "agent"
    RoleReader        AgentRole = "reader"
)
```

Database CHECK constraint updated in migration.

---

## 3. Authentication Changes

### JWT Claims

```go
type Claims struct {
    jwt.RegisteredClaims
    AgentID string    `json:"agent_id"`
    OrgID   uuid.UUID `json:"org_id"`    // NEW
    Role    AgentRole `json:"role"`
}
```

`org_id` is populated at token issuance by looking up the agent's org membership. The agent table already links agent -> org via the new `org_id` column.

### Token Issuance (`POST /auth/token`)

Current flow: `agent_id` + `api_key` -> verify -> issue JWT with `agent_id` + `role`.

New flow: `agent_id` + `api_key` -> verify -> look up agent's `org_id` -> issue JWT with `agent_id` + `org_id` + `role`.

**SDK impact**: None. SDKs send the same `agent_id` + `api_key`. The JWT they receive back has an additional `org_id` claim they don't need to parse.

### Auth Middleware

`ClaimsFromContext` now returns claims with `OrgID`. Every handler/service method receives org context via claims.

New middleware: `orgContextMiddleware` extracts `org_id` from claims and makes it available as `OrgIDFromContext(ctx)` for use in storage queries and RLS setup.

---

## 4. Storage Layer Changes

### Every Query Gets org_id

All storage methods gain an `orgID uuid.UUID` parameter (or receive it from context). Example:

```go
// Before
func (db *DB) QueryDecisions(ctx context.Context, req model.QueryRequest) ([]model.Decision, int, error)

// After
func (db *DB) QueryDecisions(ctx context.Context, orgID uuid.UUID, req model.QueryRequest) ([]model.Decision, int, error)
```

The `buildDecisionWhereClause` function prepends `org_id = $1` to every WHERE clause.

### COPY Operations

`InsertEvents` (COPY-based) adds `org_id` as the first column in the COPY column list. The `Buffer.Append` method receives org_id from the handler context.

`CreateTraceTx` passes org_id through to all INSERT/COPY statements within the transaction.

### Service Layer

`decisions.Service` methods gain org_id:

```go
func (s *Service) Trace(ctx context.Context, orgID uuid.UUID, input TraceInput) (TraceResult, error)
func (s *Service) Check(ctx context.Context, orgID uuid.UUID, ...) (model.CheckResponse, error)
func (s *Service) Search(ctx context.Context, orgID uuid.UUID, ...) ([]model.SearchResult, error)
func (s *Service) Query(ctx context.Context, orgID uuid.UUID, ...) ([]model.Decision, int, error)
func (s *Service) Recent(ctx context.Context, orgID uuid.UUID, ...) ([]model.Decision, int, error)
```

### MCP Authorization Fix (F-3/F-13)

MCP handlers currently bypass all authorization. Fix:

1. Extract claims from MCP request context (the HTTP auth middleware already runs before the MCP handler)
2. Pass `claims.OrgID` to all service calls
3. In `handleTrace`: verify `claims.Role` allows writes AND `claims.AgentID == requestedAgentID || claims.Role >= admin`
4. In read handlers: org_id scoping via service layer automatically limits results

This closes the CRITICAL security finding.

---

## 5. Signup & Onboarding

### Flow

1. `POST /auth/signup` -- body: `{ email, password, org_name }`
   - Validate email format, password strength
   - Create org (plan=free, email_verified=false)
   - Create org_owner agent with hashed password
   - Generate email verification token
   - Send verification email (via configured SMTP or provider)
   - Return `{ org_id, agent_id, message: "check email" }`

2. `GET /auth/verify?token=<token>`
   - Look up token in `email_verifications`
   - Set `organizations.email_verified = true`
   - Mark token as used
   - Redirect to success page or return JSON

3. `POST /auth/token` -- existing flow, now works with verified email check
   - If org not verified, return 403 "email not verified"

### API Key Generation

After verification, the org_owner can generate API keys for agents via:
- `POST /v1/agents` (existing, now org-scoped)

---

## 6. Stripe Integration

### New Package: `internal/billing`

```
internal/billing/
    billing.go      -- Stripe client wrapper, plan definitions
    webhooks.go     -- Webhook handler
    metering.go     -- Usage counter, quota checks
```

### Plan Definitions

```go
var Plans = map[string]Plan{
    "free": {
        Name:          "Free",
        PriceID:       "",  // no Stripe price for free
        DecisionLimit: 1_000,
        AgentLimit:    1,
    },
    "pro": {
        Name:          "Pro",
        PriceID:       os.Getenv("STRIPE_PRO_PRICE_ID"),
        DecisionLimit: 50_000,
        AgentLimit:    0,  // unlimited
    },
    "enterprise": {
        Name:          "Enterprise",
        PriceID:       "",  // custom
        DecisionLimit: 0,   // custom, set per org
        AgentLimit:    0,   // custom
    },
}
```

### Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | /billing/checkout | org_owner | Create Stripe Checkout session for plan upgrade |
| POST | /billing/portal | org_owner | Create Stripe billing portal session |
| POST | /billing/webhooks | none (Stripe signature) | Handle Stripe events |
| GET | /v1/usage | org_owner, admin | Current period usage stats |

### Webhook Events

| Event | Action |
|-------|--------|
| `checkout.session.completed` | Set org plan, store stripe_customer_id + subscription_id |
| `customer.subscription.updated` | Update org plan + limits |
| `customer.subscription.deleted` | Downgrade org to free |
| `invoice.payment_failed` | Log warning, send notification (future: grace period) |

### Metering

On every `POST /v1/trace` and MCP `akashi_trace`:
1. `SELECT decision_count FROM org_usage WHERE org_id = $1 AND period = $2`
2. If `decision_count >= org.decision_limit`, return 429 "quota exceeded"
3. After successful trace: `INSERT INTO org_usage ... ON CONFLICT DO UPDATE SET decision_count = decision_count + 1`

The counter increment is atomic via `ON CONFLICT ... UPDATE`. No race conditions.

Agent limit enforcement: on `POST /v1/agents`:
1. `SELECT COUNT(*) FROM agents WHERE org_id = $1`
2. If `count >= org.agent_limit` (and limit > 0), return 429 "agent limit exceeded"

### Config

```
STRIPE_SECRET_KEY          -- Stripe API key
STRIPE_WEBHOOK_SECRET      -- Webhook signing secret
STRIPE_PRO_PRICE_ID        -- Price ID for Pro plan
AKASHI_SMTP_HOST           -- For email verification
AKASHI_SMTP_PORT
AKASHI_SMTP_USER
AKASHI_SMTP_PASSWORD
AKASHI_SMTP_FROM
```

---

## 7. SSE Org-Scoped Filtering

### Fix 1: statusWriter Flusher

```go
// internal/server/middleware.go
type statusWriter struct {
    http.ResponseWriter
    status int
    size   int
}

// Implement Flusher by delegating to the underlying writer
func (w *statusWriter) Flush() {
    if f, ok := w.ResponseWriter.(http.Flusher); ok {
        f.Flush()
    }
}
```

### Fix 2: Org-Scoped Notifications

The NOTIFY payload already includes `agent_id`. After multi-tenancy, it also includes `org_id`:

```go
notifyPayload := map[string]any{
    "decision_id": decision.ID,
    "agent_id":    input.AgentID,
    "org_id":      orgID,  // NEW
    "outcome":     input.Decision.Outcome,
}
```

The SSE handler filters: only send events where `payload.org_id == subscriber.claims.OrgID`.

---

## 8. Handler Error Logging

Every handler that calls a service method must log the error before returning a generic 500:

```go
// Before (error discarded)
if err != nil {
    writeError(w, r, 500, model.ErrCodeInternalError, "failed to create trace")
    return
}

// After (error logged)
if err != nil {
    h.logger.Error("trace failed", "error", err, "agent_id", claims.AgentID, "org_id", claims.OrgID)
    writeError(w, r, 500, model.ErrCodeInternalError, "failed to create trace")
    return
}
```

---

## 9. QueryDecisions valid_to IS NULL Fix

`buildDecisionWhereClause` must prepend `valid_to IS NULL` for non-temporal queries:

```go
func buildDecisionWhereClause(orgID uuid.UUID, filters model.QueryFilters, temporal bool) (string, []any) {
    args := []any{orgID}
    conditions := []string{"org_id = $1"}
    if !temporal {
        conditions = append(conditions, "valid_to IS NULL")
    }
    // ... rest of existing logic
}
```

---

## 10. Rate Limiting Changes

Current rate limiting is per-agent-id. After multi-tenancy:

- **API rate limits** remain per-agent (prevent one noisy agent from starving others within the same org)
- **Quota limits** are per-org per-month (decision count, agent count)
- **Auth rate limit** remains per-IP

No changes to the Redis sliding window implementation. The quota system is a separate check in the billing/metering layer.

---

## Success Criteria

1. All existing SDK test suites (Go 5, Python 10, TypeScript 23) pass without modification
2. All existing server integration tests pass (with migration to default org)
3. New tests cover:
   - Cross-org data isolation (org A cannot see org B's decisions)
   - Role hierarchy enforcement (reader cannot write, agent cannot manage)
   - MCP authorization (reader cannot trace, agent cannot access other org)
   - Quota enforcement (free tier rejects after 1000 decisions)
   - Stripe webhook processing (plan upgrade/downgrade)
   - SSE filtering (subscriber only receives own org's events)
   - Email verification flow
4. RLS policies prevent data leakage even if application code has a bug
5. `go test -race ./...` passes

---

## File Changes Summary

| File / Package | Change Type | Description |
|---------------|-------------|-------------|
| `migrations/014_multi_tenancy.sql` | New | Schema changes, RLS, default org |
| `internal/model/agent.go` | Modify | Add OrgID field, new roles |
| `internal/model/api.go` | Modify | Signup request/response types |
| `internal/auth/auth.go` | Modify | Add org_id to JWT claims |
| `internal/config/config.go` | Modify | Stripe + SMTP config |
| `internal/storage/*.go` | Modify | Add orgID parameter to all queries |
| `internal/service/decisions/service.go` | Modify | Add orgID parameter to all methods |
| `internal/service/trace/buffer.go` | Modify | Pass orgID through buffer |
| `internal/server/middleware.go` | Modify | statusWriter Flusher, org context middleware |
| `internal/server/handlers*.go` | Modify | Extract orgID from claims, log errors |
| `internal/server/authz.go` | Modify | Org-scoped authorization |
| `internal/server/server.go` | Modify | New routes (signup, verify, billing) |
| `internal/mcp/tools.go` | Modify | Extract claims, enforce org boundaries |
| `internal/mcp/resources.go` | Modify | Org-scoped resource handlers |
| `internal/billing/` | New | Stripe integration, metering, webhooks |
| `internal/signup/` | New | Signup flow, email verification |
| `go.mod` | Modify | Add `github.com/stripe/stripe-go/v82` |

---

## Implementation Order

1. **Migration 014** -- schema first, everything depends on it
2. **Model + auth changes** -- org_id in types and JWT
3. **Storage layer** -- org_id in all queries (biggest change by line count)
4. **Service layer** -- pass-through org_id
5. **Handler fixes** -- error logging, valid_to filter, statusWriter Flusher
6. **MCP authorization** -- close CRITICAL security finding
7. **SSE filtering** -- org-scoped notifications
8. **Signup flow** -- new endpoints, email verification
9. **Billing** -- Stripe integration, metering, quota enforcement
10. **Tests** -- cross-org isolation, role enforcement, quota, webhooks
