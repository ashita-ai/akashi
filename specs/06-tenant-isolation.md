# Tenant Isolation Specification

**Status:** Draft
**Author:** Akashi team
**Last Updated:** 2026-02-07

## 1. Overview

Akashi supports three tiers of tenant isolation, deployable simultaneously:

| Tier | Isolation | Who | GDPR Deletion |
|------|-----------|-----|---------------|
| **Standard** | Schema-per-tenant in shared database | Free & Pro plans | `DROP SCHEMA CASCADE` |
| **Enterprise** | Dedicated database (TigerData service) | Enterprise plan | Drop the database |
| **Intra-tenant** | Row-level security + RBAC within each schema/database | All tiers | Per-agent fine-grained access |

Every tenant gets at minimum a dedicated PostgreSQL schema. Enterprise tenants optionally get a dedicated database. Row-level security is active within every schema/database for intra-tenant agent-to-agent authorization.

### Architecture Diagram

```
                         ┌─────────────────────────────────────────┐
                         │           Control Database              │
                         │  (shared TigerData service)             │
                         │                                         │
                         │  public schema:                         │
                         │    organizations   (routing table)      │
                         │    email_verifications                  │
                         │    org_usage                            │
                         │                                         │
                         │  tenant_default schema:                 │
                         │    agents, decisions, agent_events...   │
                         │                                         │
                         │  tenant_acme schema:                    │
                         │    agents, decisions, agent_events...   │
                         │                                         │
                         │  tenant_initech schema:                 │
                         │    agents, decisions, agent_events...   │
                         └─────────────────────────────────────────┘

                         ┌─────────────────────────────────────────┐
                         │     Enterprise Database (BigCorp)       │
                         │  (dedicated TigerData service)          │
                         │                                         │
                         │  public schema:                         │
                         │    agents, decisions, agent_events...   │
                         └─────────────────────────────────────────┘
```

---

## 2. Data Model Changes

### 2.1 Organization Table Changes

Add isolation routing columns to the `organizations` table.

**Migration: `016_tenant_isolation.sql`**

```sql
-- Add isolation tier and routing columns.
ALTER TABLE organizations
    ADD COLUMN isolation_tier TEXT NOT NULL DEFAULT 'schema'
        CHECK (isolation_tier IN ('schema', 'database')),
    ADD COLUMN schema_name TEXT,
    ADD COLUMN database_url TEXT,
    ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';

-- Backfill the default org.
UPDATE organizations
SET schema_name = 'tenant_default',
    isolation_tier = 'schema'
WHERE id = '00000000-0000-0000-0000-000000000000';

-- Every org must have a schema_name. database_url is only for 'database' tier.
ALTER TABLE organizations
    ADD CONSTRAINT org_schema_name_required
        CHECK (schema_name IS NOT NULL AND schema_name != '');

ALTER TABLE organizations
    ADD CONSTRAINT org_database_url_check
        CHECK (
            (isolation_tier = 'database' AND database_url IS NOT NULL)
            OR
            (isolation_tier = 'schema' AND database_url IS NULL)
        );

CREATE UNIQUE INDEX idx_organizations_schema_name
    ON organizations (schema_name);
```

### 2.2 Agent Tags

Add a `tags` column to the agents table for tag-based access grouping.

**In tenant schema template (and migration for existing schemas):**

```sql
ALTER TABLE agents
    ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';

CREATE INDEX idx_agents_tags ON agents USING GIN (tags);
```

### 2.3 Tag-Based Access Grants

Extend the access_grants table to support tag-based grants. A grant with `grantee_tag` instead of `grantee_id` applies to all agents carrying that tag.

```sql
ALTER TABLE access_grants
    ALTER COLUMN grantee_id DROP NOT NULL;

ALTER TABLE access_grants
    ADD COLUMN grantee_tag TEXT;

ALTER TABLE access_grants
    ADD CONSTRAINT grant_target_check
        CHECK (
            (grantee_id IS NOT NULL AND grantee_tag IS NULL)
            OR
            (grantee_id IS NULL AND grantee_tag IS NOT NULL)
        );

CREATE INDEX idx_access_grants_tag
    ON access_grants (org_id, grantee_tag)
    WHERE grantee_tag IS NOT NULL;
```

**Semantics:** When evaluating `HasAccess`, check both direct grants (`grantee_id = agent.id`) and tag grants (`grantee_tag = ANY(agent.tags)`).

---

## 3. Tenant Schema Template

A tenant schema contains all tenant-scoped tables. The template is a single SQL file applied when provisioning a new tenant.

**File: `internal/tenant/template.sql`**

This file contains the DDL for all tenant-scoped tables, identical to what currently exists in the public schema but without `org_id` in WHERE clauses (the schema IS the isolation boundary). The `org_id` column is retained for defense-in-depth RLS.

### Tables in tenant schema:

| Table | Notes |
|-------|-------|
| `agents` | Includes `tags TEXT[]` column + GIN index |
| `agent_runs` | |
| `agent_events` | TimescaleDB hypertable, compression enabled |
| `decisions` | HNSW index on `embedding` column |
| `alternatives` | |
| `evidence` | HNSW index on `embedding` column |
| `access_grants` | Includes `grantee_tag` column |
| `decision_conflicts` | Materialized view |
| `agent_current_state` | Materialized view |
| `current_decisions` | View |

