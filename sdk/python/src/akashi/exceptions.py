"""Exception hierarchy for the Akashi Python SDK."""

from __future__ import annotations


class AkashiError(Exception):
    """Base exception for all Akashi SDK errors."""


class AuthenticationError(AkashiError):
    """Raised when authentication fails (401)."""


class AuthorizationError(AkashiError):
    """Raised when the agent lacks permission (403)."""


class NotFoundError(AkashiError):
    """Raised when a requested resource does not exist (404)."""


class ValidationError(AkashiError):
    """Raised when the server rejects input as invalid (400)."""


class ConflictError(AkashiError):
    """Raised on duplicate or conflicting resources (409)."""


class ServerError(AkashiError):
    """Raised on unexpected server-side errors (5xx)."""


class TokenExpiredError(AuthenticationError):
    """Raised when the JWT token has expired and could not be refreshed."""
