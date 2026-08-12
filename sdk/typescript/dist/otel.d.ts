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
export declare function traceIdFromContext(): string | undefined;
//# sourceMappingURL=otel.d.ts.map