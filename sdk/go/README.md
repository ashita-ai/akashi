# Kyoyu Go SDK

Go client for the [Kyoyu](../../README.md) decision-tracing API. Uses `net/http` with no dependencies beyond `github.com/google/uuid`.

## Install

```bash
go get github.com/ashita-ai/kyoyu/sdk/go/kyoyu
```

## Usage

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/ashita-ai/kyoyu/sdk/go/kyoyu"
)

func main() {
    client := kyoyu.NewClient(kyoyu.Config{
        BaseURL: "http://localhost:8080",
        AgentID: "my-agent",
        APIKey:  "my-api-key",
    })

    ctx := context.Background()

    // Check for precedents before making a decision.
    check, err := client.Check(ctx, kyoyu.CheckRequest{
        DecisionType: "model_selection",
    })
    if err != nil {
        log.Fatal(err)
    }

    if check.HasPrecedent {
        fmt.Printf("Found %d prior decisions\n", len(check.Decisions))
        for _, d := range check.Decisions {
            fmt.Printf("  %s: %s (confidence %.2f)\n", d.DecisionType, d.Outcome, d.Confidence)
        }
    }

    // Record a decision.
    reasoning := "Best quality-to-cost ratio for summarization"
    resp, err := client.Trace(ctx, kyoyu.TraceRequest{
        DecisionType: "model_selection",
        Outcome:      "chose gpt-4o for summarization",
        Confidence:   0.85,
        Reasoning:    &reasoning,
        Alternatives: []kyoyu.TraceAlternative{
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

## API

All methods take `context.Context` as the first argument and are safe for concurrent use.

### `NewClient(cfg Config) *Client`

Creates a client. Fields:

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `BaseURL` | `string` | yes | | Server URL (e.g. `http://localhost:8080`) |
| `AgentID` | `string` | yes | | Agent identifier for auth and tracing |
| `APIKey` | `string` | yes | | Secret for JWT token acquisition |
| `HTTPClient` | `*http.Client` | no | 30s timeout | Custom HTTP client |
| `Timeout` | `time.Duration` | no | 30s | Request timeout (ignored if HTTPClient is set) |

### `Check(ctx, CheckRequest) (*CheckResponse, error)`

Look up existing decisions before making a new one. If `Query` is non-empty, performs semantic search; otherwise does a structured lookup by decision type.

### `Trace(ctx, TraceRequest) (*TraceResponse, error)`

Record a decision. The client's `AgentID` is automatically included. Returns the run ID and decision ID.

### `Query(ctx, *QueryFilters, *QueryOptions) (*QueryResponse, error)`

Query past decisions with structured filters (agent IDs, decision type, confidence threshold, outcome) and pagination.

### `Search(ctx, query string, limit int) (*SearchResponse, error)`

Search decision history by semantic similarity.

### `Recent(ctx, *RecentOptions) ([]Decision, error)`

Get the most recent decisions, optionally filtered by agent ID or decision type.

## Error handling

All API errors are returned as `*kyoyu.Error` with `StatusCode`, `Code`, and `Message` fields. Helper functions:

```go
if kyoyu.IsNotFound(err) { /* 404 */ }
if kyoyu.IsUnauthorized(err) { /* 401 */ }
if kyoyu.IsForbidden(err) { /* 403 */ }
```