### Tables that stay in `public` (control plane):

| Table | Notes |
|-------|-------|
| `organizations` | Routing table, cross-tenant |
| `org_usage` | Billing, cross-tenant |
| `email_verifications` | Signup flow, cross-tenant |

### Template SQL structure:

```sql
-- Applied within: CREATE SCHEMA tenant_{slug}
-- All CREATE statements are unqualified (use search_path).

CREATE TABLE agents (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id    TEXT NOT NULL,
    org_id      UUID NOT NULL,
    name        TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('platform_admin','org_owner','admin','agent','reader')),
    api_key_hash TEXT,
    metadata    JSONB DEFAULT '{}',
    tags        TEXT[] NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, agent_id)
);
CREATE INDEX idx_agents_tags ON agents USING GIN (tags);

CREATE TABLE agent_runs ( ... );
-- [full DDL for each table, matching current schema]

-- TimescaleDB hypertable for agent_events
SELECT create_hypertable('agent_events', 'occurred_at', if_not_exists => TRUE);

-- Compression policy
ALTER TABLE agent_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'org_id,agent_id,run_id',
    timescaledb.compress_orderby = 'occurred_at DESC'
);
SELECT add_compression_policy('agent_events', INTERVAL '7 days', if_not_exists => TRUE);

-- pgvector indexes (per-tenant, accurate ANN search)
CREATE INDEX idx_decisions_embedding ON decisions
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

CREATE INDEX idx_evidence_embedding ON evidence
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- Materialized views
CREATE MATERIALIZED VIEW decision_conflicts AS
SELECT ... FROM decisions d1 JOIN decisions d2 ... WITH DATA;
CREATE UNIQUE INDEX idx_decision_conflicts_pair ON decision_conflicts(decision_a_id, decision_b_id);

CREATE MATERIALIZED VIEW agent_current_state AS
SELECT ... WITH DATA;

CREATE VIEW current_decisions AS
SELECT * FROM decisions WHERE valid_to IS NULL ORDER BY valid_from DESC;

-- Row-level security (intra-tenant defense-in-depth)
ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_grants ENABLE ROW LEVEL SECURITY;

-- RLS policies for akashi_app role
CREATE POLICY org_isolation_agents ON agents
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);

CREATE POLICY org_isolation_runs ON agent_runs
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);

CREATE POLICY org_isolation_decisions ON decisions
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);

CREATE POLICY org_isolation_grants ON access_grants
    FOR ALL TO akashi_app
    USING (org_id = current_setting('app.org_id', true)::uuid)
    WITH CHECK (org_id = current_setting('app.org_id', true)::uuid);

-- Grant permissions to application role
GRANT ALL ON ALL TABLES IN SCHEMA {schema_name} TO akashi_app;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA {schema_name} TO akashi_app;
```

---

## 4. Go Package: `internal/tenant`

### 4.1 Types

**File: `internal/tenant/tenant.go`**

```go
package tenant

import (
    "github.com/google/uuid"
    "github.com/jackc/pgx/v5/pgxpool"
)

// IsolationTier defines the tenant's isolation level.
type IsolationTier string

const (
    TierSchema   IsolationTier = "schema"
    TierDatabase IsolationTier = "database"
)

// Info holds routing information for a tenant, resolved from the
// organizations table at request time.
type Info struct {
    OrgID         uuid.UUID
    SchemaName    string
    IsolationTier IsolationTier
    DatabaseURL   *string // non-nil only for TierDatabase
}

// Scope represents a resolved, ready-to-use database scope for a
// specific tenant. Handlers receive this via context.
type Scope struct {
    pool   *pgxpool.Pool
    schema string
    orgID  uuid.UUID
}

// Pool returns the connection pool for this tenant.
func (s *Scope) Pool() *pgxpool.Pool { return s.pool }

// Schema returns the schema name.
func (s *Scope) Schema() string { return s.schema }

// OrgID returns the tenant's organization ID.
func (s *Scope) OrgID() uuid.UUID { return s.orgID }
```

### 4.2 Manager

**File: `internal/tenant/manager.go`**

The Manager is the central component that resolves tenants and manages connection pools.

```go
package tenant

import (
    "context"
    "fmt"
    "sync"

    "github.com/google/uuid"
    "github.com/jackc/pgx/v5/pgxpool"
)

// Manager resolves tenant routing and manages per-tenant connection pools.
type Manager struct {
    controlPool *pgxpool.Pool          // shared DB pool (for schema tenants + control plane)
    tenantPools map[uuid.UUID]*pgxpool.Pool // dedicated pools for database-tier tenants
    mu          sync.RWMutex
    logger      *slog.Logger
}

func New(controlPool *pgxpool.Pool, logger *slog.Logger) *Manager

// Resolve looks up the tenant's routing info from the organizations table
// and returns a Scope with the correct pool and schema.
//
// For schema-tier tenants: returns controlPool + schema_name.
// For database-tier tenants: returns or creates a dedicated pool.
//
// The Scope is safe for concurrent use within the request lifetime.
func (m *Manager) Resolve(ctx context.Context, orgID uuid.UUID) (*Scope, error)

// ControlPool returns the shared control-plane pool (for org CRUD, signup, etc.).
func (m *Manager) ControlPool() *pgxpool.Pool

// Close closes all managed tenant pools.
func (m *Manager) Close()
```

