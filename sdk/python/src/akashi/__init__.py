"""Akashi Python SDK — decision tracing for AI agent coordination."""

from akashi.client import AkashiClient, AkashiSyncClient
from akashi.exceptions import (
    AuthenticationError,
    AuthorizationError,
    ConflictError,
    AkashiError,
    NotFoundError,
    RateLimitError,
    ServerError,
    TokenExpiredError,
    ValidationError,
)
from akashi.middleware import AkashiMiddleware, AkashiSyncMiddleware
from akashi.otel import trace_id_from_context
from akashi.types import (
    Agent,
    AgentEvent,
    AgentRun,
    Alternative,
    CheckResponse,
    CompleteRunRequest,
    CreateAgentRequest,
    CreateGrantRequest,
    CreateRunRequest,
    Decision,
    DecisionConflict,
    EventInput,
    Evidence,
    Grant,
    HealthResponse,
    QueryFilters,
    QueryResponse,
    SearchResponse,
    SearchResult,
    TemporalQueryRequest,
    TraceAlternative,
    TraceEvidence,
    TraceRequest,
    TraceResponse,
    UsageResponse,
)

__all__ = [
    # Clients
    "AkashiClient",
    "AkashiSyncClient",
    # Middleware
    "AkashiMiddleware",
    "AkashiSyncMiddleware",
    # Types — domain
    "Decision",
    "Alternative",
    "Evidence",
    "DecisionConflict",
    "AgentRun",
    "AgentEvent",
    "Agent",
    "Grant",
    # Types — requests
    "TraceRequest",
    "TraceAlternative",
    "TraceEvidence",
    "QueryFilters",
    "CreateRunRequest",
    "EventInput",
    "CompleteRunRequest",
    "CreateAgentRequest",
    "CreateGrantRequest",
    "TemporalQueryRequest",
    # Types — responses
    "TraceResponse",
    "CheckResponse",
    "QueryResponse",
    "SearchResult",
    "SearchResponse",
    "HealthResponse",
    "UsageResponse",
    # OTEL helpers
    "trace_id_from_context",
    # Exceptions
    "AkashiError",
    "AuthenticationError",
    "AuthorizationError",
    "NotFoundError",
    "ValidationError",
    "ConflictError",
    "RateLimitError",
    "ServerError",
    "TokenExpiredError",
]
