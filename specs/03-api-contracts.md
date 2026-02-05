# SPEC-003: API Contracts

**Status:** Draft
**Date:** 2026-02-03
**Depends on:** SPEC-001 (System Overview), SPEC-002 (Data Model), ADR-004 (MCP distribution)

---

## Overview

Akashi exposes two API surfaces that share a single service layer:

1. **HTTP JSON API** — primary programmatic interface for SDKs and direct clients
2. **MCP Server** — framework-agnostic interface for MCP-compatible AI agents

Both require JWT authentication. All responses include standard error envelopes.

## Authentication

### JWT Flow

```
1. Client authenticates with agent_id + api_key → POST /auth/token
2. Server validates credentials, returns JWT with claims:
   {
     "sub": "agent-uuid",
     "agent_id": "underwriting-agent",
     "role": "agent",
     "exp": 1738600000,
     "iss": "akashi"
   }
3. Client includes JWT in subsequent requests:
   Authorization: Bearer <jwt>
```

### Endpoints

```
POST /auth/token
  Request:  { "agent_id": "string", "api_key": "string" }
  Response: { "token": "jwt-string", "expires_at": "iso8601" }
  Errors:   401 Invalid credentials

POST /auth/refresh
  Request:  Authorization: Bearer <expiring-jwt>
  Response: { "token": "jwt-string", "expires_at": "iso8601" }
```

### RBAC Permissions

| Role | Trace (write) | Query (read) | Search (read) | Admin |
|------|--------------|-------------|--------------|-------|
| `admin` | All | All | All | Yes |
| `agent` | Own traces | Granted traces | Granted traces | No |
| `reader` | No | Granted traces | Granted traces | No |

"Granted" means the agent has an `access_grants` row for the target resource.

---

## HTTP API

### Standard Response Envelope

Success:
```json
{
  "data": { ... },
  "meta": {
    "request_id": "uuid",
    "timestamp": "iso8601"
  }
}
```

Error:
```json
{
  "error": {
    "code": "INVALID_INPUT",
    "message": "Human-readable message",
    "details": { ... }
  },
  "meta": {
    "request_id": "uuid",
    "timestamp": "iso8601"
  }
}
```

### Error Codes

| HTTP Status | Code | Meaning |
|------------|------|---------|
| 400 | `INVALID_INPUT` | Malformed request body or parameters |
| 401 | `UNAUTHORIZED` | Missing or invalid JWT |
| 403 | `FORBIDDEN` | Valid JWT but insufficient permissions |
| 404 | `NOT_FOUND` | Resource does not exist |
| 409 | `CONFLICT` | Conflicting state (e.g., duplicate run) |
| 429 | `RATE_LIMITED` | Too many requests |
| 500 | `INTERNAL_ERROR` | Server error |

---

### Trace Ingestion

#### `POST /v1/runs`

Create a new agent run.

```json
// Request
{
  "agent_id": "underwriting-agent",
  "trace_id": "otel-trace-id-optional",
  "parent_run_id": "uuid-optional",
  "metadata": { "model": "gpt-4o", "version": "1.2.0" }
}

// Response 201
{
  "data": {
    "id": "uuid",
    "agent_id": "underwriting-agent",
    "status": "running",
    "started_at": "2026-02-03T10:00:00Z"
  }
}
```

#### `POST /v1/runs/{run_id}/events`

Append events to a run. Accepts single events or batches.

```json
// Request (batch)
{
  "events": [
    {
      "event_type": "DecisionStarted",
      "occurred_at": "2026-02-03T10:00:01Z",
      "payload": {
        "decision_type": "loan_approval",
        "context_summary": "Evaluating application DTI=42%, credit=720"
      }
    },
    {
      "event_type": "EvidenceGathered",
      "occurred_at": "2026-02-03T10:00:02Z",
      "payload": {
        "decision_id": "uuid",
        "source_type": "api_response",
        "source_uri": "credit-bureau/report/12345",
        "content_summary": "Credit score 720",
        "relevance_score": 0.95
      }
    }
  ]
}

// Response 201
{
  "data": {
    "accepted": 2,
    "event_ids": ["uuid1", "uuid2"]
  }
}
```

Events are accepted **optimistically** — the server never blocks an agent from writing. Validation errors on individual events are reported but don't reject the batch.

#### `POST /v1/runs/{run_id}/complete`

Mark a run as completed.

```json
// Request
{
  "status": "completed",
  "metadata": { "total_tokens": 15400, "duration_ms": 4523 }
}

// Response 200
{
  "data": {
    "id": "uuid",
    "status": "completed",
    "completed_at": "2026-02-03T10:00:05Z"
  }
}
```

#### `POST /v1/trace`

Convenience endpoint: create run + record events + complete in one call. Equivalent to calling the three endpoints above sequentially.

