# Spec 05c: Handlers + Security Fixes

**Status**: Ready for implementation
**Phase**: 3 of 5 (Multi-Tenancy)
**Depends on**: Phase 2 (05b — model/auth/storage/service signatures updated)
**Blocks**: Phase 4 (05d), Phase 5 (05e)

## Goal

Wire org_id through all HTTP handlers and MCP tools. Fix the CRITICAL MCP authorization bypass (F-3/F-13). Fix SSE org-scoped filtering. Fix statusWriter Flusher. Add error logging to all handlers. Fix QueryDecisions valid_to IS NULL default.

After this phase, the application compiles, all existing tests pass (using the default org), and multi-tenant data isolation is enforced.

## Deliverables

1. All HTTP handlers extract `org_id` from claims and pass to service/storage
2. MCP tools extract claims from context and enforce auth + org boundaries
3. SSE events are filtered by org_id
4. `statusWriter` implements `http.Flusher`
5. All handler error paths log the error before returning 500
6. `QueryDecisions` defaults to `valid_to IS NULL` for non-temporal queries
7. `go test -race ./...` passes
8. Role checks updated for new roles (`org_owner`, `platform_admin`)

---

## 1. HTTP Handler Changes

### General Pattern

Every handler that calls a service or storage method must:

1. Extract claims: `claims := ClaimsFromContext(r.Context())`
2. Get org_id: `orgID := claims.OrgID`
3. Pass `orgID` to service/storage calls
4. Log errors before returning 500s

### `internal/server/handlers.go`

