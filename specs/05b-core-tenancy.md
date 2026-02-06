# Spec 05b: Core Tenancy (Model + Auth + Storage + Service)

**Status**: Ready for implementation
**Phase**: 2 of 5 (Multi-Tenancy)
**Depends on**: Phase 1 (05a — migration 014 must exist)
**Blocks**: Phase 3 (05c)

## Goal

Thread `org_id` through the entire Go codebase: model types, JWT claims, storage queries, service layer, and buffer. After this phase, every data path carries org context, but handlers and MCP tools are not yet updated (that's Phase 3).

## Deliverables

1. Updated model types with `OrgID` field and new roles
2. Updated JWT claims with `org_id`
3. Updated token issuance to include `org_id`
4. All storage methods gain `orgID uuid.UUID` parameter
5. All service methods gain `orgID uuid.UUID` parameter
6. Buffer carries `org_id` through to COPY
7. Org context middleware and helper
8. `go vet ./...` passes (compilation check — handlers will be broken until Phase 3)

---

## 1. Model Changes

### `internal/model/agent.go`

Add `OrgID` field to `Agent` and add new roles:

```go
// Current roles
const (
    RoleAdmin  AgentRole = "admin"
    RoleAgent  AgentRole = "agent"
    RoleReader AgentRole = "reader"
)

// Change to:
const (
    RolePlatformAdmin AgentRole = "platform_admin"
    RoleOrgOwner      AgentRole = "org_owner"
    RoleAdmin         AgentRole = "admin"
    RoleAgent         AgentRole = "agent"
    RoleReader        AgentRole = "reader"
)
```

Add `OrgID` to Agent struct:

```go
type Agent struct {
    ID         uuid.UUID      `json:"id"`
    AgentID    string         `json:"agent_id"`
    OrgID      uuid.UUID      `json:"org_id"`   // NEW
    Name       string         `json:"name"`
    Role       AgentRole      `json:"role"`
    APIKeyHash *string        `json:"-"`
    Metadata   map[string]any `json:"metadata"`
    CreatedAt  time.Time      `json:"created_at"`
    UpdatedAt  time.Time      `json:"updated_at"`
}
```

Add `OrgID` to `AgentRun`:

```go
type AgentRun struct {
    ID          uuid.UUID      `json:"id"`
    AgentID     string         `json:"agent_id"`
    OrgID       uuid.UUID      `json:"org_id"`   // NEW
    TraceID     *string        `json:"trace_id,omitempty"`
    // ... rest unchanged
}
```

Add `OrgID` to `AgentEvent`:

```go
type AgentEvent struct {
    ID          uuid.UUID      `json:"id"`
    RunID       uuid.UUID      `json:"run_id"`
    OrgID       uuid.UUID      `json:"org_id"`   // NEW
    // ... rest unchanged
}
```

Add `OrgID` to `Decision`:

```go
type Decision struct {
    ID             uuid.UUID      `json:"id"`
    RunID          uuid.UUID      `json:"run_id"`
    AgentID        string         `json:"agent_id"`
    OrgID          uuid.UUID      `json:"org_id"`   // NEW
    // ... rest unchanged
}
```

Add `OrgID` to `AccessGrant`:

```go
type AccessGrant struct {
    ID           uuid.UUID  `json:"id"`
    OrgID        uuid.UUID  `json:"org_id"`   // NEW
    GrantorID    uuid.UUID  `json:"grantor_id"`
    // ... rest unchanged
}
```

Add a role hierarchy helper:

```go
// RoleRank returns the numeric rank of a role (higher = more privileges).
func RoleRank(r AgentRole) int {
    switch r {
    case RolePlatformAdmin:
        return 100
    case RoleOrgOwner:
        return 4
    case RoleAdmin:
        return 3
    case RoleAgent:
        return 2
    case RoleReader:
        return 1
    default:
        return 0
    }
}

// RoleAtLeast returns true if role r has at least the privileges of minRole.
func RoleAtLeast(r, minRole AgentRole) bool {
    return RoleRank(r) >= RoleRank(minRole)
}
```

### `internal/model/api.go`

Add `Organization` model type:

```go
type Organization struct {
    ID                   uuid.UUID `json:"id"`
    Name                 string    `json:"name"`
    Slug                 string    `json:"slug"`
    Plan                 string    `json:"plan"`
    StripeCustomerID     *string   `json:"stripe_customer_id,omitempty"`
    StripeSubscriptionID *string   `json:"stripe_subscription_id,omitempty"`
    DecisionLimit        int       `json:"decision_limit"`
    AgentLimit           int       `json:"agent_limit"`
    Email                string    `json:"email"`
    EmailVerified        bool      `json:"email_verified"`
    CreatedAt            time.Time `json:"created_at"`
    UpdatedAt            time.Time `json:"updated_at"`
}
```

Add `OrgUsage` type:

```go
type OrgUsage struct {
    OrgID          uuid.UUID `json:"org_id"`
    Period         string    `json:"period"`
    DecisionCount  int       `json:"decision_count"`
}
```

Add `DecisionConflict` org_id field (if not already there — check the struct and add `OrgID uuid.UUID`).

---

## 2. Auth Changes

### `internal/auth/auth.go`

Add `OrgID` to Claims:

```go
type Claims struct {
    jwt.RegisteredClaims
    AgentID string          `json:"agent_id"`
    OrgID   uuid.UUID       `json:"org_id"`    // NEW
    Role    model.AgentRole `json:"role"`
}
```

Update `IssueToken` to accept org_id:

```go
// IssueToken creates a signed JWT for the given agent.
// The agent's OrgID field must be populated.
func (m *JWTManager) IssueToken(agent model.Agent) (string, time.Time, error) {
    now := time.Now().UTC()
    exp := now.Add(m.expiration)

    claims := Claims{
        RegisteredClaims: jwt.RegisteredClaims{
            Subject:   agent.ID.String(),
            Issuer:    "akashi",
            IssuedAt:  jwt.NewNumericDate(now),
            ExpiresAt: jwt.NewNumericDate(exp),
            ID:        uuid.New().String(),
        },
        AgentID: agent.AgentID,
        OrgID:   agent.OrgID,     // NEW — populated from agent lookup
        Role:    agent.Role,
    }

    token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
    signed, err := token.SignedString(m.privateKey)
    if err != nil {
        return "", time.Time{}, fmt.Errorf("auth: sign token: %w", err)
    }
    return signed, exp, nil
}
```

**Note**: `IssueToken` signature stays the same (`agent model.Agent`). The `Agent` struct now has `OrgID` populated by the caller (the auth handler in Phase 3 will populate it from the DB lookup).

---

## 3. Middleware: Org Context

### `internal/server/middleware.go`

Add org context key and helpers:

```go
const contextKeyOrgID contextKey = "org_id"

// OrgIDFromContext extracts the org_id from the context.
func OrgIDFromContext(ctx context.Context) uuid.UUID {
    if v, ok := ctx.Value(contextKeyOrgID).(uuid.UUID); ok {
        return v
    }
    return uuid.Nil
}
```

Add `statusWriter` Flusher implementation (fix for SSE):

```go
func (w *statusWriter) Flush() {
    if f, ok := w.ResponseWriter.(http.Flusher); ok {
        f.Flush()
    }
}
```

The `statusWriter` currently only has `WriteHeader`. Add `Flush` so SSE works through the middleware chain.

---

## 4. Storage Changes

Every storage method that touches a tenant-scoped table must add `orgID uuid.UUID` as a parameter and include it in queries.

### `internal/storage/agents.go`

**CreateAgent**: Add `org_id` to INSERT.

```go
func (db *DB) CreateAgent(ctx context.Context, agent model.Agent) (model.Agent, error) {
    // ... existing setup ...
    _, err := db.pool.Exec(ctx,
        `INSERT INTO agents (id, agent_id, org_id, name, role, api_key_hash, metadata, created_at, updated_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
        agent.ID, agent.AgentID, agent.OrgID, agent.Name, string(agent.Role),
        agent.APIKeyHash, agent.Metadata, agent.CreatedAt, agent.UpdatedAt,
    )
    // ...
}
```

**GetAgentByAgentID**: Add `orgID` parameter. Query changes to `WHERE agent_id = $1 AND org_id = $2`. Scan includes `org_id`.

**IMPORTANT exception**: `GetAgentByAgentID` is also used by `HandleAuthToken` for login, where org_id isn't known yet. Create a separate method:

```go
// GetAgentByAgentIDGlobal retrieves an agent by agent_id across all orgs.
// Used ONLY for authentication (token issuance) where org_id isn't known yet.
func (db *DB) GetAgentByAgentIDGlobal(ctx context.Context, agentID string) (model.Agent, error) {
    // Same as current GetAgentByAgentID but scans org_id too
}