```json
// Request
{
  "agent_id": "underwriting-agent",
  "trace_id": "otel-trace-id-optional",
  "decision": {
    "decision_type": "loan_approval",
    "outcome": "approve_with_conditions",
    "confidence": 0.87,
    "reasoning": "DTI within threshold, strong credit history",
    "alternatives": [
      { "label": "Approve", "score": 0.87, "selected": true },
      { "label": "Deny", "score": 0.15, "selected": false, "rejection_reason": "Strong credit profile" },
      { "label": "Refer to committee", "score": 0.42, "selected": false, "rejection_reason": "Within auto-approval threshold" }
    ],
    "evidence": [
      { "source_type": "api_response", "source_uri": "credit-bureau/12345", "content": "Score 720", "relevance_score": 0.95 },
      { "source_type": "document", "source_uri": "s3://docs/bank-statements.pdf", "content": "6 months statements verified", "relevance_score": 0.8 }
    ]
  },
  "metadata": { "model": "gpt-4o", "temperature": 0.1 }
}

// Response 201
{
  "data": {
    "run_id": "uuid",
    "decision_id": "uuid",
    "event_count": 7
  }
}
```

---

### Query

#### `POST /v1/query`

Structured query over decision traces. Supports filters, time ranges, and pagination.

```json
// Request
{
  "filters": {
    "agent_id": ["underwriting-agent", "risk-agent"],
    "decision_type": "loan_approval",
    "confidence_min": 0.7,
    "outcome": "approve",
    "time_range": {
      "from": "2026-01-01T00:00:00Z",
      "to": "2026-02-03T23:59:59Z"
    }
  },
  "include": ["alternatives", "evidence"],
  "order_by": "confidence",
  "order_dir": "desc",
  "limit": 50,
  "offset": 0
}

// Response 200
{
  "data": {
    "decisions": [
      {
        "id": "uuid",
        "run_id": "uuid",
        "agent_id": "underwriting-agent",
        "decision_type": "loan_approval",
        "outcome": "approve_with_conditions",
        "confidence": 0.87,
        "reasoning": "...",
        "valid_from": "2026-02-03T10:00:00Z",
        "valid_to": null,
        "alternatives": [ ... ],
        "evidence": [ ... ]
      }
    ],
    "total": 142,
    "limit": 50,
    "offset": 0
  }
}
```

#### `POST /v1/query/temporal`

Point-in-time query (bi-temporal). "What did we know at time T?"

```json
// Request
{
  "as_of": "2026-02-01T12:00:00Z",
  "filters": {
    "agent_id": ["underwriting-agent"],
    "decision_type": "loan_approval"
  }
}

// Response 200
{
  "data": {
    "as_of": "2026-02-01T12:00:00Z",
    "decisions": [ ... ]
  }
}
```

#### `GET /v1/runs/{run_id}`

Get a specific run with its events.

```json
// Response 200
{
  "data": {
    "run": { ... },
    "events": [ ... ],
    "decisions": [ ... ]
  }
}
```

#### `GET /v1/agents/{agent_id}/history`

Get an agent's decision history.

```json
// Query params: ?limit=50&offset=0&from=iso8601&to=iso8601
// Response 200
{
  "data": {
    "agent_id": "underwriting-agent",
    "decisions": [ ... ],
    "total": 340,
    "limit": 50,
    "offset": 0
  }
}
```

---

### Search

#### `POST /v1/search`

Semantic similarity search over decision traces using pgvector.

```json
// Request
{
  "query": "loan decisions where the borrower had employment gaps",
  "search_type": "decisions",
  "filters": {
    "agent_id": ["underwriting-agent"],
    "confidence_min": 0.5,
    "time_range": {
      "from": "2025-01-01T00:00:00Z"
    }
  },
  "limit": 10
}

// Response 200
{
  "data": {
    "results": [
      {
        "decision": { ... },
        "similarity_score": 0.92,
        "alternatives": [ ... ],
        "evidence": [ ... ]
      }
    ],
    "total": 10
  }
}
```

---

### Subscriptions

#### `GET /v1/subscribe`

Server-Sent Events (SSE) stream for real-time decision notifications.

```
GET /v1/subscribe?agent_id=underwriting-agent&decision_type=loan_approval&confidence_min=0.7
Authorization: Bearer <jwt>
Accept: text/event-stream

// Response (SSE stream)
data: {"event":"decision_made","decision":{"id":"uuid","agent_id":"underwriting-agent","outcome":"approve","confidence":0.87}}

data: {"event":"conflict_detected","conflict":{"decision_a_id":"uuid1","decision_b_id":"uuid2","type":"opposing_outcomes"}}
```

---

### Access Control

#### `POST /v1/grants`

Grant an agent access to another agent's traces.

