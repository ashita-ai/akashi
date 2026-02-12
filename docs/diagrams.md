# Akashi System Diagrams

Mermaid diagrams documenting the core data flows, authentication model, and schema
of the Akashi decision trace layer. These render natively on GitHub.

---

## 1. Write Path (Trace Ingestion)

A client records a decision by posting to `/v1/trace`. The request passes through
the middleware chain (request ID, security headers, CORS, tracing, logging, JWT
auth, panic recovery), then through `requireRole(agent)` authorization before
reaching the handler. The handler delegates to the shared `decisions.Service`,
which orchestrates embedding generation, quality scoring, and
an atomic transactional write. Notification happens after the
transaction commits and is non-fatal on failure.

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant MW as Middleware Chain
    participant RR as requireRole(agent)
    participant H as HandleTrace
    participant DS as decisionSvc.Trace()
    participant E as embedder.Embed()
    participant Q as quality.Score()
    participant DB as storage.CreateTraceTx()
    participant N as storage.Notify()

    C->>MW: POST /v1/trace (Bearer JWT)
    MW->>MW: requestID, securityHeaders, CORS, tracing, logging
    MW->>MW: authMiddleware: Ed25519 verify, check expiry/issuer/subject
    MW->>RR: authenticated request with claims in context
    RR->>RR: claims.Role >= agent
    RR->>H: authorized request
    H->>H: decodeJSON (MaxBytesReader), validate fields
    H->>H: verify agent_id exists in caller's org
    H->>DS: Trace(ctx, orgID, TraceInput)
    DS->>E: Embed(decisionType + outcome + reasoning)
    E-->>DS: vector(1024) or warning
    DS->>E: Embed(evidence[i].content) for each evidence
    E-->>DS: evidence vectors
    DS->>Q: Score(decision)
    Q-->>DS: quality_score (0.0-1.0)
    DS->>DB: CreateTraceTx(params)

    rect rgb(235, 245, 255)
        Note over DB: Single PostgreSQL Transaction
        DB->>DB: INSERT INTO agent_runs (status=running)
        DB->>DB: INSERT INTO decisions (embedding, quality_score)
        DB->>DB: COPY INTO alternatives
        DB->>DB: COPY INTO evidence
        DB->>DB: INSERT INTO search_outbox (if embedding present)
        DB->>DB: UPDATE agent_runs SET status=completed
        DB->>DB: COMMIT
    end

    DB-->>DS: (run, decision)
    DS->>N: pg_notify('akashi_decisions', payload)
    N-->>DS: OK (non-fatal on error)
    DS-->>H: TraceResult{RunID, DecisionID, EventCount}
    H-->>C: 201 Created {run_id, decision_id, event_count}
```

---

## 2. Read Path

Three query endpoints serve different access patterns. All three enforce org-scoped
data isolation via `org_id` WHERE clauses and apply role-based access filtering
after the query returns: admins see all decisions within their org, agents see
their own decisions plus those covered by access grants.

```mermaid
flowchart TD
    Q1[POST /v1/query] --> PARSE1[Parse QueryRequest filters:<br/>agent_id, decision_type,<br/>confidence_min, time_range]
    PARSE1 --> SQL1[SQL WHERE org_id = ? <br/>AND valid_to IS NULL<br/>+ filter predicates]
    SQL1 --> PAG[ORDER BY + LIMIT/OFFSET pagination]
    PAG --> FILTER

    Q2[POST /v1/search] --> CHK{Qdrant configured<br/>AND healthy?}
    CHK -- Yes --> EMB[embedder.Embed query text]
    EMB --> ANN[Qdrant ANN search<br/>cosine similarity<br/>org_id payload filter]
    ANN --> HYD[Hydrate from PostgreSQL<br/>GetDecisionsByIDs]
    HYD --> RESCORE["ReScore:<br/>relevance = similarity<br/>* (0.6 + 0.3 * quality_score)<br/>* 1/(1 + age_days/90)"]
    RESCORE --> FILTER
    CHK -- No --> ILIKE[PostgreSQL ILIKE fallback<br/>keyword match on outcome,<br/>reasoning, decision_type]
    ILIKE --> RESCORE2["SQL relevance:<br/>(0.6 + 0.3 * quality_score)<br/>* 1/(1 + age_days/90)"]
    RESCORE2 --> FILTER

    Q3[POST /v1/query/temporal] --> PARSE3[Parse TemporalQueryRequest<br/>as_of timestamp + filters]
    PARSE3 --> SQL3["SQL WHERE org_id = ?<br/>AND transaction_time <= as_of<br/>AND (valid_to IS NULL<br/>&nbsp;&nbsp;&nbsp;&nbsp;OR valid_to > as_of)<br/>+ filter predicates"]
    SQL3 --> FILTER

    FILTER[filterDecisionsByAccess<br/>admin: sees all in org<br/>agent: own + granted] --> RESP[JSON Response]

    style Q1 fill:#f0f4ff,stroke:#4a6fa5
    style Q2 fill:#f0f4ff,stroke:#4a6fa5
    style Q3 fill:#f0f4ff,stroke:#4a6fa5
    style FILTER fill:#fff3e0,stroke:#e65100
    style RESP fill:#e8f5e9,stroke:#2e7d32
