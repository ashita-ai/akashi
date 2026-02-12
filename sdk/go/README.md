# Akashi Go SDK

Go client for the [Akashi](../../README.md) decision audit API -- the black box recorder for AI decisions. Uses `net/http` with no dependencies beyond `github.com/google/uuid`.

## Install

```bash
go get github.com/ashita-ai/akashi/sdk/go/akashi
```

## Quick start

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/ashita-ai/akashi/sdk/go/akashi"
)

func main() {
    client, err := akashi.NewClient(akashi.Config{
        BaseURL: "http://localhost:8080",
        AgentID: "my-agent",
        APIKey:  "my-api-key",
    })
    if err != nil {
        log.Fatal(err)
    }

    ctx := context.Background()

    // Check for precedents before making a decision.
    check, err := client.Check(ctx, akashi.CheckRequest{
        DecisionType: "model_selection",
    })
    if err != nil {
        log.Fatal(err)
    }

    if check.HasPrecedent {
        fmt.Printf("Found %d prior decisions\n", len(check.Decisions))
    }

    // Record a decision.
    reasoning := "Best quality-to-cost ratio for summarization"
    resp, err := client.Trace(ctx, akashi.TraceRequest{
        DecisionType: "model_selection",
        Outcome:      "chose gpt-4o for summarization",
        Confidence:   0.85,
        Reasoning:    &reasoning,
        Alternatives: []akashi.TraceAlternative{
            {Label: "gpt-4o", Selected: true},
            {Label: "claude-3-haiku", Selected: false},
        },
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("Recorded decision %s\n", resp.DecisionID)
}
```

## Constructor

### `NewClient(cfg Config) (*Client, error)`

Creates a client. Returns an error if any required field is empty.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `BaseURL` | `string` | yes | | Server URL (e.g. `http://localhost:8080`) |
| `AgentID` | `string` | yes | | Agent identifier for auth and tracing |
| `APIKey` | `string` | yes | | Secret for JWT token acquisition |
| `HTTPClient` | `*http.Client` | no | 30s timeout | Custom HTTP client |
| `Timeout` | `time.Duration` | no | 30s | Request timeout (ignored if HTTPClient is set) |

## API reference

All methods take `context.Context` as the first argument and are safe for concurrent use. JWT tokens are acquired and refreshed automatically.

### Decision tracing

| Method | Description |
|--------|-------------|
| `Trace(ctx, TraceRequest) (*TraceResponse, error)` | Record a decision with alternatives, evidence, and reasoning. The client's AgentID is included automatically. |
| `Check(ctx, CheckRequest) (*CheckResponse, error)` | Look up existing decisions before making a new one. Performs semantic search if `Query` is set; structured lookup otherwise. |

### Run lifecycle

| Method | Description |
|--------|-------------|
| `CreateRun(ctx, CreateRunRequest) (*AgentRun, error)` | Start a new agent run. |
| `AppendEvents(ctx, runID, []EventInput) (*AppendEventsResponse, error)` | Append events to a running run. |
| `CompleteRun(ctx, runID, status, metadata) (*AgentRun, error)` | Mark a run as completed or failed. |
| `GetRun(ctx, runID) (*GetRunResponse, error)` | Retrieve a run with its events and decisions. |

### Querying

| Method | Description |
|--------|-------------|
| `Query(ctx, *QueryFilters, *QueryOptions) (*QueryResponse, error)` | Query decisions with structured filters and pagination. |
| `TemporalQuery(ctx, asOf, *QueryFilters) (*TemporalQueryResponse, error)` | Bi-temporal query: decisions valid at a point in time. |
| `Search(ctx, query, limit) (*SearchResponse, error)` | Semantic similarity search over decision history. |
| `Recent(ctx, *RecentOptions) ([]Decision, error)` | Get the most recent decisions, optionally filtered. |

### Agent history

| Method | Description |
|--------|-------------|
| `AgentHistory(ctx, agentID, limit) (*AgentHistoryResponse, error)` | Get the decision history for a specific agent. |

### Agent management

| Method | Description |
|--------|-------------|
| `CreateAgent(ctx, CreateAgentRequest) (*Agent, error)` | Register a new agent. Requires admin role. |
| `ListAgents(ctx) ([]Agent, error)` | List all agents in the organization. Requires admin role. |
| `DeleteAgent(ctx, agentID) (*DeleteAgentResponse, error)` | Delete an agent and all associated data. Requires admin role. |
| `UpdateAgentTags(ctx, agentID, tags) (*Agent, error)` | Replace an agent's tags. Requires admin role. |

### Access control

| Method | Description |
|--------|-------------|
| `CreateGrant(ctx, CreateGrantRequest) (*Grant, error)` | Grant another agent access to specific resources. |
| `DeleteGrant(ctx, grantID) error` | Revoke an access grant. |

### Integrity

| Method | Description |
|--------|-------------|
| `VerifyDecision(ctx, decisionID) (*VerifyResponse, error)` | Recompute the SHA-256 content hash and compare to the stored hash. |
| `GetDecisionRevisions(ctx, decisionID) (*RevisionsResponse, error)` | Get the full revision chain for a decision. |

### Monitoring

| Method | Description |
|--------|-------------|
| `ListConflicts(ctx, *ConflictOptions) (*ConflictsResponse, error)` | List detected conflicts between decisions. |
| `Health(ctx) (*HealthResponse, error)` | Check server health. Does not require authentication. |

## Error handling

All API errors are returned as `*akashi.Error` with `StatusCode`, `Code`, and `Message` fields.

```go
resp, err := client.Trace(ctx, req)
if err != nil {
    if akashi.IsRateLimited(err) {
        // Back off and retry
    }
    if akashi.IsNotFound(err) {
        // Resource does not exist
    }
    log.Fatal(err)
}
```

Helper functions:

| Function | Matches |
|----------|---------|
| `IsNotFound(err)` | HTTP 404 |
| `IsUnauthorized(err)` | HTTP 401 |
| `IsForbidden(err)` | HTTP 403 |
| `IsRateLimited(err)` | HTTP 429 |
| `IsConflict(err)` | HTTP 409 |

## Retry and rate limiting

The SDK does not retry failed requests automatically. When the server returns HTTP 429, the error satisfies `IsRateLimited()`. Callers should implement their own retry logic with exponential backoff. The server includes `Retry-After` headers on rate-limited responses.
