"""Retry primitives for the Akashi Python SDK."""

from __future__ import annotations

import random

DEFAULT_MAX_RETRIES: int = 3
DEFAULT_RETRY_BASE_DELAY: float = 0.5  # seconds
MAX_RETRY_DELAY: float = 30.0  # seconds


def is_retryable_status(code: int) -> bool:
    """Return True for status codes that warrant an automatic retry."""
    return code == 429 or code >= 500


def retry_delay(attempt: int, base_delay: float, retry_after: float = 0.0) -> float:
    """Compute the delay before the next retry attempt.

    Uses exponential backoff (base_delay * 2^attempt) with +/-25% random jitter,
    capped at MAX_RETRY_DELAY. If the server sent a Retry-After header whose
    parsed value exceeds the computed delay, the larger value wins.
    """
    exp = base_delay * (2 ** attempt)
    jittered = exp * random.uniform(0.75, 1.25)
    capped = min(jittered, MAX_RETRY_DELAY)
    return min(max(capped, retry_after), MAX_RETRY_DELAY)


def parse_retry_after(header: str | None) -> float:
    """Parse an integer-seconds Retry-After header value.

    Returns 0.0 if the header is absent or cannot be parsed as a non-negative
    integer. HTTP-date values are not supported; they return 0.0.
    """
    if header is None:
        return 0.0
    try:
        value = int(header)
        return float(value) if value >= 0 else 0.0
    except (ValueError, OverflowError):
        return 0.0