```

---

## 3. Outbox Sync (Qdrant)

The search outbox guarantees at-least-once delivery of decision embeddings to
Qdrant. The outbox row is inserted inside the same transaction that writes the
decision, so the two are atomically consistent. A background worker polls the
outbox on a configurable interval, processes entries in batches using
`SELECT ... FOR UPDATE SKIP LOCKED` for concurrency safety, and uses exponential
backoff on failure. Entries that exceed 10 attempts are logged as dead letters
and cleaned up after 7 days.

```mermaid
sequenceDiagram
    autonumber
    participant TX as CreateTraceTx
    participant OB as search_outbox table
    participant W as OutboxWorker (background)
    participant PG as PostgreSQL
    participant QD as Qdrant

    TX->>OB: INSERT search_outbox (decision_id, org_id, 'upsert')<br/>inside same transaction as decision
    TX->>TX: COMMIT (decision + outbox atomically)

    loop Every pollInterval (ticker)
        W->>PG: BEGIN
        W->>PG: SELECT id, decision_id, org_id, operation, attempts<br/>FROM search_outbox<br/>WHERE (locked_until IS NULL OR locked_until < now())<br/>AND attempts < 10<br/>ORDER BY created_at ASC LIMIT batchSize<br/>FOR UPDATE SKIP LOCKED
        PG-->>W: batch of outbox entries
        W->>PG: UPDATE search_outbox SET locked_until = now() + 60s<br/>WHERE id = ANY(batch_ids)
        W->>PG: COMMIT (lock acquired)

        W->>PG: SELECT id, org_id, agent_id, decision_type,<br/>confidence, quality_score, valid_from, embedding<br/>FROM decisions WHERE id = ANY(decision_ids)<br/>AND valid_to IS NULL AND embedding IS NOT NULL
        PG-->>W: DecisionForIndex rows

        alt Qdrant upsert succeeds
            W->>QD: Upsert points (id, vector, payload:{org_id, agent_id, ...})
            QD-->>W: OK
            W->>PG: DELETE FROM search_outbox WHERE id = ANY(batch_ids)
        else Qdrant upsert fails
            W->>PG: UPDATE search_outbox<br/>SET attempts = attempts + 1,<br/>last_error = '...',<br/>locked_until = now() + LEAST(2^(attempts+1), 300)s
            Note over W: Exponential backoff, capped at 5 minutes
        end

        alt attempts >= 10
            Note over W: Log dead-letter warning<br/>Periodic cleanup deletes entries<br/>older than 7 days
        end
    end
