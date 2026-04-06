import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  isRetryableStatus,
  retryDelayMs,
  parseRetryAfter,
  checkResponseSize,
  sleep,
  DEFAULT_MAX_RETRIES,
  DEFAULT_RETRY_BASE_DELAY_MS,
} from "../src/retry.js";
import { AkashiClient, _resetProjectCache } from "../src/client.js";
import {
  RateLimitError,
  ServerError,
  ValidationError,
  AuthenticationError,
  AuthorizationError,
  NotFoundError,
} from "../src/errors.js";

// ---------------------------------------------------------------------------
// Unit tests for retry utility functions
// ---------------------------------------------------------------------------

describe("retry utilities", () => {
  describe("isRetryableStatus", () => {
    it("returns true for 429 (rate limit)", () => {
      expect(isRetryableStatus(429)).toBe(true);
    });

    it("returns true for 5xx server errors", () => {
      expect(isRetryableStatus(500)).toBe(true);
      expect(isRetryableStatus(502)).toBe(true);
      expect(isRetryableStatus(503)).toBe(true);
      expect(isRetryableStatus(504)).toBe(true);
      expect(isRetryableStatus(599)).toBe(true);
    });

    it("returns false for successful responses", () => {
      expect(isRetryableStatus(200)).toBe(false);
      expect(isRetryableStatus(201)).toBe(false);
      expect(isRetryableStatus(204)).toBe(false);
    });

    it("returns false for non-retryable client errors", () => {
      expect(isRetryableStatus(400)).toBe(false);
      expect(isRetryableStatus(401)).toBe(false);
      expect(isRetryableStatus(403)).toBe(false);
      expect(isRetryableStatus(404)).toBe(false);
      expect(isRetryableStatus(409)).toBe(false);
      expect(isRetryableStatus(422)).toBe(false);
    });
  });

  describe("retryDelayMs", () => {
    it("uses exponential backoff", () => {
      // With jitter disabled (by seeding), we check the general shape.
      // Attempt 0: base * 2^0 = 500ms ± jitter
      // Attempt 1: base * 2^1 = 1000ms ± jitter
      // Attempt 2: base * 2^2 = 2000ms ± jitter
      const base = 500;
      for (let attempt = 0; attempt < 5; attempt++) {
        const delay = retryDelayMs(attempt, base);
        const expected = base * Math.pow(2, attempt);
        const capped = Math.min(expected, 30_000);
        // ±25% jitter means the delay should be within [75%, 125%] of expected
        expect(delay).toBeGreaterThanOrEqual(capped * 0.75);
        expect(delay).toBeLessThanOrEqual(capped * 1.25);
      }
    });

    it("caps delay at 30 seconds", () => {
      // Attempt 20 with 500ms base = 500 * 2^20 = ~524M ms, way over cap
      const delay = retryDelayMs(20, 500);
      // Max is 30_000 ± 25% jitter = [22_500, 37_500]
      expect(delay).toBeLessThanOrEqual(37_500);
      expect(delay).toBeGreaterThanOrEqual(22_500);
    });

    it("respects Retry-After when it exceeds calculated delay", () => {
      // Attempt 0 with base 100ms = ~100ms delay.
      // Retry-After of 5000ms should override.
      const delay = retryDelayMs(0, 100, 5000);
      expect(delay).toBeGreaterThanOrEqual(4000); // some tolerance for capping
      expect(delay).toBeLessThanOrEqual(5000);
    });

    it("ignores Retry-After when it is shorter than calculated delay", () => {
      // Attempt 3 with base 500ms = 4000ms ± jitter.
      // Retry-After of 100ms should NOT override.
      const delay = retryDelayMs(3, 500, 100);
      expect(delay).toBeGreaterThanOrEqual(3000);
    });

    it("caps Retry-After at MAX_RETRY_DELAY_MS", () => {
      const delay = retryDelayMs(0, 100, 60_000);
      expect(delay).toBeLessThanOrEqual(30_000);
    });

    it("returns non-negative values", () => {
      for (let i = 0; i < 100; i++) {
        expect(retryDelayMs(0, 500)).toBeGreaterThanOrEqual(0);
      }
    });
  });

  describe("parseRetryAfter", () => {
    it("parses integer seconds to milliseconds", () => {
      expect(parseRetryAfter("5")).toBe(5000);
      expect(parseRetryAfter("1")).toBe(1000);
      expect(parseRetryAfter("120")).toBe(120_000);
    });

    it("returns 0 for null or undefined", () => {
      expect(parseRetryAfter(null)).toBe(0);
      expect(parseRetryAfter(undefined)).toBe(0);
    });

    it("returns 0 for non-numeric strings", () => {
      expect(parseRetryAfter("abc")).toBe(0);
      expect(parseRetryAfter("")).toBe(0);
    });

    it("returns 0 for negative values", () => {
      expect(parseRetryAfter("-1")).toBe(0);
    });

    it("returns 0 for zero", () => {
      expect(parseRetryAfter("0")).toBe(0);
    });
  });

  describe("checkResponseSize", () => {
    it("does not throw for responses under 10 MiB", () => {
      expect(() => checkResponseSize("1024")).not.toThrow();
      expect(() => checkResponseSize("10485760")).not.toThrow(); // exactly 10 MiB
    });

    it("throws for responses over 10 MiB", () => {
      expect(() => checkResponseSize("10485761")).toThrow(/exceeds/);
    });

    it("does not throw for null content-length", () => {
      expect(() => checkResponseSize(null)).not.toThrow();
    });
  });

  describe("sleep", () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    afterEach(() => {
      vi.useRealTimers();
    });

    it("resolves after the specified delay", async () => {
      const p = sleep(100);
      vi.advanceTimersByTime(100);
      await expect(p).resolves.toBeUndefined();
    });

    it("rejects immediately when signal is already aborted", async () => {
      const controller = new AbortController();
      controller.abort(new Error("cancelled"));
      await expect(sleep(100, controller.signal)).rejects.toThrow("cancelled");
    });

    it("rejects when signal aborts during sleep", async () => {
      const controller = new AbortController();
      const p = sleep(1000, controller.signal);
      controller.abort(new Error("cancelled mid-sleep"));
      await expect(p).rejects.toThrow("cancelled mid-sleep");
    });
  });

  describe("defaults", () => {
    it("DEFAULT_MAX_RETRIES is 3", () => {
      expect(DEFAULT_MAX_RETRIES).toBe(3);
    });

    it("DEFAULT_RETRY_BASE_DELAY_MS is 500", () => {
      expect(DEFAULT_RETRY_BASE_DELAY_MS).toBe(500);
    });
  });
});