**HandleAuthToken**: Use `GetAgentByAgentIDGlobal` (not org-scoped, since org isn't known before login). The agent's `OrgID` is now populated from the DB, so `IssueToken(agent)` automatically includes it in the JWT.

```go
func (h *Handlers) HandleAuthToken(w http.ResponseWriter, r *http.Request) {
    // ... decode request ...

    agent, err := h.db.GetAgentByAgentIDGlobal(r.Context(), req.AgentID)
    if err != nil {
        writeError(w, r, http.StatusUnauthorized, model.ErrCodeUnauthorized, "invalid credentials")
        return
    }

    // ... verify API key ...

    // Check email verification (org must be verified for non-default orgs).
    if agent.OrgID != DefaultOrgID {
        org, err := h.db.GetOrganization(r.Context(), agent.OrgID)
        if err != nil {
            h.logger.Error("auth: get org for verification check", "error", err, "org_id", agent.OrgID)
            writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "internal error")
            return
        }
        if !org.EmailVerified {
            writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "email not verified")
            return
        }
    }

    token, expiresAt, err := h.jwtMgr.IssueToken(agent)
    // ...
}
```

Add `DefaultOrgID` constant:

```go
var DefaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000000")
```

**SeedAdmin**: Seed admin into default org. Set `agent.OrgID = DefaultOrgID`.

**HandleSubscribe**: Filter SSE events by org_id. See Section 4.

### `internal/server/handlers_decisions.go`

**HandleTrace**:

```go
func (h *Handlers) HandleTrace(w http.ResponseWriter, r *http.Request) {
    claims := ClaimsFromContext(r.Context())
    orgID := claims.OrgID

    // ... decode, validate ...

    // Role check: use RoleAtLeast for new role hierarchy
    if !model.RoleAtLeast(claims.Role, model.RoleAdmin) && req.AgentID != claims.AgentID {
        writeError(w, r, http.StatusForbidden, model.ErrCodeForbidden, "can only trace for your own agent_id")
        return
    }

    result, err := h.decisionSvc.Trace(r.Context(), orgID, decisions.TraceInput{...})
    if err != nil {
        h.logger.Error("trace failed", "error", err, "agent_id", claims.AgentID, "org_id", orgID)
        writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "failed to create trace")
        return
    }
    // ...
}
```

**HandleQuery**:

```go
func (h *Handlers) HandleQuery(w http.ResponseWriter, r *http.Request) {
    claims := ClaimsFromContext(r.Context())
    orgID := claims.OrgID

    // ... decode ...

    decisions, total, err := h.decisionSvc.Query(r.Context(), orgID, req)
    if err != nil {
        h.logger.Error("query failed", "error", err, "org_id", orgID)
        writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "query failed")
        return
    }

    // Access filter is now simpler: within an org, use the existing filter logic.
    // But org_id scoping already handles most of the isolation.
    decisions, err = filterDecisionsByAccess(r.Context(), h.db, claims, decisions)
    // ...
}
```

Apply the same `orgID` pattern to: **HandleTemporalQuery**, **HandleAgentHistory**, **HandleSearch**, **HandleCheck**, **HandleDecisionsRecent**, **HandleListConflicts**.

**HandleTemporalQuery**: Pass `orgID` to `db.QueryDecisionsTemporal(ctx, orgID, req)`.

**HandleAgentHistory**: Pass `orgID` to `db.GetDecisionsByAgent(ctx, orgID, agentID, ...)`.

**HandleSearch**: Pass `orgID` to `decisionSvc.Search(ctx, orgID, ...)`.

**HandleCheck**: Pass `orgID` to `decisionSvc.Check(ctx, orgID, ...)`.

**HandleDecisionsRecent**: Pass `orgID` to `decisionSvc.Recent(ctx, orgID, ...)`.

**HandleListConflicts**: Pass `orgID` to `db.ListConflicts(ctx, orgID, ...)`.

### `internal/server/handlers_runs.go`

**HandleCreateRun**: Pass `orgID` to `db.CreateRun`. Set `req.OrgID = claims.OrgID`.

**HandleAppendEvents**: Pass `orgID` to `buffer.Append(ctx, runID, run.AgentID, claims.OrgID, req.Events)`.

**HandleCompleteRun**: No change needed (operates by UUID). Add error logging.

**HandleGetRun**: Pass `orgID` to the `QueryDecisions` call.

### `internal/server/handlers_admin.go`

**HandleCreateAgent**: Set `agent.OrgID = claims.OrgID` before creating. Pass `orgID` to `CountAgents` for agent limit checks (Phase 5 adds the actual limit check).

**HandleListAgents**: Pass `orgID` to `db.ListAgents(ctx, claims.OrgID)`.

**HandleDeleteAgent**: Pass `orgID` to `db.DeleteAgentData(ctx, claims.OrgID, agentID)`.

**HandleCreateGrant**: Set `grant.OrgID = claims.OrgID`. Pass `orgID` to grant lookups.

**HandleDeleteGrant**: Verify grant's `OrgID` matches caller's `OrgID` before allowing deletion.

### `internal/server/handlers_export.go`

**HandleExportDecisions**: Pass `orgID` to `db.QueryDecisions`.

### `internal/server/authz.go`

Update `canAccessAgent`, `filterDecisionsByAccess`, `filterSearchResultsByAccess`, `filterConflictsByAccess` to use the new role hierarchy:

```go
func canAccessAgent(ctx context.Context, db *storage.DB, claims *auth.Claims, targetAgentID string) (bool, error) {
    if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
        return true, nil
    }
    // ... rest unchanged (org_owner also gets admin-level access via RoleAtLeast) ...
}
```

### `internal/server/server.go`

Update route role requirements to include new roles:

```go
adminOnly := requireRole(model.RolePlatformAdmin, model.RoleOrgOwner, model.RoleAdmin)
writeRoles := requireRole(model.RolePlatformAdmin, model.RoleOrgOwner, model.RoleAdmin, model.RoleAgent)
allRoles := requireRole(model.RolePlatformAdmin, model.RoleOrgOwner, model.RoleAdmin, model.RoleAgent, model.RoleReader)
```

Update `agentKeyFunc` to handle new roles:

```go
func agentKeyFunc(r *http.Request) string {
    claims := ClaimsFromContext(r.Context())
    if claims == nil {
        return ""
    }
    if model.RoleAtLeast(claims.Role, model.RoleAdmin) {
        return "" // exempt from rate limits
    }
    return claims.AgentID
}
```

---

## 2. MCP Authorization Fix (CRITICAL: F-3/F-13)

The MCP handlers currently bypass ALL authorization. They call `s.decisionSvc.Trace()` and `s.decisionSvc.Query()` without checking who's calling.

### Root Cause

The MCP `context.Context` passed to tool/resource handlers comes from the HTTP request that hit `/mcp`. The auth middleware has already validated the JWT and placed claims in context. But the MCP handlers never extract them.

### Fix: Extract Claims in MCP Handlers

The MCP server receives the `context.Context` from the HTTP handler. Claims are already in context. We need to extract them.

**Problem**: The `mcp-go` library's `CallToolRequest` handler receives a generic `context.Context`. The claims are placed by `authMiddleware` using `contextKeyClaims`. The MCP package needs to import from the `server` package to use `ClaimsFromContext` — but this creates a circular dependency (`server` imports `mcp`).

**Solution**: Move `ClaimsFromContext` and `OrgIDFromContext` to a shared package that both `server` and `mcp` can import. Create `internal/ctxutil/ctxutil.go`:

```go
package ctxutil

import (
    "context"

    "github.com/google/uuid"

    "github.com/ashita-ai/akashi/internal/auth"
)

type contextKey string

const (
    KeyRequestID contextKey = "request_id"
    KeyClaims    contextKey = "claims"
    KeyOrgID     contextKey = "org_id"
)

func ClaimsFromContext(ctx context.Context) *auth.Claims {
    if v, ok := ctx.Value(KeyClaims).(*auth.Claims); ok {
        return v
    }
    return nil
}

func OrgIDFromContext(ctx context.Context) uuid.UUID {
    if v, ok := ctx.Value(KeyOrgID).(uuid.UUID); ok {
        return v
    }
    return uuid.Nil
}

func RequestIDFromContext(ctx context.Context) string {
    if v, ok := ctx.Value(KeyRequestID).(string); ok {
        return v
    }
    return ""
}
```

Update `internal/server/middleware.go` to use `ctxutil.KeyClaims` etc. instead of local constants.

### MCP Tools: `internal/mcp/tools.go`

**handleTrace**: Extract claims and enforce authorization:

```go
func (s *Server) handleTrace(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
    claims := ctxutil.ClaimsFromContext(ctx)
    if claims == nil {
        return errorResult("unauthorized: no claims in context"), nil
    }

    // Enforce write permission.
    if !model.RoleAtLeast(claims.Role, model.RoleAgent) {
        return errorResult("forbidden: insufficient permissions to trace"), nil
    }

    agentID := request.GetString("agent_id", "")
    // ...

    // Enforce agent identity: can only trace as yourself unless admin+.
    if !model.RoleAtLeast(claims.Role, model.RoleAdmin) && agentID != claims.AgentID {
        return errorResult("forbidden: can only trace for your own agent_id"), nil
    }

    result, err := s.decisionSvc.Trace(ctx, claims.OrgID, decisions.TraceInput{...})
    // ...
}
```

**handleCheck**, **handleQuery**, **handleSearch**, **handleRecent**: Extract claims and pass `claims.OrgID`:

```go
func (s *Server) handleCheck(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
    claims := ctxutil.ClaimsFromContext(ctx)
    if claims == nil {
        return errorResult("unauthorized: no claims in context"), nil
    }

    // Read operations: any authenticated role is fine (already checked at HTTP level).
    // ...

    resp, err := s.decisionSvc.Check(ctx, claims.OrgID, decisionType, query, agentID, limit)
    // ...
}
```

### MCP Resources: `internal/mcp/resources.go`

Same pattern: extract claims, pass `claims.OrgID` to all DB queries:

```go
func (s *Server) handleSessionCurrent(ctx context.Context, request mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
    claims := ctxutil.ClaimsFromContext(ctx)
    if claims == nil {
        return nil, fmt.Errorf("mcp: unauthorized: no claims in context")
    }

    decisions, _, err := s.db.QueryDecisions(ctx, claims.OrgID, model.QueryRequest{
        OrderBy:  "valid_from",
        OrderDir: "desc",
        Limit:    10,
    })
    // ...
}
```

---

## 3. Error Logging

Every handler that returns a 500 must log the error. Pattern:

```go
if err != nil {
    h.logger.Error("handler_name failed",
        "error", err,
        "agent_id", claims.AgentID,
        "org_id", claims.OrgID,
        "request_id", RequestIDFromContext(r.Context()),
    )
    writeError(w, r, http.StatusInternalServerError, model.ErrCodeInternalError, "user-facing message")
    return
}
```

Apply to ALL error paths in: HandleTrace, HandleQuery, HandleTemporalQuery, HandleSearch, HandleCheck, HandleDecisionsRecent, HandleListConflicts, HandleCreateRun, HandleAppendEvents, HandleCompleteRun, HandleGetRun, HandleCreateAgent, HandleListAgents, HandleDeleteAgent, HandleCreateGrant, HandleDeleteGrant, HandleExportDecisions.

---

## 4. SSE Org-Scoped Filtering

### Broker Changes

The broker currently broadcasts raw notification payloads to all subscribers. After multi-tenancy, each subscriber must only receive events from their org.

**Option A (chosen)**: The broker sends the raw payload. The SSE handler (`HandleSubscribe`) parses the `org_id` from the payload and filters.

```go
func (h *Handlers) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
    claims := ClaimsFromContext(r.Context())
    orgID := claims.OrgID

    // ... flusher check, headers ...

    ch := h.broker.Subscribe()
    defer h.broker.Unsubscribe(ch)

    ctx := r.Context()
    for {
        select {
        case <-ctx.Done():
            return
        case event, ok := <-ch:
            if !ok {
                return
            }
            // Filter: only send events matching this subscriber's org.
            if !eventBelongsToOrg(event, orgID) {
                continue
            }
            if _, err := w.Write(event); err != nil {
                return
            }
            flusher.Flush()
        }
    }
}

// eventBelongsToOrg parses the SSE data payload and checks the org_id field.
func eventBelongsToOrg(event []byte, orgID uuid.UUID) bool {
    // SSE format: "event: <type>\ndata: <json>\n\n"
    // Extract the data line.
    dataPrefix := []byte("data: ")
    for _, line := range bytes.Split(event, []byte("\n")) {
        if bytes.HasPrefix(line, dataPrefix) {
            payload := line[len(dataPrefix):]
            var m map[string]any
            if json.Unmarshal(payload, &m) == nil {
                if oid, ok := m["org_id"].(string); ok {
                    return oid == orgID.String()
                }
            }
            break
        }
    }
    // If no org_id in payload, don't send (safe default).
    return false
}
```

### Notify Payload Update

In `internal/service/decisions/service.go` `Trace` method, the notify payload must include `org_id`:

```go
notifyPayload, err := json.Marshal(map[string]any{
    "decision_id": decision.ID,
    "agent_id":    input.AgentID,
    "org_id":      orgID,              // NEW
    "outcome":     input.Decision.Outcome,
})
```

---

## 5. valid_to IS NULL Default

In `internal/storage/decisions.go`, update `buildDecisionWhereClause`:

```go
func buildDecisionWhereClause(orgID uuid.UUID, f model.QueryFilters, startArgIdx int, temporal bool) (string, []any) {
    var conditions []string
    var args []any
    idx := startArgIdx

    // Org isolation is always the first condition.
    conditions = append(conditions, fmt.Sprintf("org_id = $%d", idx))
    args = append(args, orgID)
    idx++

    // For non-temporal queries, only return current (non-superseded) decisions.
    if !temporal {
        conditions = append(conditions, "valid_to IS NULL")
    }

    // ... rest of existing filter logic ...
}
```

Update callers:
- `QueryDecisions`: `buildDecisionWhereClause(orgID, req.Filters, 1, false)`
- `QueryDecisionsTemporal`: `buildDecisionWhereClause(orgID, req.Filters, 1, true)`
- `SearchDecisionsByEmbedding`: `buildDecisionWhereClause(orgID, filters, 2, false)`
- `GetDecisionsByAgent`: `buildDecisionWhereClause(orgID, filters, 1, false)`

---

## 6. New File

### `internal/ctxutil/ctxutil.go`

Create this file with the shared context key types and extraction functions (see Section 2).

---

## Files Changed

| File | Action |
|------|--------|
| `internal/ctxutil/ctxutil.go` | **Create** — shared context helpers |
| `internal/server/middleware.go` | Modify — use ctxutil keys, add Flush |
| `internal/server/handlers.go` | Modify — org_id extraction, DefaultOrgID, error logging |
| `internal/server/handlers_decisions.go` | Modify — pass orgID to all service calls, error logging |
| `internal/server/handlers_runs.go` | Modify — pass orgID, error logging |
| `internal/server/handlers_admin.go` | Modify — pass orgID, error logging |
| `internal/server/handlers_export.go` | Modify — pass orgID |
| `internal/server/authz.go` | Modify — use RoleAtLeast |
| `internal/server/server.go` | Modify — updated role lists |
| `internal/server/broker.go` | No change (filtering done in handler) |
| `internal/mcp/tools.go` | Modify — extract claims, enforce auth, pass orgID |
| `internal/mcp/resources.go` | Modify — extract claims, pass orgID |
| `internal/service/decisions/service.go` | Modify — org_id in notify payload |

## Success Criteria

1. `go build ./...` succeeds
2. `go test -race ./...` passes
3. MCP tools reject unauthenticated requests
4. MCP `akashi_trace` enforces agent identity (can't impersonate another agent)
5. All queries are org-scoped (verified by inspecting SQL in storage methods)
6. SSE only delivers events matching subscriber's org
7. No handler returns 500 without logging the error
8. `QueryDecisions` only returns `valid_to IS NULL` results by default