```

---

## 4. Authentication Flow

Two authentication paths serve different use cases. **Path A** exchanges long-lived
API key credentials for a short-lived JWT. The server iterates over all agents
matching the `agent_id` (which is unique per org, not globally) and uses
timing-safe Argon2id comparison. A dummy verify runs when no hash is found to
prevent timing side-channels. **Path B** validates the JWT on every API request
via the `authMiddleware`, which checks the Ed25519 signature, expiry, issuer
(`akashi`), and subject (must be a valid UUID).

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant S as POST /auth/token
    participant DB as PostgreSQL
    participant JWT as JWTManager

    rect rgb(240, 248, 255)
        Note over C, JWT: Path A: API Key to JWT Exchange
        C->>S: POST /auth/token {agent_id, api_key}
        S->>DB: GetAgentsByAgentIDGlobal(agent_id)
        alt No agents found
            S->>S: DummyVerify() (timing-safe)
            S-->>C: 401 Unauthorized
        else Agents found
            loop For each agent with matching agent_id
                S->>S: Argon2id VerifyAPIKey(api_key, agent.api_key_hash)
                Note over S: First successful match wins
            end
            alt No match (or no agent had a hash)
                S->>S: DummyVerify() if no hash checked
                S-->>C: 401 Unauthorized
            else Match found
                S->>DB: GetOrganization(agent.org_id)
                alt Org email not verified
                    S-->>C: 403 Forbidden
                else Org verified
                    S->>JWT: IssueToken(agent)
                    JWT->>JWT: Build claims: agent_id, role, org_id,<br/>issuer=akashi, subject=agent.ID (UUID),<br/>jti=random UUID, exp=now+expiration
                    JWT->>JWT: Sign with Ed25519 (EdDSA)
                    JWT-->>S: signed JWT + expiresAt
                    S-->>C: 200 {token, expires_at}
                end
            end
        end
    end

    rect rgb(245, 255, 245)
        Note over C, JWT: Path B: Bearer Token on /v1/* Requests
        C->>S: GET /v1/decisions/recent (Authorization: Bearer <jwt>)
        S->>S: authMiddleware: extract Bearer token
        S->>JWT: ValidateToken(tokenStr)
        JWT->>JWT: Ed25519 signature verify
        JWT->>JWT: Check expiry (ExpiresAt)
        JWT->>JWT: Check issuer == "akashi"
        JWT->>JWT: Check subject is valid UUID
        JWT-->>S: Claims{AgentID, OrgID, Role}
        S->>S: Set claims in request context
        S->>S: Handler executes with authenticated context
    end
```

---

## 5. SSE Subscription Lifecycle

Real-time notifications are delivered via Server-Sent Events. The Broker listens
on a dedicated PostgreSQL connection (direct, not through PgBouncer) for
`LISTEN/NOTIFY` messages. Subscribers are org-scoped: a notification's `org_id`
payload field determines which subscribers receive it. Slow subscribers with full
buffers are skipped to prevent one client from blocking others. The notify
connection has automatic reconnect with exponential backoff, and tracked channels
are re-subscribed after reconnection.

```mermaid
sequenceDiagram
    autonumber
    participant C1 as Subscriber Client
    participant SSE as GET /v1/subscribe
    participant BR as Broker
    participant PG as PostgreSQL (direct conn)
    participant C2 as Writer Client
    participant HT as HandleTrace
    participant NF as storage.Notify()

    Note over BR, PG: Startup: Broker.Start() in background goroutine

    BR->>PG: LISTEN akashi_decisions
    BR->>PG: LISTEN akashi_conflicts

    C1->>SSE: GET /v1/subscribe (Bearer JWT, org_id=X)
    SSE->>SSE: Verify auth, extract org_id from claims
    SSE->>SSE: Set Content-Type: text/event-stream
    SSE->>BR: Subscribe(org_id=X) returns channel (buffered, cap=64)

    C2->>HT: POST /v1/trace (creates decision in org X)
    HT->>NF: pg_notify('akashi_decisions', {decision_id, agent_id, org_id})
    NF->>PG: SELECT pg_notify('akashi_decisions', payload)

    PG-->>BR: WaitForNotification() returns (channel, payload)
    BR->>BR: extractOrgID(payload) = X
    BR->>BR: formatSSE("akashi_decisions", payload)
    BR->>BR: broadcastToOrg(event, org_id=X)

    loop For each subscriber where sub.orgID == X
        alt Buffer has space
            BR->>C1: SSE event: "event: akashi_decisions\ndata: {...}\n\n"
        else Buffer full (cap=64)
            Note over BR: Skip slow subscriber, log warning
        end
    end

    C1->>SSE: Client disconnects (context cancelled)
    SSE->>BR: Unsubscribe(channel)
    BR->>BR: delete subscriber, close channel

    rect rgb(255, 245, 238)
        Note over BR, PG: Connection Loss Recovery
        PG--xBR: WaitForNotification() error (connection dropped)
        BR->>BR: notifyMu.Lock()
        BR->>PG: reconnectNotify(ctx) with exponential backoff
        BR->>PG: Re-LISTEN on all tracked channels
        BR->>BR: notifyMu.Unlock()
        Note over BR: Return error to caller so it retries WaitForNotification
    end
```

