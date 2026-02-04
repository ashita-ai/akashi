/** Base error for all Kyoyu SDK errors. */
export class KyoyuError extends Error {
  constructor(
    message: string,
    public readonly statusCode?: number,
  ) {
    super(message);
    this.name = "KyoyuError";
  }
}

/** Raised when authentication fails (401). */
export class AuthenticationError extends KyoyuError {
  constructor(message = "Authentication failed") {
    super(message, 401);
    this.name = "AuthenticationError";
  }
}

/** Raised when the agent lacks permission (403). */
export class AuthorizationError extends KyoyuError {
  constructor(message = "Insufficient permissions") {
    super(message, 403);
    this.name = "AuthorizationError";
  }
}

/** Raised when a requested resource does not exist (404). */
export class NotFoundError extends KyoyuError {
  constructor(message = "Resource not found") {
    super(message, 404);
    this.name = "NotFoundError";
  }
}

/** Raised when the server rejects input as invalid (400). */
export class ValidationError extends KyoyuError {
  constructor(message = "Bad request") {
    super(message, 400);
    this.name = "ValidationError";
  }
}

/** Raised on duplicate or conflicting resources (409). */
export class ConflictError extends KyoyuError {
  constructor(message = "Conflict") {
    super(message, 409);
    this.name = "ConflictError";
  }
}

/** Raised on unexpected server-side errors (5xx). */
export class ServerError extends KyoyuError {
  constructor(statusCode: number, message = "Server error") {
    super(message, statusCode);
    this.name = "ServerError";
  }
}
