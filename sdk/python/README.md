# Akashi Python SDK

Python client for the [Akashi](../../README.md) decision-tracing API. Provides both async and sync clients, with middleware for the check-before/record-after pattern.

**Requirements:** Python 3.10+, httpx, pydantic v2

## Install

```bash
pip install akashi
# or from source:
pip install -e sdk/python
```

## Quick start

### Async client

```python
import asyncio
from akashi import AkashiClient, TraceRequest

async def main():
    client = AkashiClient(
        base_url="http://localhost:8080",
        agent_id="my-agent",
        api_key="my-api-key",
    )

    # Check for precedents before deciding.
    check = await client.check("model_selection")
    if check.has_precedent:
        print(f"Found {len(check.decisions)} prior decisions")

    # Record a decision.
    resp = await client.trace(TraceRequest(
        decision_type="model_selection",
        outcome="chose gpt-4o for summarization",
        confidence=0.85,
        reasoning="Best quality-to-cost ratio",
    ))
    print(f"Recorded decision {resp.decision_id}")

asyncio.run(main())
```

### Sync client

```python
from akashi import AkashiSyncClient, TraceRequest

client = AkashiSyncClient(
    base_url="http://localhost:8080",
    agent_id="my-agent",
    api_key="my-api-key",
)

check = client.check("model_selection")
resp = client.trace(TraceRequest(
    decision_type="model_selection",
    outcome="chose gpt-4o",
    confidence=0.9,
))
```

## API

Both `AkashiClient` (async) and `AkashiSyncClient` (sync) expose the same methods:

| Method | Description |
|--------|-------------|
| `check(decision_type, query?, agent_id?, limit?)` | Look up precedents before deciding |
| `trace(request: TraceRequest)` | Record a decision |
| `query(filters?, limit?, offset?, order_by?, order_dir?)` | Structured query with pagination |
| `search(query, limit?)` | Semantic similarity search |
| `recent(limit?, agent_id?, decision_type?)` | Get recent decisions |

## Middleware

The middleware enforces the check-before/record-after pattern automatically. Your decision function receives precedents and returns a result that implements the `Traceable` protocol (i.e., has a `to_trace()` method).

```python
from dataclasses import dataclass
from akashi import AkashiClient, AkashiMiddleware, CheckResponse, TraceRequest

@dataclass
class ModelChoice:
    model: str
    confidence: float

    def to_trace(self) -> TraceRequest:
        return TraceRequest(
            decision_type="model_selection",
            outcome=f"chose {self.model}",
            confidence=self.confidence,
        )

async def choose_model(precedents: CheckResponse, **kwargs) -> ModelChoice:
    if precedents.has_precedent:
        # Reuse a prior decision if confidence is high enough.
        best = max(precedents.decisions, key=lambda d: d.confidence)
        if best.confidence > 0.8:
            return ModelChoice(model=best.outcome, confidence=best.confidence)
    return ModelChoice(model="gpt-4o", confidence=0.85)

client = AkashiClient(base_url="...", agent_id="...", api_key="...")
middleware = AkashiMiddleware(client)
result = await middleware.wrap("model_selection", choose_model)
```

A synchronous variant `AkashiSyncMiddleware` works identically with `AkashiSyncClient`.

## Error handling

All errors inherit from `AkashiError`:

```python
from akashi import (
    AkashiError,
    AuthenticationError,   # 401
    AuthorizationError,    # 403
    NotFoundError,         # 404
    ValidationError,       # 400
    ConflictError,         # 409
    ServerError,           # 5xx
    TokenExpiredError,     # token refresh failed
)
```

## Types

All request and response types are Pydantic v2 models. Import them from `akashi`:

```python
from akashi import (
    Decision, Alternative, Evidence, DecisionConflict,
    TraceRequest, TraceAlternative, TraceEvidence,
    TraceResponse, CheckResponse, QueryResponse,
    SearchResult, SearchResponse, QueryFilters,
)
```
