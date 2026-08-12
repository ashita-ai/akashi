/** Base error for all Akashi SDK errors. */
export declare class AkashiError extends Error {
    readonly statusCode?: number | undefined;
    constructor(message: string, statusCode?: number | undefined);
}
/** Raised when authentication fails (401). */
export declare class AuthenticationError extends AkashiError {
    constructor(message?: string);
}
/** Raised when the agent lacks permission (403). */
export declare class AuthorizationError extends AkashiError {
    constructor(message?: string);
}
/** Raised when a requested resource does not exist (404). */
export declare class NotFoundError extends AkashiError {
    constructor(message?: string);
}
/** Raised when the server rejects input as invalid (400). */
export declare class ValidationError extends AkashiError {
    constructor(message?: string);
}
/** Raised on duplicate or conflicting resources (409). */
export declare class ConflictError extends AkashiError {
    constructor(message?: string);
}
/** Raised when the client is rate-limited (429). */
export declare class RateLimitError extends AkashiError {
    constructor(message?: string);
}
/** Raised on unexpected server-side errors (5xx). */
export declare class ServerError extends AkashiError {
    constructor(statusCode: number, message?: string);
}
//# sourceMappingURL=errors.d.ts.map