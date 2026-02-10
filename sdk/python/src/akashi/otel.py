"""Optional OTEL trace context helpers for Akashi.

These functions gracefully degrade when the opentelemetry-api package is not
installed. They are convenience wrappers that extract the current OTEL trace ID
so it can be passed to Akashi's trace_id field without manual header
construction.
"""


def trace_id_from_context() -> str | None:
    """Extract the current OTEL trace ID, if available.

    Returns the 32-character lowercase hex trace ID from the active OTEL span,
    or None if OTEL is not installed or no active span exists.
    """
    try:
        from opentelemetry import trace as otel_trace

        span = otel_trace.get_current_span()
        ctx = span.get_span_context()
        if ctx.is_valid:
            return format(ctx.trace_id, "032x")
    except ImportError:
        pass
    return None
