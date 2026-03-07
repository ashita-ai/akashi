import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { ApiError } from "./api";

// We test the ApiError class and the internal helpers indirectly.
// The fetch-based functions are integration-level; we test the error class shape
// and the request header logic by mocking fetch.

describe("ApiError", () => {
  it("has correct properties", () => {
    const err = new ApiError(403, "FORBIDDEN", "Not allowed");
    expect(err).toBeInstanceOf(Error);
    expect(err.name).toBe("ApiError");
    expect(err.status).toBe(403);
    expect(err.code).toBe("FORBIDDEN");
    expect(err.message).toBe("Not allowed");
  });
});

describe("login", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            data: { token: "tok_123", expires_at: "2025-12-31T00:00:00Z" },
            meta: { request_id: "r1", timestamp: "now" },
          }),
      }),
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("sends POST to /auth/token with credentials", async () => {
    // Dynamic import so the stubbed fetch is in place.
    const { login } = await import("./api");
    const result = await login("agent-1", "key-abc");

    expect(fetch).toHaveBeenCalledOnce();
    const [path, opts] = (fetch as ReturnType<typeof vi.fn>).mock.calls[0]!;
    expect(path).toBe("/auth/token");
    expect(opts.method).toBe("POST");
    const body = JSON.parse(opts.body as string);
    expect(body).toEqual({ agent_id: "agent-1", api_key: "key-abc" });
    expect(result.token).toBe("tok_123");
  });
});

describe("request error handling", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("throws ApiError with parsed error body on non-ok response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 401,
        json: () =>
          Promise.resolve({
            error: { code: "UNAUTHORIZED", message: "Bad credentials" },
            meta: { request_id: "r1", timestamp: "now" },
          }),
      }),
    );

    const { login } = await import("./api");
    await expect(login("bad", "creds")).rejects.toThrow("Bad credentials");
  });

  it("throws ApiError with fallback message when response is not JSON", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        json: () => Promise.reject(new Error("not json")),
      }),
    );

    const { login } = await import("./api");
    await expect(login("x", "y")).rejects.toThrow(
      "Request failed with status 500",
    );
  });
});
