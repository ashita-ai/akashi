# Akashi Technical Deep Dive

A comprehensive technical guide covering architecture, core concepts, and code organization for the Akashi decision trace layer.

---

## Part 1: Architecture

### System Overview

Akashi is a decision audit layer for multi-agent AI systems. It captures, stores, and queries the decisions made by AI agents—providing accountability, conflict detection, and semantic search across decision history.

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Client Layer                                 │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐               │
│  │   Go SDK     │  │  Python SDK  │  │   MCP Client │               │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘               │
└─────────┼─────────────────┼─────────────────┼───────────────────────┘
          │                 │                 │
          ▼                 ▼                 ▼
┌─────────────────────────────────────────────────────────────────────┐
│                       HTTP/MCP Layer                                 │
│  ┌────────────────────────────────────────────────────────────────┐ │
│  │                  net/http Server (Go 1.25)                      │ │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────────────┐  │ │
│  │  │  /v1/*   │  │   /mcp   │  │ /health  │  │  /v1/subscribe │  │ │
│  │  │ REST API │  │ MCP HTTP │  │ readiness│  │     SSE        │  │ │
│  │  └────┬─────┘  └────┬─────┘  └──────────┘  └───────┬────────┘  │ │
│  └───────┼─────────────┼──────────────────────────────┼───────────┘ │
└──────────┼─────────────┼──────────────────────────────┼─────────────┘
           │             │                              │
           ▼             ▼                              ▼
┌─────────────────────────────────────────────────────────────────────┐
│                      Service Layer                                   │
│  ┌────────────────┐  ┌────────────────┐  ┌────────────────────────┐ │
│  │ decisions.Svc  │  │  trace.Buffer  │  │    server.Broker       │ │
│  │ (shared logic) │  │ (COPY ingestion)│  │  (SSE fan-out)        │ │
│  └───────┬────────┘  └───────┬────────┘  └───────────┬────────────┘ │
│          │                   │                       │              │
│  ┌───────┴───────────────────┴───────────────────────┘              │
│  │                                                                  │
│  │  ┌────────────────┐  ┌────────────────┐                          │
│  │  │embedding.Prov. │  │ quality.Score  │                          │
│  │  │Ollama/OpenAI   │  │ (completeness) │                          │
│  │  └────────────────┘  └────────────────┘                          │
└──┼──────────────────────────────────────────────────────────────────┘
   │
   ▼
┌─────────────────────────────────────────────────────────────────────┐
│                      Storage Layer                                   │
│  ┌────────────────────────────────────────────────────────────────┐ │
│  │                     storage.DB                                  │ │
│  │  ┌─────────────────────┐  ┌─────────────────────────────────┐  │ │
│  │  │     pgxpool.Pool    │  │      pgx.Conn (dedicated)       │  │ │
│  │  │   (via PgBouncer)   │  │    (LISTEN/NOTIFY direct)       │  │ │
│  │  └─────────┬───────────┘  └───────────────┬─────────────────┘  │ │
│  └────────────┼──────────────────────────────┼────────────────────┘ │
└───────────────┼──────────────────────────────┼──────────────────────┘
                │                              │
                ▼                              ▼
┌─────────────────────────────────────────────────────────────────────┐
│                    PostgreSQL 18                                     │
│  ┌───────────────┐  ┌────────────────┐  ┌─────────────────────────┐ │
│  │   pgvector    │  │  TimescaleDB   │  │    LISTEN/NOTIFY        │ │
│  │ (similarity)  │  │ (time-series)  │  │  (real-time events)     │ │
│  └───────────────┘  └────────────────┘  └─────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────┘
```

### Component Interactions

#### 1. Request Flow: Trace Ingestion

When an agent records a decision via `POST /v1/trace`:

```
HTTP Request
     │
     ▼
┌────────────────┐
│ authMiddleware │──▶ JWT validation via auth.JWTManager
└───────┬────────┘
        ▼
┌────────────────┐
│ requireRole    │──▶ RBAC check (admin|agent only)
└───────┬────────┘
        ▼
┌────────────────┐
│ HandleTrace    │──▶ handlers_decisions.go:17
└───────┬────────┘
        ▼
┌────────────────┐    ┌──────────────────┐
│ decisionSvc.   │──▶│ 1. embedder.Embed │ (Ollama/OpenAI)
│ Trace()        │    │ 2. quality.Score  │ (completeness)
│                │    │ 3. db.CreateTrace │ (transactional)
│                │    │ 4. db.Notify      │ (LISTEN/NOTIFY)
└───────┬────────┘    └──────────────────┘
        ▼
    JSON Response
```

#### 2. Event Buffering

High-throughput event ingestion uses an in-memory buffer with `COPY` protocol:

```
POST /v1/runs/{run_id}/events
         │
         ▼
┌─────────────────────┐
│  trace.Buffer       │
│  ┌───────────────┐  │
│  │ []AgentEvent  │  │  ← events accumulate
│  │  (in-memory)  │  │
│  └───────┬───────┘  │
│          │          │
│  flush triggers:    │
│  • maxSize reached  │
│  • flushTimeout     │
│  • ctx cancellation │
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ db.InsertEvents()   │
│ using pgx.CopyFrom  │──▶ PostgreSQL COPY protocol
└─────────────────────┘    (10-100x faster than INSERT)
```

Key design decisions:
- **Backpressure**: Buffer rejects writes when at capacity (100k events)
- **Sequence numbers**: Reserved atomically from Postgres SEQUENCE before buffering
- **Retry on failure**: Failed flushes requeue events (respecting capacity)

#### 3. Real-Time Subscriptions

SSE streaming uses PostgreSQL LISTEN/NOTIFY with a fan-out broker:

```
PostgreSQL                    Akashi Server                   Clients
    │                              │                             │
    │  NOTIFY decisions            │                             │
    │  ─────────────────────────▶  │                             │
    │                              │                             │
    │                    ┌─────────┴─────────┐                   │
    │                    │  server.Broker    │                   │
    │                    │  ┌─────────────┐  │                   │
    │                    │  │ subscribers │  │                   │
    │                    │  │ map[ch]bool │  │                   │
    │                    │  └──────┬──────┘  │                   │
    │                    │         │         │                   │
    │                    │    broadcast()    │                   │
    │                    └─────────┬─────────┘                   │
    │                              │                             │
    │                              │  SSE: event: decisions      │
    │                              │  ─────────────────────────▶ │
    │                              │                             │
```

**Dual connection design**:
- Pool connection (via PgBouncer): Standard queries
- Direct connection: LISTEN/NOTIFY (PgBouncer doesn't support it)

#### 4. Authorization Model

Three-tier RBAC with fine-grained access grants:

```
┌─────────────────────────────────────────────────────────────┐
│                      RBAC Roles                              │
├─────────────┬───────────────────────────────────────────────┤
│   admin     │ Full access to everything                      │
├─────────────┼───────────────────────────────────────────────┤
│   agent     │ Own data + explicitly granted access           │
├─────────────┼───────────────────────────────────────────────┤
│   reader    │ Only explicitly granted access                 │
└─────────────┴───────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│                   Access Grants                              │
│  agent-A grants agent-B read access to agent_traces         │
│                                                              │
│  ┌──────────┐        ┌──────────┐       ┌──────────┐        │
│  │ agent-A  │──grant─│ agent-B  │──can──│ read A's │        │
│  │ (owner)  │        │ (grantee)│  read │ traces   │        │
│  └──────────┘        └──────────┘       └──────────┘        │
└─────────────────────────────────────────────────────────────┘
```

Read path filtering (authz.go):
```
filterDecisionsByAccess(decisions) {
    for each decision:
        if admin: allow
        if agent && own data: allow
        if has grant for agent_traces: allow
        else: filter out
}
```

---

## Part 2: Core Concepts

### Bi-Temporal Data Model

Every decision has two time dimensions:

```
┌───────────────────────────────────────────────────────────────────┐
│                    Bi-Temporal Columns                             │
├────────────────────┬──────────────────────────────────────────────┤
│ valid_from         │ When the decision became effective            │
│ valid_to           │ When the decision was superseded (nullable)   │
├────────────────────┼──────────────────────────────────────────────┤
│ transaction_time   │ When the decision was recorded in the system  │
└────────────────────┴──────────────────────────────────────────────┘

Example: Point-in-time query "What decisions were active on Jan 15?"

SELECT * FROM decisions
WHERE transaction_time <= '2026-01-15'      -- known by that date
  AND (valid_to IS NULL OR valid_to > '2026-01-15')  -- still valid
```

**Revision pattern** (storage/decisions.go:89):
```go
func ReviseDecision(originalID, revised Decision) {
    tx.Begin()

    // 1. Invalidate the original by setting valid_to
    UPDATE decisions SET valid_to = now() WHERE id = originalID

    // 2. Insert the revised decision with valid_from = now()
    INSERT decisions (..., valid_from = now(), ...)

    tx.Commit()
}
```

### Event Sourcing

Raw events are the source of truth; decisions are derived views:

```
┌────────────────────────────────────────────────────────────────────┐
│                      agent_events table                             │
│  ┌──────────────┬──────────────┬───────────────┬─────────────────┐ │
│  │ sequence_num │  event_type  │   occurred_at │     payload     │ │
│  ├──────────────┼──────────────┼───────────────┼─────────────────┤ │
│  │     1        │ run_started  │  10:00:00     │ {agent: "a1"}   │ │
│  │     2        │ context_add  │  10:00:01     │ {data: "..."}   │ │
│  │     3        │ decision_made│  10:00:02     │ {outcome: "X"}  │ │
│  │     4        │ run_completed│  10:00:03     │ {status: "ok"}  │ │
│  └──────────────┴──────────────┴───────────────┴─────────────────┘ │
│                              │                                      │
│                              ▼                                      │
│                    Derived: decisions table                         │
│                    (materialized from decision_made events)         │
└────────────────────────────────────────────────────────────────────┘
```

**Sequence number allocation** (storage/events.go:18):
```go
// Globally unique, gap-safe sequence from Postgres SEQUENCE
func ReserveSequenceNums(count int) []int64 {
    SELECT nextval('event_sequence_num_seq')
    FROM generate_series(1, count)
}
```

### Vector Embeddings & Semantic Search

Decisions are embedded for similarity search using pgvector:

```
┌────────────────────────────────────────────────────────────────────┐
│               Embedding Generation                                  │
│                                                                     │
│  Decision:                                                          │
│    decision_type: "code_review"                                     │
│    outcome: "approve"                                               │
│    reasoning: "All tests pass, code is clean"                       │
│                       │                                             │
│                       ▼                                             │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │ Text: "code_review: approve All tests pass, code is clean"  │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                       │                                             │
│                       ▼                                             │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │ Ollama/OpenAI API → vector(1024)                            │   │
│  │ [0.023, -0.114, 0.891, ...]                                 │   │
│  └─────────────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────────────┘
```

**Weighted relevance scoring** (storage/decisions.go:269):
```sql
SELECT *,
  (1 - (embedding <=> $1))                    -- cosine similarity (60%)
  * (0.6 + 0.3 * COALESCE(quality_score, 0))  -- quality bonus (up to 30%)
  * (1.0 / (1.0 + age_days / 90.0))           -- recency decay (to 50% at 90d)
  AS relevance
FROM decisions
ORDER BY relevance DESC
```

**Provider selection** (main.go:168):
```go
switch cfg.EmbeddingProvider {
case "ollama":   return NewOllamaProvider(...)   // On-premises, no cost
case "openai":   return NewOpenAIProvider(...)   // Cloud API
case "noop":     return NewNoopProvider(...)     // Zero vectors (disabled)
default:         // Auto-detect: Ollama → OpenAI → Noop
}
```

### JWT Authentication with Ed25519

Ed25519 (EdDSA) for compact, fast, secure tokens:

```
┌────────────────────────────────────────────────────────────────────┐
│                    JWT Claims Structure                             │
├────────────────────────────────────────────────────────────────────┤
│  {                                                                  │
│    "sub": "550e8400-e29b-41d4-a716-446655440000",  // Agent UUID   │
│    "iss": "akashi",                                                │
│    "iat": 1706745600,                                              │
│    "exp": 1706832000,                              // 24h default  │
│    "jti": "unique-token-id",                                       │
│    "agent_id": "agent-alpha",                      // Akashi field │
│    "role": "agent"                                 // Akashi field │
│  }                                                                  │
└────────────────────────────────────────────────────────────────────┘
```

**Key management** (auth/auth.go:37):
```go
func NewJWTManager(privateKeyPath, publicKeyPath, expiration) {
    if paths == "" {
        // Dev mode: generate ephemeral key pair
        pub, priv, _ := ed25519.GenerateKey(rand.Reader)
    } else {
        // Production: load from PEM files
        priv := x509.ParsePKCS8PrivateKey(...)
        pub := x509.ParsePKIXPublicKey(...)
    }
}
```

### API Key Hashing with Argon2id

API keys are hashed with Argon2id (memory-hard, resistant to GPU attacks):

```go
// auth/hash.go
func HashAPIKey(key string) (string, error) {
    // Argon2id with sensible defaults:
    // time=1, memory=64MB, threads=4, keyLen=32
    return argon2id.CreateHash(key, argon2id.DefaultParams)
}

func VerifyAPIKey(key, hash string) (bool, error) {
    return argon2id.ComparePasswordAndHash(key, hash)
}
```

### Go Patterns Used

#### 1. Struct-Based Configuration
```go
// config/config.go loads from environment
cfg, _ := config.Load()
srv := server.New(server.ServerConfig{DB: db, JWTMgr: jwtMgr, DecisionSvc: decisionSvc, ...})
```

#### 2. Context Propagation
```go
// Claims flow through context
ctx := context.WithValue(r.Context(), contextKeyClaims, claims)
// Later:
claims := ClaimsFromContext(r.Context())
```

#### 3. Interface-Based Abstraction
```go
// embedding.Provider interface
type Provider interface {
    Embed(ctx, text) (pgvector.Vector, error)
    EmbedBatch(ctx, texts) ([]pgvector.Vector, error)
    Dimensions() int
}

// Implementations: OpenAIProvider, OllamaProvider, NoopProvider
```

#### 4. Middleware Chain
```go
// server.go:122-128 - Applied inside-out
handler = authMiddleware(jwtMgr, handler)      // Innermost (runs last)
handler = loggingMiddleware(logger, handler)
handler = tracingMiddleware(handler)
handler = securityHeadersMiddleware(handler)
handler = requestIDMiddleware(handler)          // Outermost (runs first)
```

#### 5. Graceful Shutdown
```go
// main.go:45-46
ctx, cancel := signal.NotifyContext(context.Background(),
    syscall.SIGINT, syscall.SIGTERM)
defer cancel()

// main.go:152-158 - Shutdown sequence
buf.Drain(shutdownCtx)           // Flush pending events
srv.Shutdown(shutdownCtx)        // Stop accepting requests
```

#### 6. Fan-Out Pattern (Broker)
```go
// broker.go:84-97
func (b *Broker) broadcast(event []byte) {
    b.mu.RLock()
    defer b.mu.RUnlock()

    for ch := range b.subscribers {
        select {
        case ch <- event:          // Non-blocking send
        default:
            b.logger.Warn("dropped event for slow subscriber")
        }
    }
}
```

---

## Part 3: Code Organization

### Directory Structure

```
akashi/
├── cmd/akashi/
│   └── main.go              # Entry point, wiring, graceful shutdown
│
├── internal/
│   ├── auth/
│   │   ├── auth.go          # JWT manager (Ed25519)
│   │   └── hash.go          # Argon2id API key hashing
│   │
│   ├── config/
│   │   └── config.go        # Environment-based configuration
│   │
│   ├── model/
│   │   ├── agent.go         # Agent, AccessGrant, RBAC types
│   │   ├── decision.go      # Decision, Alternative, Evidence
│   │   ├── event.go         # AgentEvent, EventType
│   │   ├── run.go           # Run, RunStatus
│   │   ├── query.go         # QueryRequest, SearchRequest
│   │   └── api.go           # API request/response types
│   │
│   ├── server/
│   │   ├── server.go        # HTTP server, route registration
│   │   ├── middleware.go    # Auth, logging, tracing, security
│   │   ├── authz.go         # Authorization helpers (filterByAccess)
│   │   ├── handlers.go      # Core handlers (auth, health, SSE)
│   │   ├── handlers_*.go    # Domain-specific handlers
│   │   └── broker.go        # SSE fan-out broker
│   │
│   ├── service/
│   │   ├── decisions/
│   │   │   └── service.go   # Shared business logic
│   │   ├── embedding/
│   │   │   ├── embedding.go # Provider interface, OpenAI impl
│   │   │   └── ollama.go    # Ollama implementation
│   │   ├── quality/
│   │   │   └── quality.go   # Trace completeness scoring
│   │   └── trace/
│   │       └── buffer.go    # Event buffering, COPY ingestion
│   │
│   ├── storage/
│   │   ├── pool.go          # DB wrapper, dual connections
│   │   ├── decisions.go     # Decision CRUD, semantic search
│   │   ├── events.go        # Event ingestion (COPY protocol)
│   │   ├── runs.go          # Run lifecycle
│   │   ├── agents.go        # Agent management
│   │   ├── grants.go        # Access grants
│   │   ├── conflicts.go     # Conflict detection
│   │   └── notify.go        # LISTEN/NOTIFY helpers
│   │
│   ├── mcp/
│   │   ├── mcp.go           # MCP server setup
│   │   ├── tools.go         # MCP tool definitions
│   │   ├── resources.go     # MCP resource definitions
│   │   └── prompts.go       # MCP prompt templates
│   │
│   └── telemetry/
│       └── telemetry.go     # OpenTelemetry setup
│
├── migrations/              # SQL migrations (Atlas-managed)
│
└── sdk/
    └── go/akashi/           # Go client SDK
```

### Key File Deep Dives

#### `internal/server/server.go` (Lines 37-141)

The HTTP server constructor wires everything together:

```go
func New(db, jwtMgr, decisionSvc, buffer, broker, ...) *Server {
    h := NewHandlers(...)

    mux := http.NewServeMux()

    // Route registration with middleware composition
    mux.Handle("POST /auth/token", http.HandlerFunc(h.HandleAuthToken))
    mux.Handle("POST /v1/runs", writeRoles(http.HandlerFunc(h.HandleCreateRun)))
    // ... more routes

    // Middleware chain (applied inside-out)
    handler = authMiddleware(jwtMgr, handler)
    handler = loggingMiddleware(logger, handler)
    // ...
}
```

#### `internal/service/decisions/service.go` (Lines 58-146)

The `Trace` method shows the complete decision recording flow:

```go
func (s *Service) Trace(ctx, input TraceInput) (TraceResult, error) {
    // 1. Generate embedding (outside tx - may call external API)
    embText := input.Decision.DecisionType + ": " + input.Decision.Outcome
    decisionEmb, err := s.embedder.Embed(ctx, embText)

    // 2. Compute quality score
    qualityScore := quality.Score(input.Decision)

    // 3. Build alternatives
    alts := make([]model.Alternative, len(input.Decision.Alternatives))

    // 4. Build evidence with embeddings
    evs := make([]model.Evidence, len(input.Decision.Evidence))
    for i, e := range input.Decision.Evidence {
        evEmb, _ := s.embedder.Embed(ctx, e.Content)
        evs[i] = model.Evidence{..., Embedding: &evEmb}
    }

    // 5. Transactional write (all or nothing)
    run, decision, err := s.db.CreateTraceTx(ctx, CreateTraceParams{...})

    // 6. Notify subscribers (after commit)
    s.db.Notify(ctx, ChannelDecisions, payload)

    return TraceResult{RunID: run.ID, DecisionID: decision.ID}
}
```

#### `internal/service/trace/buffer.go` (Lines 21-163)

The event buffer implements backpressure and batch flushing:

```go
type Buffer struct {
    db           *storage.DB
    maxSize      int           // Trigger flush when reached
    flushTimeout time.Duration // Flush after this duration
    events       []model.AgentEvent
    flushCh      chan struct{} // Signal immediate flush
    done         chan struct{} // Signal shutdown complete
}

func (b *Buffer) Append(ctx, runID, agentID, inputs) ([]AgentEvent, error) {
    b.mu.Lock()
    defer b.mu.Unlock()

    // Backpressure: reject when buffer is full
    if len(b.events)+len(inputs) > maxBufferCapacity {
        return nil, fmt.Errorf("buffer at capacity")
    }

    // Reserve sequence numbers atomically
    seqNums, err := b.db.ReserveSequenceNums(ctx, len(inputs))

    // Append to buffer
    b.events = append(b.events, events...)

    // Trigger flush if threshold reached
    if len(b.events) >= b.maxSize {
        select {
        case b.flushCh <- struct{}{}: // Non-blocking
        default:
        }
    }
}

func (b *Buffer) flushLoop(ctx context.Context) {
    ticker := time.NewTicker(b.flushTimeout)
    for {
        select {
        case <-ctx.Done():
            b.flush(context.Background())  // Final flush
            close(b.done)
            return
        case <-ticker.C:
            b.flush(ctx)
        case <-b.flushCh:
            b.flush(ctx)
        }
    }
}
```

#### `internal/storage/pool.go` (Lines 21-186)

Dual-connection design for PgBouncer compatibility:

```go
type DB struct {
    pool       *pgxpool.Pool  // Via PgBouncer: queries
    notifyConn *pgx.Conn      // Direct: LISTEN/NOTIFY
    notifyDSN  string
    listenChannels []string   // Track for reconnection
}

func New(ctx, poolDSN, notifyDSN, logger) (*DB, error) {
    poolCfg, _ := pgxpool.ParseConfig(poolDSN)

    // Register pgvector types on each connection
    poolCfg.AfterConnect = func(ctx, conn) error {
        pgxvector.RegisterTypes(ctx, conn)
        return nil
    }

    pool, _ := pgxpool.NewWithConfig(ctx, poolCfg)

    // Separate connection for LISTEN/NOTIFY (PgBouncer incompatible)
    var notifyConn *pgx.Conn
    if notifyDSN != "" {
        notifyConn, _ = pgx.Connect(ctx, notifyDSN)
    }

    return &DB{pool: pool, notifyConn: notifyConn, ...}
}

func (db *DB) reconnectNotify(ctx context.Context) error {
    // Exponential backoff with jitter
    for attempt := range maxRetries {
        backoff := 500ms * 2^attempt + random(backoff/2)
        conn, err := pgx.Connect(ctx, db.notifyDSN)
        if err != nil { continue }

        // Re-subscribe to tracked channels
        for _, ch := range db.listenChannels {
            conn.Exec(ctx, "LISTEN "+ch)
        }
        db.notifyConn = conn
        return nil
    }
}
```

#### `internal/server/authz.go` (Lines 22-135)

Authorization helpers with caching:

```go
func canAccessAgent(ctx, db, claims, targetAgentID) (bool, error) {
    if claims.Role == RoleAdmin {
        return true, nil
    }

    if claims.Role == RoleAgent && claims.AgentID == targetAgentID {
        return true, nil  // Own data
    }

    // Check for explicit grant
    callerUUID, err := uuid.Parse(claims.Subject)
    if err != nil {
        slog.Warn("authz: malformed JWT subject, denying")
        return false, nil
    }

    return db.HasAccess(ctx, callerUUID, "agent_traces", targetAgentID, "read")
}

func filterDecisionsByAccess(ctx, db, claims, decisions) ([]Decision, error) {
    if claims.Role == RoleAdmin {
        return decisions, nil
    }

    // Cache access checks to avoid N+1 queries
    accessCache := make(map[string]bool)

    var allowed []Decision
    for _, d := range decisions {
        ok, cached := accessCache[d.AgentID]
        if !cached {
            ok, _ = canAccessAgent(ctx, db, claims, d.AgentID)
            accessCache[d.AgentID] = ok
        }
        if ok {
            allowed = append(allowed, d)
        }
    }
    return allowed, nil
}
```

---

## Quick Reference

### Database Tables

| Table | Purpose |
|-------|---------|
| `agents` | Agent identities with roles |
| `agent_events` | Immutable event log |
| `agent_runs` | Agent execution sessions |
| `decisions` | First-class decisions with bi-temporal columns |
| `alternatives` | Options considered for each decision |
| `evidence` | Supporting data for decisions |
| `access_grants` | Fine-grained access permissions |
| `scored_conflicts` | Semantic conflict pairs (event-driven, see docs/decisions.md) |

### API Endpoints

| Endpoint | Role(s) | Purpose |
|----------|---------|---------|
| `POST /auth/token` | - | Exchange API key for JWT |
| `POST /v1/runs` | admin, agent | Start agent run |
| `POST /v1/runs/{run_id}/events` | admin, agent | Append events |
| `POST /v1/trace` | admin, agent | Record decision (convenience) |
| `POST /v1/query` | all | Structured decision query |
| `POST /v1/search` | all | Semantic similarity search |
| `POST /v1/check` | all | Precedent lookup |
| `GET /v1/subscribe` | all | SSE real-time events |
| `GET /v1/export/decisions` | admin | Bulk export (NDJSON) |

### Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `DATABASE_URL` | - | PgBouncer connection string |
| `NOTIFY_URL` | - | Direct Postgres (LISTEN/NOTIFY) |
| `AKASHI_EMBEDDING_PROVIDER` | auto | ollama/openai/noop/auto |
| `OLLAMA_URL` | http://localhost:11434 | Ollama server |
| `OPENAI_API_KEY` | - | OpenAI API key |
| `AKASHI_ADMIN_API_KEY` | - | Initial admin seed |