// GetAgentByAgentID retrieves an agent by agent_id within an org.
func (db *DB) GetAgentByAgentID(ctx context.Context, orgID uuid.UUID, agentID string) (model.Agent, error) {
    // WHERE agent_id = $1 AND org_id = $2
}
```

**GetAgentByID**: Scan `org_id` into the struct. No orgID parameter needed (lookup by internal UUID is unambiguous).

**ListAgents**: Add `orgID` parameter. `WHERE org_id = $1`.

**CountAgents**: Add `orgID` parameter. `WHERE org_id = $1`.

### `internal/storage/runs.go`

**CreateRun**: Add `org_id` to the INSERT and the `model.AgentRun` being built. The `org_id` comes from the `model.CreateRunRequest` (add `OrgID uuid.UUID` to that struct too).

**GetRun**: Scan `org_id`. No orgID parameter (lookup by UUID).

**CompleteRun**: No change needed (updates by UUID).

**ListRunsByAgent**: Add `orgID` parameter. `WHERE agent_id = $1 AND org_id = $2`.

### `internal/storage/events.go`

**InsertEvents**: Add `org_id` to COPY columns list. Each event row includes `e.OrgID`.

**InsertEvent**: Add `org_id` to INSERT.

**GetEventsByRun**: No orgID parameter (lookup by run UUID). Scan `org_id`.

**GetEventsByRunBeforeTime**: Same — no orgID parameter.

**ReserveSequenceNums**: No change (sequence is global).

### `internal/storage/decisions.go`

**CreateDecision**: Add `org_id` to INSERT.

**GetDecision**: Scan `org_id`. No orgID parameter (lookup by UUID).

**ReviseDecision**: Scan `org_id` on the new decision. No orgID parameter.

**QueryDecisions**: Add `orgID uuid.UUID` parameter. `buildDecisionWhereClause` gets org_id as first condition.

**QueryDecisionsTemporal**: Add `orgID uuid.UUID` parameter.

**SearchDecisionsByEmbedding**: Add `orgID uuid.UUID` parameter.

**GetDecisionsByAgent**: Add `orgID uuid.UUID` parameter.

**buildDecisionWhereClause**: Change signature to accept orgID:

```go
func buildDecisionWhereClause(orgID uuid.UUID, f model.QueryFilters, startArgIdx int) (string, []any) {
    var conditions []string
    var args []any
    idx := startArgIdx

    // Org isolation is always the first condition.
    conditions = append(conditions, fmt.Sprintf("org_id = $%d", idx))
    args = append(args, orgID)
    idx++

    // ... rest of existing logic unchanged ...
}
```

**scanDecisions**: Scan `org_id` field (add `&d.OrgID` to the Scan call).

### `internal/storage/trace.go`

**CreateTraceParams**: Add `OrgID uuid.UUID`.

**CreateTraceTx**: Pass `org_id` through to all INSERTs:
- Run INSERT: add `org_id` column and `params.OrgID` value
- Decision INSERT: add `org_id` column and `params.OrgID` value
- Set `d.OrgID = params.OrgID` and `run.OrgID = params.OrgID`

### `internal/storage/grants.go`

**CreateGrant**: Add `org_id` to INSERT.

**GetGrant**: Scan `org_id`.

**HasAccess**: Add `orgID` parameter. `AND org_id = $5` condition.

**ListGrantsByGrantee**: Add `orgID` parameter. `AND org_id = $2` condition.

### `internal/storage/conflicts.go`

**ListConflicts**: Add `orgID` parameter. Filter by `org_id = $1`.

### `internal/storage/delete.go`

**DeleteAgentData**: Add `orgID` parameter. Include `AND org_id = $1` in all DELETE queries.

### New: `internal/storage/organizations.go`

Create this new file with org CRUD:

```go
package storage