**Resolve implementation details:**
1. Query `SELECT schema_name, isolation_tier, database_url FROM organizations WHERE id = $1` from control pool.
2. If `isolation_tier = 'schema'`: return `Scope{pool: controlPool, schema: info.SchemaName, orgID: orgID}`.
3. If `isolation_tier = 'database'`: check `tenantPools[orgID]`. If exists, return it. If not, create a new `pgxpool.Pool` from `database_url`, cache it, return `Scope{pool: newPool, schema: "public", orgID: orgID}`.
4. Cache the `Info` with a short TTL (30s) to avoid hitting the organizations table on every request. Use `sync.Map` or a simple mutex-protected map with expiry.

### 4.3 Transaction Helper

**File: `internal/tenant/scope.go`**

Every database operation within a tenant scope must set `search_path` and `app.org_id` at the start of each transaction. This is the critical integration point.

```go
// BeginTx starts a transaction scoped to this tenant.
// Sets search_path to the tenant's schema and app.org_id for RLS.
func (s *Scope) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
    tx, err := s.pool.BeginTx(ctx, opts)
    if err != nil {
        return nil, fmt.Errorf("tenant: begin tx: %w", err)
    }

    // SET LOCAL is scoped to the transaction — safe with PgBouncer transaction pooling.
    _, err = tx.Exec(ctx, fmt.Sprintf(
        "SET LOCAL search_path = %s, public",
        pgx.Identifier{s.schema}.Sanitize(),
    ))
    if err != nil {
        _ = tx.Rollback(ctx)
        return nil, fmt.Errorf("tenant: set search_path: %w", err)
    }

    _, err = tx.Exec(ctx, "SET LOCAL app.org_id = $1", s.orgID.String())
    if err != nil {
        _ = tx.Rollback(ctx)
        return nil, fmt.Errorf("tenant: set org_id: %w", err)
    }

    return tx, nil
}

// Exec executes a single statement within a one-shot transaction.
// For operations that don't need explicit transaction control.
func (s *Scope) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
    tx, err := s.BeginTx(ctx, pgx.TxOptions{})
    if err != nil {
        return pgconn.CommandTag{}, err
    }
    defer func() { _ = tx.Rollback(ctx) }()

    tag, err := tx.Exec(ctx, sql, args...)
    if err != nil {
        return tag, err
    }
    return tag, tx.Commit(ctx)
}

// Query runs a query within a one-shot transaction.
func (s *Scope) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
    // For queries, we set search_path as a session-level SET LOCAL within a
    // short-lived tx that we keep open for the rows' lifetime.
    // Implementation: use pool.Acquire() + conn.Begin() pattern.
    // Rows must be fully consumed before the tx commits.
    ...
}
```

**Critical design decision:** `SET LOCAL` is transaction-scoped and compatible with PgBouncer transaction pooling. When the transaction ends, the settings revert. No state leaks to the next user of the pooled connection.

### 4.4 Provisioner

**File: `internal/tenant/provisioner.go`**

Creates new tenant schemas and databases.

```go
// ProvisionSchema creates a new schema for a tenant in the shared database.
// Applies the full template SQL to set up all tables, indexes, hypertables,
// materialized views, and RLS policies.
func (m *Manager) ProvisionSchema(ctx context.Context, orgSlug string) (schemaName string, err error)

// ProvisionDatabase creates a new database for an enterprise tenant.
// This calls the TigerData API (or runs CREATE DATABASE for self-hosted).
// After creation, connects to the new database and applies the template.
func (m *Manager) ProvisionDatabase(ctx context.Context, orgSlug string, databaseURL string) (schemaName string, err error)

// DeprovisionSchema drops a tenant's schema (GDPR deletion).
// Returns an error if the schema contains data and force is false.
func (m *Manager) DeprovisionSchema(ctx context.Context, schemaName string, force bool) error

// DeprovisionDatabase drops a tenant's dedicated database connection
// and optionally deletes the TigerData service.
func (m *Manager) DeprovisionDatabase(ctx context.Context, orgID uuid.UUID) error
```

**ProvisionSchema implementation:**
1. `CREATE SCHEMA IF NOT EXISTS tenant_{slug}`
2. `SET search_path = tenant_{slug}, public`
3. Execute template SQL (from embedded `template.sql`)
4. Verify by querying `pg_tables` for expected tables
5. Update `organizations.schema_name = 'tenant_{slug}'`

### 4.5 Migrator

**File: `internal/tenant/migrator.go`**

Applies incremental migrations across all tenant schemas.

