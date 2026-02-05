"""Akashi Python SDK — decision tracing for AI agent coordination."""

from akashi.client import AkashiClient, AkashiSyncClient
from akashi.exceptions import (
    AuthenticationError,
    AuthorizationError,
    ConflictError,
    AkashiError,
    NotFoundError,
    ServerError,
    TokenExpiredError,
    ValidationError,
)
from akashi.middleware import AkashiMiddleware, AkashiSyncMiddleware
from akashi.types import (
    Alternative,
    CheckResponse,
    Decision,
    DecisionConflict,
    Evidence,
    QueryFilters,
    QueryResponse,
    SearchResponse,
    SearchResult,
    TraceAlternative,
    TraceEvidence,
    TraceRequest,
    TraceResponse,
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
    # Types — requests
    "TraceRequest",
    "TraceAlternative",
    "TraceEvidence",
    "QueryFilters",
    # Types — responses
    "TraceResponse",
    "CheckResponse",
    "QueryResponse",
    "SearchResult",
    "SearchResponse",
    # Exceptions
    "AkashiError",
    "AuthenticationError",
    "AuthorizationError",
    "NotFoundError",
    "ValidationError",
    "ConflictError",
    "ServerError",
    "TokenExpiredError",
]