---

## 6. Entity Relationship Diagram

The schema is organized around organizations as the top-level tenant boundary.
Every tenant-scoped table carries an `org_id` foreign key. Decisions use
bi-temporal modeling (`valid_from`/`valid_to` for business time,
`transaction_time` for system time). The `search_outbox` table drives
asynchronous sync to Qdrant. The `agent_events` table is a TimescaleDB
hypertable partitioned by `occurred_at`, which does not support foreign keys.
The `alternatives` table tracks the options considered for each decision, and
the `evidence` table records supporting information with provenance tracking.

```mermaid
erDiagram
    organizations {
        uuid id PK
        text name
        text slug UK
        text plan "oss"
        timestamptz created_at
        timestamptz updated_at
    }

    agents {
        uuid id PK
        uuid org_id FK
        text agent_id "unique per org"
        text name
        text role "platform_admin | org_owner | admin | agent | reader"
        text api_key_hash "nullable, Argon2id"
        jsonb metadata
        timestamptz created_at
        timestamptz updated_at
    }

    agent_runs {
        uuid id PK
        uuid org_id FK
        text agent_id
        text trace_id "nullable"
        uuid parent_run_id FK "nullable, self-ref"
        text status "running | completed | failed"
        jsonb metadata
        timestamptz started_at
        timestamptz completed_at "nullable"
        timestamptz created_at
    }

    decisions {
        uuid id PK
        uuid org_id FK
        uuid run_id FK
        text agent_id
        text decision_type
        text outcome
        real confidence "0.0 to 1.0"
        text reasoning "nullable"
        vector_1024 embedding "nullable"
        real quality_score "0.0 to 1.0"
        uuid precedent_ref FK "nullable, self-ref"
        jsonb metadata
        timestamptz valid_from "business time start"
        timestamptz valid_to "nullable, business time end"
        timestamptz transaction_time "system time"
        timestamptz created_at
    }

    alternatives {
        uuid id PK
        uuid decision_id FK
        text label
        real score "nullable"
        bool selected
        text rejection_reason "nullable"
        jsonb metadata
        timestamptz created_at
    }

    evidence {
        uuid id PK
        uuid decision_id FK
        uuid org_id FK
        text source_type "validated regex: a-z, 0-9, underscore"
        text source_uri "nullable"
        text content
        real relevance_score "nullable"
        vector_1024 embedding "nullable"
        jsonb metadata
        timestamptz created_at
    }

    agent_events {
        uuid id PK "composite PK with occurred_at"
        uuid org_id
        uuid run_id "no FK (hypertable)"
        text agent_id
        text event_type
        bigint sequence_num
        timestamptz occurred_at "hypertable partition key"
        jsonb payload
        timestamptz created_at
    }

    access_grants {
        uuid id PK
        uuid org_id FK
        uuid grantor_id FK "references agents.id"
        uuid grantee_id FK "references agents.id"
        text resource_type "agent_traces | decision | run"
        text resource_id "nullable"
        text permission "read | write"
        timestamptz granted_at
        timestamptz expires_at "nullable"
    }

    search_outbox {
        bigserial id PK
        uuid decision_id "unique with operation"
        uuid org_id
        text operation "upsert | delete"
        int attempts "default 0"
        text last_error "nullable"
        timestamptz locked_until "nullable"
        timestamptz created_at
    }

    organizations ||--o{ agents : "has"
    organizations ||--o{ agent_runs : "has"
    organizations ||--o{ decisions : "has"
    organizations ||--o{ evidence : "has"
    organizations ||--o{ access_grants : "has"
    agents ||--o{ access_grants : "grantor"
    agents ||--o{ access_grants : "grantee"
    agent_runs ||--o{ decisions : "produces"
    agent_runs ||--o{ agent_events : "emits (app-enforced)"
    agent_runs ||--o| agent_runs : "parent (self-ref)"
    decisions ||--o{ alternatives : "considered"
    decisions ||--o{ evidence : "supported by"
    decisions ||--o| decisions : "precedent (self-ref)"
    decisions ||--o{ search_outbox : "queues sync"
```