```go
// MigrateAll applies pending migrations to all tenant schemas.
// Migrations are defined as SQL files in the tenant_migrations/ directory.
// Each migration is applied within the tenant's schema (search_path set).
//
// Tracks per-schema migration state in a tenant_schema_versions table
// within each tenant schema.
//
// On partial failure: logs errors, continues to remaining schemas,
// returns a MultiError with all failures.
func (m *Manager) MigrateAll(ctx context.Context, migrationsFS fs.FS) error

// MigrateOne applies pending migrations to a single tenant schema.
func (m *Manager) MigrateOne(ctx context.Context, schemaName string, migrationsFS fs.FS) error
```

**Migration tracking:**
Each tenant schema gets a `_schema_migrations` table:
```sql
CREATE TABLE IF NOT EXISTS _schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**Migration file naming:** `tenant_migrations/001_initial.sql`, `tenant_migrations/002_add_tags.sql`, etc. These are separate from the control-plane migrations in `migrations/`.

---

## 5. Storage Layer Refactoring

### 5.1 Split storage.DB into Control + Tenant

The current `storage.DB` struct serves dual duty: control-plane operations (org CRUD, auth lookups) and tenant-scoped operations (decisions, events, agents). After refactoring:

**`storage.DB` (control plane only):**
- Holds the control pool (shared database)
- Methods: `GetOrganization`, `CreateOrganization`, `UpdateOrganization`, `IncrementUsage`, `GetUsage`, `CreateEmailVerification`, `VerifyEmail`, `GetAgentByAgentIDGlobal`, `Ping`, `RunMigrations`
- Also holds `notifyConn` for LISTEN/NOTIFY

**`storage.TenantStore` (tenant-scoped operations):**
- Constructed from a `*tenant.Scope`
- All methods that currently take `orgID uuid.UUID` as first parameter move here
- The `orgID` comes from the scope, not from parameters
- All operations go through `scope.BeginTx()` to get proper `search_path`

```go
// TenantStore provides tenant-scoped database operations.
// Created per-request from a tenant.Scope.
type TenantStore struct {
    scope *tenant.Scope
}

func NewTenantStore(scope *tenant.Scope) *TenantStore

// Methods (org_id is implicit from scope):
func (ts *TenantStore) CreateDecision(ctx context.Context, d model.Decision) (model.Decision, error)
func (ts *TenantStore) QueryDecisions(ctx context.Context, req model.QueryRequest) ([]model.Decision, int, error)
func (ts *TenantStore) GetDecision(ctx context.Context, id uuid.UUID, includeAlts, includeEvidence bool) (model.Decision, error)
func (ts *TenantStore) SearchDecisionsByEmbedding(ctx context.Context, embedding pgvector.Vector, filters model.QueryFilters, limit int) ([]model.SearchResult, error)
func (ts *TenantStore) CreateAgent(ctx context.Context, agent model.Agent) (model.Agent, error)
func (ts *TenantStore) GetAgentByAgentID(ctx context.Context, agentID string) (model.Agent, error)
func (ts *TenantStore) ListAgents(ctx context.Context) ([]model.Agent, error)
func (ts *TenantStore) CreateTraceTx(ctx context.Context, params CreateTraceParams) (model.AgentRun, model.Decision, error)
func (ts *TenantStore) ListConflicts(ctx context.Context, filters ConflictFilters, limit, offset int) ([]model.DecisionConflict, error)
func (ts *TenantStore) CountConflicts(ctx context.Context, filters ConflictFilters) (int, error)
func (ts *TenantStore) RefreshConflicts(ctx context.Context) error
func (ts *TenantStore) DeleteAgentData(ctx context.Context, agentID string) (DeleteAgentResult, error)
func (ts *TenantStore) HasAccess(ctx context.Context, granteeID uuid.UUID, resourceType, resourceID, permission string) (bool, error)
// ... all current org-scoped methods, minus the orgID parameter
```

### 5.2 Query Changes

**Before (current):**
```go
func (db *DB) QueryDecisions(ctx context.Context, orgID uuid.UUID, req model.QueryRequest) ([]model.Decision, int, error) {
    rows, err := db.pool.Query(ctx, sql, orgID, ...)
}
```

**After:**
```go
func (ts *TenantStore) QueryDecisions(ctx context.Context, req model.QueryRequest) ([]model.Decision, int, error) {
    tx, err := ts.scope.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
    // search_path and app.org_id already set by BeginTx
    rows, err := tx.Query(ctx, sql, ts.scope.OrgID(), ...)
    // org_id still in WHERE for defense-in-depth, but schema boundary is primary isolation
}
```

### 5.3 COPY Operations

`pgx.CopyFrom` must also run within the scoped transaction so that `search_path` is set. The current pattern:

```go
db.pool.CopyFrom(ctx, pgx.Identifier{"agent_events"}, columns, source)
```

Becomes:

```go
tx, _ := ts.scope.BeginTx(ctx, pgx.TxOptions{})
tx.CopyFrom(ctx, pgx.Identifier{"agent_events"}, columns, source)
tx.Commit(ctx)
```

### 5.4 LISTEN/NOTIFY

LISTEN/NOTIFY operates at the database level, not schema level. For schema-isolated tenants sharing a database, notifications are shared. The notification payload already includes `org_id` so the SSE broker can filter.

For database-isolated enterprise tenants, we need a dedicated notify connection per database. The `tenant.Manager` manages these:

```go
// NotifyConn returns a LISTEN/NOTIFY connection for the given tenant.
// Schema tenants share the control DB's notify connection.
// Database tenants get their own.
func (m *Manager) NotifyConn(orgID uuid.UUID) *pgx.Conn
```

---

## 6. Handler & Middleware Changes

### 6.1 Tenant Resolution Middleware

**File: `internal/server/middleware.go`**

Add middleware that resolves the tenant after authentication:

```go
func (s *Server) tenantMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        orgID := OrgIDFromContext(r.Context())
        if orgID == uuid.Nil {
            writeError(w, r, 401, "UNAUTHORIZED", "missing org context")
            return
        }

        scope, err := s.tenantMgr.Resolve(r.Context(), orgID)
        if err != nil {
            writeError(w, r, 500, "INTERNAL_ERROR", "failed to resolve tenant")
            return
        }

        ctx := ctxutil.WithTenantScope(r.Context(), scope)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}
