"""Exception hierarchy for the Kyoyu Python SDK."""

from __future__ import annotations


class KyoyuError(Exception):
    """Base exception for all Kyoyu SDK errors."""


class AuthenticationError(KyoyuError):
    """Raised when authentication fails (401)."""


class AuthorizationError(KyoyuError):
    """Raised when the agent lacks permission (403)."""


class NotFoundError(KyoyuError):
    """Raised when a requested resource does not exist (404)."""


class ValidationError(KyoyuError):
    """Raised when the server rejects input as invalid (400)."""


class ConflictError(KyoyuError):
    """Raised on duplicate or conflicting resources (409)."""


class ServerError(KyoyuError):
    """Raised on unexpected server-side errors (5xx)."""


class TokenExpiredError(AuthenticationError):
    """Raised when the JWT token has expired and could not be refreshed."""