// CreateOrganization inserts a new organization.
func (db *DB) CreateOrganization(ctx context.Context, org model.Organization) (model.Organization, error)

// GetOrganization retrieves an org by ID.
func (db *DB) GetOrganization(ctx context.Context, id uuid.UUID) (model.Organization, error)

// GetOrganizationBySlug retrieves an org by slug.
func (db *DB) GetOrganizationBySlug(ctx context.Context, slug string) (model.Organization, error)

// UpdateOrganization updates org fields.
func (db *DB) UpdateOrganization(ctx context.Context, org model.Organization) error

// IncrementUsage atomically increments the decision count for an org's current period.
// Returns the new count.
func (db *DB) IncrementUsage(ctx context.Context, orgID uuid.UUID, period string) (int, error)

// GetUsage returns the current period's usage for an org.
func (db *DB) GetUsage(ctx context.Context, orgID uuid.UUID, period string) (model.OrgUsage, error)
```

---

## 5. Service Layer Changes

### `internal/service/decisions/service.go`

All methods gain `orgID uuid.UUID` parameter:

```go
func (s *Service) Trace(ctx context.Context, orgID uuid.UUID, input TraceInput) (TraceResult, error)
func (s *Service) Check(ctx context.Context, orgID uuid.UUID, decisionType, query, agentID string, limit int) (model.CheckResponse, error)
func (s *Service) Search(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error)
func (s *Service) Query(ctx context.Context, orgID uuid.UUID, req model.QueryRequest) ([]model.Decision, int, error)
func (s *Service) Recent(ctx context.Context, orgID uuid.UUID, filters model.QueryFilters, limit int) ([]model.Decision, int, error)
```

**Trace**: Pass `orgID` to `CreateTraceTx` via `CreateTraceParams.OrgID`. Include `org_id` in the notify payload.

**Check**: Pass `orgID` to `SearchDecisionsByEmbedding`, `QueryDecisions`, and `ListConflicts`.

**Search**: Pass `orgID` to `SearchDecisionsByEmbedding`.

**Query**: Pass `orgID` to `QueryDecisions`.

**Recent**: Pass `orgID` to `QueryDecisions`.

### `internal/service/trace/buffer.go`

The `Append` method builds `model.AgentEvent` objects. These must carry `OrgID`. Add `orgID uuid.UUID` parameter to `Append`:

```go
func (b *Buffer) Append(ctx context.Context, runID uuid.UUID, agentID string, orgID uuid.UUID, events []model.EventInput) ([]model.AgentEvent, error)
```

Set `OrgID: orgID` on each created `model.AgentEvent`.

The COPY flush path calls `db.InsertEvents` which now includes `org_id` in the column list — this works automatically because the `AgentEvent.OrgID` field is populated.

---

## 6. Compilation Strategy

After making all these changes, many call sites will break because they don't pass `orgID` yet. That's expected — **Phase 3 fixes all call sites in handlers and MCP tools.**

To verify Phase 2 independently:
1. Run `go vet ./internal/model/... ./internal/auth/... ./internal/storage/... ./internal/service/...` — these packages should compile.
2. Handler and MCP packages will have compilation errors (expected, fixed in Phase 3).

## Files Changed

| File | Action |
|------|--------|
| `internal/model/agent.go` | Modify — add OrgID, new roles, RoleAtLeast |
| `internal/model/api.go` | Modify — Organization, OrgUsage types |
| `internal/auth/auth.go` | Modify — OrgID in Claims |
| `internal/storage/agents.go` | Modify — org_id in all queries |
| `internal/storage/runs.go` | Modify — org_id in all queries |
| `internal/storage/events.go` | Modify — org_id in COPY + queries |
| `internal/storage/decisions.go` | Modify — org_id in all queries + buildWhere |
| `internal/storage/trace.go` | Modify — org_id in CreateTraceTx |
| `internal/storage/grants.go` | Modify — org_id in all queries |
| `internal/storage/conflicts.go` | Modify — org_id filter |
| `internal/storage/delete.go` | Modify — org_id filter |
| `internal/storage/organizations.go` | **Create** — org CRUD + usage |
| `internal/service/decisions/service.go` | Modify — orgID parameter on all methods |
| `internal/service/trace/buffer.go` | Modify — orgID parameter on Append |
| `internal/server/middleware.go` | Modify — OrgIDFromContext, statusWriter.Flush |