```json
// Request (requires admin or grantor role)
{
  "grantee_agent_id": "compliance-agent",
  "resource_type": "agent_traces",
  "resource_id": "underwriting-agent",
  "permission": "read",
  "expires_at": "2026-12-31T23:59:59Z"
}

// Response 201
{
  "data": {
    "id": "uuid",
    "granted_at": "2026-02-03T10:00:00Z"
  }
}
```

#### `DELETE /v1/grants/{grant_id}`

Revoke an access grant.

---

### Conflicts

#### `GET /v1/conflicts`

List detected decision conflicts.

```json
// Query params: ?decision_type=loan_approval&severity=high&limit=50
// Response 200
{
  "data": {
    "conflicts": [
      {
        "decision_a": { "id": "uuid1", "agent_id": "underwriting-agent", "outcome": "approve", "confidence": 0.87 },
        "decision_b": { "id": "uuid2", "agent_id": "risk-agent", "outcome": "deny", "confidence": 0.73 },
        "decision_type": "loan_approval",
        "detected_at": "2026-02-03T10:05:00Z"
      }
    ],
    "total": 3
  }
}
```

---

### Health

#### `GET /health`

No auth required.

```json
{
  "status": "healthy",
  "version": "0.1.0",
  "postgres": "connected",
  "uptime_seconds": 86400
}
```

---

## MCP Server Interface

The MCP server exposes the same capabilities through the MCP protocol.

### Resources (read-only context)

| URI | Description | Maps to |
|-----|------------|---------|
| `akashi://session/current` | Current session context for the requesting agent | `GET /v1/agents/{agent_id}/history?limit=10` |
| `akashi://decisions/recent` | Recent decisions across all accessible agents | `POST /v1/query` with time filter |
| `akashi://agent/{id}/history` | Specific agent's decision history | `GET /v1/agents/{id}/history` |

### Tools (executable operations)

#### `akashi_trace`

Record a decision trace.

```json
{
  "name": "akashi_trace",
  "description": "Record a structured decision trace with alternatives, evidence, and confidence",
  "inputSchema": {
    "type": "object",
    "required": ["decision_type", "outcome", "confidence"],
    "properties": {
      "decision_type": { "type": "string", "description": "Category of decision" },
      "outcome": { "type": "string", "description": "What was decided" },
      "confidence": { "type": "number", "minimum": 0, "maximum": 1, "description": "Confidence score" },
      "reasoning": { "type": "string", "description": "Step-by-step reasoning chain" },
      "alternatives": {
        "type": "array",
        "items": {
          "type": "object",
          "properties": {
            "label": { "type": "string" },
            "score": { "type": "number" },
            "selected": { "type": "boolean" },
            "rejection_reason": { "type": "string" }
          }
        }
      },
      "evidence": {
        "type": "array",
        "items": {
          "type": "object",
          "properties": {
            "source_type": { "type": "string" },
            "source_uri": { "type": "string" },
            "content": { "type": "string" },
            "relevance_score": { "type": "number" }
          }
        }
      }
    }
  }
}
```

#### `akashi_query`

Structured query over past decisions.

```json
{
  "name": "akashi_query",
  "description": "Query past decisions with structured filters, time ranges, and result ordering",
  "inputSchema": {
    "type": "object",
    "properties": {
      "decision_type": { "type": "string" },
      "agent_id": { "type": "string" },
      "outcome": { "type": "string" },
      "confidence_min": { "type": "number" },
      "from": { "type": "string", "format": "date-time" },
      "to": { "type": "string", "format": "date-time" },
      "limit": { "type": "integer", "default": 10 }
    }
  }
}
```

#### `akashi_search`

Semantic similarity search.

```json
{
  "name": "akashi_search",
  "description": "Search decision history by semantic similarity. Find precedents and related decisions.",
  "inputSchema": {
    "type": "object",
    "required": ["query"],
    "properties": {
      "query": { "type": "string", "description": "Natural language search query" },
      "limit": { "type": "integer", "default": 5 },
      "confidence_min": { "type": "number" }
    }
  }
}
```

## Rate Limiting

| Role | Ingestion (events/sec) | Queries/min | Search/min |
|------|----------------------|------------|-----------|
| `admin` | Unlimited | 1000 | 200 |
| `agent` | 1000 | 300 | 100 |
| `reader` | 0 | 300 | 100 |

Rate limit headers:
```
X-RateLimit-Limit: 300
X-RateLimit-Remaining: 287
X-RateLimit-Reset: 1738600060
```

## Versioning

API is versioned via URL path (`/v1/`). Breaking changes increment the version. Non-breaking additions (new fields, new endpoints) don't require version bumps.

## References

- ADR-004: MCP as primary distribution channel
- SPEC-001: System Overview
- SPEC-002: Data Model
- MCP specification: modelcontextprotocol.io/specification/2025-11-25
