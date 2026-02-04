"""Kyoyu Python SDK — decision tracing for AI agent coordination."""

from kyoyu.client import KyoyuClient, KyoyuSyncClient
from kyoyu.exceptions import (
    AuthenticationError,
    AuthorizationError,
    ConflictError,
    KyoyuError,
    NotFoundError,
    ServerError,
    TokenExpiredError,
    ValidationError,
)
from kyoyu.middleware import KyoyuMiddleware, KyoyuSyncMiddleware
from kyoyu.types import (
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
    "KyoyuClient",
    "KyoyuSyncClient",
    # Middleware
    "KyoyuMiddleware",
    "KyoyuSyncMiddleware",
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
    "KyoyuError",
    "AuthenticationError",
    "AuthorizationError",
    "NotFoundError",
    "ValidationError",
    "ConflictError",
    "ServerError",
    "TokenExpiredError",
]