```

**Middleware chain update (server.go):**
```
requestID → securityHeaders → tracing → logging → auth → tenant → rateLimit → handler
```

### 6.2 Handler Changes

Handlers currently receive `h.db *storage.DB` and call methods with `orgID`. After refactoring:

```go
type Handlers struct {
    controlDB   *storage.DB          // for auth, signup, health
    tenantMgr   *tenant.Manager      // for resolving tenants
    decisionSvc *decisions.Service
    // ... other fields unchanged
}
```

**In each handler:**
```go
func (h *Handlers) HandleQuery(w http.ResponseWriter, r *http.Request) {
    scope := ctxutil.TenantScopeFromContext(r.Context())
    ts := storage.NewTenantStore(scope)
    decisions, total, err := ts.QueryDecisions(r.Context(), req)
    // ...
}
```

### 6.3 Context Additions

**File: `internal/ctxutil/ctxutil.go`**

```go
const keyTenantScope contextKey = "tenant_scope"

func WithTenantScope(ctx context.Context, scope *tenant.Scope) context.Context {
    return context.WithValue(ctx, keyTenantScope, scope)
}

func TenantScopeFromContext(ctx context.Context) *tenant.Scope {
    if v, ok := ctx.Value(keyTenantScope).(*tenant.Scope); ok {
        return v
    }
    return nil
}
```

---

## 7. Service Layer Changes

### 7.1 decisions.Service

Currently holds `*storage.DB`. Needs to accept a `*storage.TenantStore` per-operation instead of per-construction.

**Option A (recommended):** Make service methods accept `*storage.TenantStore` as parameter:

```go
func (s *Service) Trace(ctx context.Context, ts *storage.TenantStore, input TraceInput) (TraceResult, error)
func (s *Service) Query(ctx context.Context, ts *storage.TenantStore, req model.QueryRequest) ([]model.Decision, int, error)
func (s *Service) Search(ctx context.Context, ts *storage.TenantStore, query string, ...) ([]model.SearchResult, error)
```

**Option B:** Have the service resolve TenantStore from context. This keeps signatures cleaner but makes the dependency implicit.

Go with Option A — explicit is better.

### 7.2 billing.Service

Billing operates on the control plane (org_usage is in the public schema). No changes to the billing service. It continues to use `*storage.DB` directly.

### 7.3 signup.Service

Signup creates orgs and initial agents. After creating the org record, it must also provision the tenant schema:

```go
func (s *Service) Signup(ctx context.Context, req model.SignupRequest) error {
    // 1. Create org in control DB (existing)
    org, err := s.db.CreateOrganization(ctx, ...)

    // 2. Provision tenant schema (NEW)
    schemaName, err := s.tenantMgr.ProvisionSchema(ctx, org.Slug)

    // 3. Update org with schema_name
    org.SchemaName = schemaName
    s.db.UpdateOrganization(ctx, org)

    // 4. Create initial admin agent in tenant schema (existing, but now via TenantStore)
    scope, _ := s.tenantMgr.Resolve(ctx, org.ID)
    ts := storage.NewTenantStore(scope)
    ts.CreateAgent(ctx, initialAdmin)

    // 5. Send verification email (existing)
}
```

---

## 8. Agent Tags System

### 8.1 Model Changes

**File: `internal/model/agent.go`**

```go
type Agent struct {
    // ... existing fields ...
    Tags []string `json:"tags"`
}
```

### 8.2 API Changes

**CreateAgentRequest** — add optional `tags` field:
```go
type CreateAgentRequest struct {
    // ... existing fields ...
    Tags []string `json:"tags,omitempty"`
}
```

**New endpoints:**

| Method | Path | Description |
|--------|------|-------------|
| `PUT` | `/v1/agents/{agent_id}/tags` | Replace agent's tags |
| `POST` | `/v1/agents/{agent_id}/tags` | Add tags to agent |
| `DELETE` | `/v1/agents/{agent_id}/tags/{tag}` | Remove specific tag |
| `GET` | `/v1/agents?tag={tag}` | List agents with tag |

### 8.3 Tag-Based Access Grants

**CreateGrantRequest** — allow either `grantee_agent_id` or `grantee_tag`:
```go
type CreateGrantRequest struct {
    GranteeAgentID *string `json:"grantee_agent_id,omitempty"`
    GranteeTag     *string `json:"grantee_tag,omitempty"`
    ResourceType   string  `json:"resource_type"`
    ResourceID     *string `json:"resource_id,omitempty"`
    Permission     string  `json:"permission"`
    ExpiresAt      *string `json:"expires_at,omitempty"`
}
```

**HasAccess evaluation (updated):**
```go
func (ts *TenantStore) HasAccess(ctx context.Context, agent model.Agent, resourceType, resourceID, permission string) (bool, error) {
    // Check direct grants (existing)
    directSQL := `SELECT EXISTS(
        SELECT 1 FROM access_grants
        WHERE grantee_id = $1
          AND resource_type = $2
          AND (resource_id = $3 OR resource_id IS NULL)
          AND permission = $4
          AND (expires_at IS NULL OR expires_at > now())
    )`

    // Check tag-based grants (new)
    tagSQL := `SELECT EXISTS(
        SELECT 1 FROM access_grants
        WHERE grantee_tag = ANY($1)
          AND resource_type = $2
          AND (resource_id = $3 OR resource_id IS NULL)
          AND permission = $4
          AND (expires_at IS NULL OR expires_at > now())
    )`

    // Return true if either direct or tag grant exists
}
```

**Example usage:**
```
POST /v1/grants
{
    "grantee_tag": "security-team",
    "resource_type": "agent_traces",
    "permission": "read"
}
```
This grants all agents tagged `security-team` read access to all agent traces.

---

## 9. Existing Data Migration

### 9.1 Strategy

Migrate existing data from `public` schema tables into `tenant_default` schema:

1. Create `tenant_default` schema using the template
2. For each tenant-scoped table: `INSERT INTO tenant_default.{table} SELECT * FROM public.{table}`
3. For `agent_events` (hypertable): use `INSERT INTO ... SELECT` which TimescaleDB handles for cross-schema hypertable population
4. Recreate materialized views in `tenant_default`
5. Verify row counts match
6. Drop old tables from `public` schema (after verification)

### 9.2 Migration Script

**File: `migrations/016_tenant_isolation.sql`** (runs against control database)

This migration:
1. Adds columns to `organizations` (schema_name, isolation_tier, database_url, tags)
2. Creates the `tenant_default` schema with template
3. Copies all data from public tables into `tenant_default` tables
4. Updates the default org's `schema_name` to `tenant_default`
5. Drops the old public-schema tenant tables

**The migration must be idempotent.** Use `IF NOT EXISTS` guards.

### 9.3 Rollback Plan

Before dropping public-schema tables, create a backup schema:
```sql
CREATE SCHEMA IF NOT EXISTS _backup_pre_isolation;
-- For each table:
CREATE TABLE _backup_pre_isolation.agents AS SELECT * FROM public.agents;
```

---

## 10. Configuration Changes

**File: `internal/config/config.go`**

```go
type Config struct {
    // ... existing fields ...

    // Tenant isolation
    TenantMigrationsPath string `env:"AKASHI_TENANT_MIGRATIONS_PATH" default:"tenant_migrations"`
}
```

No other config changes needed. The control database URL is the existing `DATABASE_URL`. Enterprise tenant database URLs are stored in the `organizations` table.

---

## 11. Conflict Refresh Loop

The conflict refresh loop currently runs `REFRESH MATERIALIZED VIEW CONCURRENTLY decision_conflicts` on the shared database. With schema isolation, it must refresh the view in every tenant schema:

```go
func conflictRefreshLoop(ctx context.Context, tenantMgr *tenant.Manager, db *storage.DB, logger *slog.Logger, interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            schemas, err := db.ListTenantSchemas(ctx)
            if err != nil {
                logger.Warn("conflict refresh: list schemas failed", "error", err)
                continue
            }
            for _, schema := range schemas {
                scope, err := tenantMgr.ResolveBySchema(ctx, schema)
                if err != nil {
                    logger.Warn("conflict refresh: resolve failed", "schema", schema, "error", err)
                    continue
                }
                ts := storage.NewTenantStore(scope)
                if err := ts.RefreshConflicts(ctx); err != nil {
                    logger.Warn("conflict refresh failed", "schema", schema, "error", err)
                }
            }
        }
    }
}
```

---

## 12. SSE Broker Changes

The SSE broker currently doesn't filter by org_id (noted as a bug in MEMORY.md). With tenant isolation:

1. Notifications from schema tenants all arrive on the shared notify connection. The payload includes org_id. The broker must filter: only send events to SSE clients whose JWT org_id matches the event's org_id.

2. Notifications from database tenants arrive on their dedicated notify connections. The broker needs one listener per enterprise tenant database.

```go
type Broker struct {
    clients    map[uuid.UUID]map[chan Event]struct{} // org_id -> set of client channels
    tenantMgr  *tenant.Manager
    // ...
}