// ---------------------------------------------------------------------------
// Integration tests for retry behavior in the client
// ---------------------------------------------------------------------------

const mockFetch = vi.fn();
vi.stubGlobal("fetch", mockFetch);

function mockHeaders(entries: Record<string, string> = {}): Headers {
  return {
    get(name: string) {
      return entries[name.toLowerCase()] ?? null;
    },
  } as Headers;
}

function mockResponse(
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: mockHeaders(headers),
    json: () => Promise.resolve(body),
    body: { cancel: () => Promise.resolve() },
  } as unknown as Response;
}

const TOKEN_RESPONSE = {
  data: {
    token: "test-jwt-token",
    expires_at: new Date(Date.now() + 3600_000).toISOString(),
  },
};

describe("AkashiClient retry behavior", () => {
  // Use real timers with tiny delays (1ms base) to avoid fake-timer/async races
  // that cause unhandled rejection warnings in vitest.

  beforeEach(() => {
    mockFetch.mockReset();
    _resetProjectCache("");
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  function makeClient(overrides: { maxRetries?: number; retryBaseDelayMs?: number } = {}) {
    return new AkashiClient({
      baseUrl: "http://localhost:8080",
      agentId: "test-agent",
      apiKey: "test-key",
      maxRetries: overrides.maxRetries ?? 2,
      retryBaseDelayMs: overrides.retryBaseDelayMs ?? 1, // 1ms base for fast tests
    });
  }

  it("retries on 503 and succeeds", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE)) // auth
      .mockResolvedValueOnce(mockResponse(503, {}))             // first attempt: 503
      .mockResolvedValueOnce(                                   // retry: success
        mockResponse(200, { data: { has_precedent: false, decisions: [] } }),
      );

    const result = await makeClient().check("test");

    expect(result.has_precedent).toBe(false);
    expect(mockFetch).toHaveBeenCalledTimes(3); // auth + 503 + success
  });

  it("retries on 429 and succeeds", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(429, {}, { "retry-after": "0" }))
      .mockResolvedValueOnce(
        mockResponse(200, { data: { has_precedent: true, decisions: [] } }),
      );

    const result = await makeClient().check("test");

    expect(result.has_precedent).toBe(true);
    expect(mockFetch).toHaveBeenCalledTimes(3);
  });

  it("retries on 502 and succeeds", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(502, {}))
      .mockResolvedValueOnce(
        mockResponse(200, { data: { has_precedent: false, decisions: [] } }),
      );

    const result = await makeClient().check("test");

    expect(result.has_precedent).toBe(false);
  });

  it("retries on network error and succeeds", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockRejectedValueOnce(new TypeError("fetch failed"))     // network error
      .mockResolvedValueOnce(
        mockResponse(200, { data: { has_precedent: false, decisions: [] } }),
      );

    const result = await makeClient().check("test");

    expect(result.has_precedent).toBe(false);
    expect(mockFetch).toHaveBeenCalledTimes(3);
  });

  it("exhausts retries on persistent 503 and throws ServerError", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(503, { error: "unavailable" }))
      .mockResolvedValueOnce(mockResponse(503, { error: "unavailable" }))
      .mockResolvedValueOnce(mockResponse(503, { error: "unavailable" }));

    await expect(makeClient({ maxRetries: 2 }).check("test")).rejects.toThrow(ServerError);
    // auth + 3 attempts (initial + 2 retries)
    expect(mockFetch).toHaveBeenCalledTimes(4);
  });

  it("exhausts retries on persistent network errors and throws", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockRejectedValueOnce(new TypeError("fetch failed"))
      .mockRejectedValueOnce(new TypeError("fetch failed"))
      .mockRejectedValueOnce(new TypeError("fetch failed"));

    await expect(makeClient({ maxRetries: 2 }).check("test")).rejects.toThrow("fetch failed");
  });

  it("does not retry 400 (ValidationError)", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(400, { error: "bad input" }));

    await expect(makeClient().check("test")).rejects.toThrow(ValidationError);
    expect(mockFetch).toHaveBeenCalledTimes(2); // auth + one attempt, no retry
  });

  it("does not retry 401 (AuthenticationError)", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(401, { error: "unauthorized" }));

    await expect(makeClient().check("test")).rejects.toThrow(AuthenticationError);
    expect(mockFetch).toHaveBeenCalledTimes(2);
  });

  it("does not retry 403 (AuthorizationError)", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(403, { error: "forbidden" }));

    await expect(makeClient().check("test")).rejects.toThrow(AuthorizationError);
    expect(mockFetch).toHaveBeenCalledTimes(2);
  });

  it("does not retry 404 (NotFoundError)", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(404, { error: "not found" }));

    await expect(makeClient().check("test")).rejects.toThrow(NotFoundError);
    expect(mockFetch).toHaveBeenCalledTimes(2);
  });

  it("does not retry when maxRetries is 0", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(503, { error: "unavailable" }));

    const client = new AkashiClient({
      baseUrl: "http://localhost:8080",
      agentId: "test-agent",
      apiKey: "test-key",
      maxRetries: 0,
    });
    _resetProjectCache("");

    await expect(client.check("test")).rejects.toThrow(ServerError);
    expect(mockFetch).toHaveBeenCalledTimes(2); // auth + one attempt
  });

  it("retries up to configured maxRetries count", async () => {
    // Set maxRetries to 1, so we get 2 total attempts
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(503, {}))
      .mockResolvedValueOnce(mockResponse(503, {}));

    await expect(makeClient({ maxRetries: 1 }).check("test")).rejects.toThrow(ServerError);
    expect(mockFetch).toHaveBeenCalledTimes(3); // auth + 2 attempts
  });

  it("retries POST requests (trace) on transient failure", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(mockResponse(502, {}))
      .mockResolvedValueOnce(
        mockResponse(200, { data: { decision_id: "d-1", status: "recorded" } }),
      );

    const result = await makeClient().trace({
      decisionType: "test",
      outcome: "chose A",
      confidence: 0.8,
      reasoning: "test",
    });

    expect(result.decision_id).toBe("d-1");
    expect(mockFetch).toHaveBeenCalledTimes(3);
  });

  it("uses default retry config when not specified", () => {
    // Verify defaults through the exported constants
    expect(DEFAULT_MAX_RETRIES).toBe(3);
    expect(DEFAULT_RETRY_BASE_DELAY_MS).toBe(500);
  });
});
