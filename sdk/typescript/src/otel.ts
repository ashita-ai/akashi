/**
 * Optional OTEL trace context helpers for Akashi.
 *
 * These functions gracefully degrade when @opentelemetry/api is not installed.
 * They extract the current OTEL trace ID so it can be passed to Akashi's
 * trace_id field without manual header construction.
 */

/**
 * Extract the current OTEL trace ID from the active span, if available.
 *
 * Returns the 32-character lowercase hex trace ID, or undefined if
 * @opentelemetry/api is not installed or no active span exists.
 */
export function traceIdFromContext(): string | undefined {
  try {
    // Dynamic require so this module doesn't hard-depend on @opentelemetry/api.
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const { trace } = require("@opentelemetry/api");
    const span = trace.getActiveSpan();
    if (span) {
      const ctx = span.spanContext();
      if (ctx.traceId !== "00000000000000000000000000000000") {
        return ctx.traceId;
      }
    }
  } catch {
    // @opentelemetry/api not installed — that's fine.
  }
  return undefined;
}