// Subscribe registers an SSE client for a specific org.
func (b *Broker) Subscribe(orgID uuid.UUID) (<-chan Event, func())

// Start listens on shared notify connection + all enterprise notify connections.
func (b *Broker) Start(ctx context.Context)
```

---

## 13. UI Changes

### 13.1 Agent Tags UI

**File: `ui/src/pages/Agents.tsx`**

- Display tags as badges in the agent table
- Allow adding/removing tags in the Create Agent dialog
- Tag input: comma-separated text field or a tag-chip component

### 13.2 Grant Dialog Enhancement

**File: `ui/src/pages/Agents.tsx` (or new Grants page)**

- When creating a grant, allow choosing between "Specific Agent" and "Tag Group"
- If "Tag Group", show a text input for the tag name
- Display tag-based grants differently in the grants list

### 13.3 Types Updates

**File: `ui/src/types/api.ts`**

```typescript
export interface Agent {
    // ... existing fields ...
    tags: string[];
}

export interface CreateAgentRequest {
    // ... existing fields ...
    tags?: string[];
}

export interface CreateGrantRequest {
    grantee_agent_id?: string;
    grantee_tag?: string;
    resource_type: string;
    resource_id?: string;
    permission: string;
    expires_at?: string;
}
```

---

## 14. Testing Strategy

### 14.1 Unit Tests

| Test | Package | Description |
|------|---------|-------------|
| `TestProvisionSchema` | `tenant` | Creates schema, verifies all tables/indexes exist |
| `TestScopeBeginTx` | `tenant` | Verifies search_path and app.org_id are set |
| `TestScopeCrossSchemaIsolation` | `tenant` | Two schemas, verify queries don't leak |
| `TestMigrateAll` | `tenant` | Apply migration to N schemas, verify all updated |
| `TestMigratePartialFailure` | `tenant` | One schema fails, others succeed |
| `TestHasAccessWithTags` | `storage` | Tag-based grant evaluation |
| `TestHasAccessDirectAndTag` | `storage` | Both grant types simultaneously |
| `TestAgentTags` | `storage` | CRUD operations on agent tags |

### 14.2 Integration Tests

| Test | Description |
|------|-------------|
| `TestSchemaIsolationEndToEnd` | Create 2 orgs, insert decisions in each, verify no cross-org visibility |
| `TestDatabaseIsolationEndToEnd` | Create schema org + database org, verify total isolation |
| `TestGDPRDeletion` | Provision schema, insert data, DROP SCHEMA CASCADE, verify clean |
| `TestPgBouncerCompatibility` | Verify SET LOCAL doesn't leak across transactions |
| `TestConflictRefreshPerSchema` | Verify materialized view refresh hits all schemas |
| `TestSSEOrgFiltering` | Verify SSE events are scoped to org |
| `TestTagBasedAccessGrants` | Tag agent, create tag grant, verify access |

### 14.3 Load Tests

| Test | Description |
|------|-------------|
| `TestSchemaScaling100` | Provision 100 schemas, run queries, measure latency |
| `TestMigrationPerformance` | Apply migration to 100 schemas, measure total time |
| `TestHNSWPerSchemaAccuracy` | Compare ANN recall: shared index vs per-schema index |

---

## 15. Implementation Order

Execute in this sequence. Each phase is independently deployable.

### Phase 1: Foundation (tenant package + schema provisioning)
1. Create `internal/tenant/tenant.go` — types
2. Create `internal/tenant/template.sql` — schema DDL template
3. Create `internal/tenant/provisioner.go` — schema creation
4. Create `internal/tenant/manager.go` — resolve + pool management
5. Create `internal/tenant/scope.go` — BeginTx with SET LOCAL
6. Write `migrations/016_tenant_isolation.sql` — add org columns + create tenant_default schema + migrate existing data
7. Tests: TestProvisionSchema, TestScopeBeginTx

### Phase 2: Storage refactor (split control + tenant)
1. Create `storage/tenant_store.go` — TenantStore with all tenant-scoped methods
2. Refactor each existing storage file: extract tenant-scoped methods into TenantStore, keep control-plane methods on DB
3. Update all method signatures: remove orgID parameter, use scope.OrgID()
4. Update all pool access: use scope.BeginTx() instead of db.pool directly
5. Tests: TestScopeCrossSchemaIsolation

### Phase 3: Handler + middleware integration
1. Add tenant resolution middleware to server
2. Update ctxutil with TenantScope context helpers
3. Refactor all handlers to use TenantStore from context
4. Update decisions.Service to accept TenantStore per-operation
5. Update signup flow to provision schema on org creation
6. Tests: full integration tests

### Phase 4: Agent tags + tag-based grants
1. Add tags column to agent model + template
2. Add grantee_tag to access_grants model + template
3. Implement tag-based HasAccess evaluation
4. Add tag CRUD API endpoints
5. Update UI: agent tags display + creation, grant dialog
6. Tests: TestHasAccessWithTags, TestAgentTags

### Phase 5: Database isolation (enterprise tier)
1. Add database provisioning to tenant.Manager
2. Add per-database notify connection management
3. Add database tenant pool lifecycle (create, cache, close)
4. Update SSE broker for multi-database notifications
5. Add enterprise upgrade API (migrate schema tenant to database tenant)
6. Tests: TestDatabaseIsolationEndToEnd

### Phase 6: Operational hardening
1. Tenant migration runner (MigrateAll across schemas)
2. Per-schema conflict refresh loop
3. Per-schema compression policy management
4. Monitoring: per-tenant query metrics
5. GDPR deletion: DROP SCHEMA CASCADE endpoint
6. Tests: load tests, migration performance

---

## 16. File Manifest

### New Files

| File | Phase | Description |
|------|-------|-------------|
| `internal/tenant/tenant.go` | 1 | Types: IsolationTier, Info, Scope |
| `internal/tenant/manager.go` | 1 | Manager: Resolve, ControlPool, Close |
| `internal/tenant/scope.go` | 1 | Scope: BeginTx, Exec, Query |
| `internal/tenant/provisioner.go` | 1 | ProvisionSchema, ProvisionDatabase, Deprovision* |
| `internal/tenant/migrator.go` | 6 | MigrateAll, MigrateOne |
| `internal/tenant/template.sql` | 1 | Embedded SQL template for tenant schemas |
| `internal/storage/tenant_store.go` | 2 | TenantStore with all tenant-scoped methods |
| `migrations/016_tenant_isolation.sql` | 1 | Schema changes + data migration |
| `tenant_migrations/001_initial.sql` | 6 | First tenant-schema migration (matches template) |
| `tenant_migrations/002_add_tags.sql` | 4 | Agent tags + tag-based grants |

### Modified Files

| File | Phase | Changes |
|------|-------|---------|
| `internal/model/agent.go` | 4 | Add Tags field, update AccessGrant |
| `internal/model/api.go` | 4 | Add tags to request types, update grant request |
| `internal/storage/pool.go` | 2 | DB struct loses tenant methods, gains ListTenantSchemas |
| `internal/storage/decisions.go` | 2 | Methods move to TenantStore |
| `internal/storage/agents.go` | 2 | Methods move to TenantStore (except GetAgentByAgentIDGlobal) |
| `internal/storage/events.go` | 2 | Methods move to TenantStore |
| `internal/storage/runs.go` | 2 | Methods move to TenantStore |
| `internal/storage/grants.go` | 2 | Methods move to TenantStore, add tag-based HasAccess |
| `internal/storage/alternatives.go` | 2 | Methods move to TenantStore |
| `internal/storage/evidence.go` | 2 | Methods move to TenantStore |
| `internal/storage/conflicts.go` | 2 | Methods move to TenantStore |
| `internal/storage/trace.go` | 2 | CreateTraceTx moves to TenantStore |
| `internal/storage/delete.go` | 2 | DeleteAgentData moves to TenantStore |
| `internal/ctxutil/ctxutil.go` | 3 | Add WithTenantScope, TenantScopeFromContext |
| `internal/server/server.go` | 3 | Accept tenant.Manager, update middleware chain |
| `internal/server/handlers.go` | 3 | Use TenantStore from context |
| `internal/server/handlers_decisions.go` | 3 | Use TenantStore from context |
| `internal/server/middleware.go` | 3 | Add tenantMiddleware |
| `internal/service/decisions/service.go` | 3 | Accept TenantStore per-method |
| `internal/signup/signup.go` | 3 | Provision schema on signup |
| `internal/server/broker.go` | 5 | Filter SSE by org_id, multi-DB listeners |
| `cmd/akashi/main.go` | 3 | Create tenant.Manager, pass to server |
| `ui/src/pages/Agents.tsx` | 4 | Tags display + creation |
| `ui/src/types/api.ts` | 4 | Agent.tags, CreateGrantRequest changes |

---

## 17. Invariants & Constraints

These must hold at all times:

1. **No query on a tenant-scoped table may execute without `search_path` set to the tenant's schema.** Enforced by requiring all operations go through `TenantStore` → `Scope.BeginTx()`.

2. **`SET LOCAL` only, never `SET`.** `SET LOCAL` is transaction-scoped and PgBouncer-safe. `SET` persists on the session and would leak to the next transaction on the pooled connection.

3. **`org_id` remains in all tenant-scoped tables as defense-in-depth.** The schema boundary is the primary isolation. The `WHERE org_id` clause is the secondary check. RLS is the tertiary check.

4. **Control-plane tables never appear in tenant schemas.** `organizations`, `org_usage`, `email_verifications` live only in `public`.

5. **Every tenant schema has identical structure.** The template is the single source of truth. Tenant-schema migrations are applied to all schemas atomically.

6. **Enterprise database tenants use `public` schema within their dedicated database.** They don't need a `tenant_xxx` schema because the database IS the isolation boundary.

7. **Tag-based grants are evaluated at query time, not cached.** If an agent's tags change, their access changes immediately.

8. **Schema names are immutable after creation.** Org slugs can change; schema names cannot. `schema_name` is set once during provisioning.

9. **The `akashi_app` role must exist in every database** (shared + enterprise). RLS policies reference this role.

10. **LISTEN/NOTIFY channels include org_id in the payload.** The broker filters by org_id before delivering to SSE clients.
