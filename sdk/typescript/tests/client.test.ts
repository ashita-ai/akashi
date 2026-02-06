import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { AkashiClient } from "../src/client.js";
import { withAkashi } from "../src/middleware.js";
import {
  AkashiError,
  AuthenticationError,
  AuthorizationError,
  ConflictError,
  NotFoundError,
  ServerError,
  ValidationError,
} from "../src/errors.js";
import type { CheckResponse, Decision, TraceRequest } from "../src/types.js";

// Mock fetch globally.
const mockFetch = vi.fn();
vi.stubGlobal("fetch", mockFetch);

// Helper to create a mock Response.
function mockResponse(
  status: number,
  body: unknown,
  ok = status >= 200 && status < 300,
): Response {
  return {
    ok,
    status,
    json: () => Promise.resolve(body),
  } as Response;
}

// Standard token response for auth.
const TOKEN_RESPONSE = {
  data: {
    token: "test-jwt-token",
    expires_at: new Date(Date.now() + 3600_000).toISOString(),
  },
};

describe("AkashiClient", () => {
  let client: AkashiClient;

  beforeEach(() => {
    mockFetch.mockReset();
    client = new AkashiClient({
      baseUrl: "http://localhost:8080",
      agentId: "test-agent",
      apiKey: "test-key",
    });
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  describe("authentication", () => {
    it("fetches token before first request", async () => {
      mockFetch
        .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
        .mockResolvedValueOnce(
          mockResponse(200, {
            data: { has_precedent: false, precedents: [] },
          }),
        );

      await client.check("test-type");

      expect(mockFetch).toHaveBeenCalledTimes(2);
      expect(mockFetch).toHaveBeenNthCalledWith(
        1,
        "http://localhost:8080/auth/token",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ agent_id: "test-agent", api_key: "test-key" }),
        }),
      );
    });

    it("reuses cached token for subsequent requests", async () => {
      mockFetch
        .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
        .mockResolvedValueOnce(
          mockResponse(200, { data: { has_precedent: false, precedents: [] } }),
        )
        .mockResolvedValueOnce(
          mockResponse(200, { data: { has_precedent: true, precedents: [] } }),
        );

      await client.check("first");
      await client.check("second");

      // Only one token request, two API requests.
      expect(mockFetch).toHaveBeenCalledTimes(3);
      const calls = mockFetch.mock.calls.map((c) => c[0]);
      expect(calls.filter((u) => u.includes("/auth/token"))).toHaveLength(1);
    });
  });

  describe("check", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("returns precedent check result", async () => {
      const expected: CheckResponse = {
        has_precedent: true,
        precedents: [
          {
            decision_id: "dec-123",
            decision_type: "model_selection",
            outcome: "gpt-4o",
            confidence: 0.9,
            similarity: 0.85,
          },
        ],
      };
      mockFetch.mockResolvedValueOnce(mockResponse(200, { data: expected }));

      const result = await client.check("model_selection", "summarization");

      expect(result).toEqual(expected);
      expect(mockFetch).toHaveBeenLastCalledWith(
        "http://localhost:8080/v1/check",
        expect.objectContaining({
          method: "POST",
          headers: expect.objectContaining({
            Authorization: "Bearer test-jwt-token",
            "Content-Type": "application/json",
          }),
        }),
      );
    });

    it("includes optional parameters in request body", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { has_precedent: false, precedents: [] } }),
      );

      await client.check("arch", "storage", { agentId: "other", limit: 10 });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body).toEqual({
        decision_type: "arch",
        query: "storage",
        agent_id: "other",
        limit: 10,
      });
    });
  });

  describe("trace", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("records a decision trace", async () => {
      const traceResponse = { run_id: "run-456", decision_id: "dec-789" };
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: traceResponse }),
      );

      const request: TraceRequest = {
        decisionType: "model_selection",
        outcome: "chose gpt-4o",
        confidence: 0.92,
        reasoning: "best for summarization",
        alternatives: [
          { label: "gpt-4o", selected: true, score: 0.92 },
          { label: "claude-3-haiku", selected: false, score: 0.78 },
        ],
        evidence: [
          { source_type: "benchmark", content: "94% accuracy on eval set" },
        ],
      };

      const result = await client.trace(request);

      expect(result).toEqual(traceResponse);

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.agent_id).toBe("test-agent");
      expect(body.decision.decision_type).toBe("model_selection");
      expect(body.decision.alternatives).toHaveLength(2);
      expect(body.decision.evidence).toHaveLength(1);
    });

    it("handles minimal trace request", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { run_id: "r", decision_id: "d" } }),
      );

      await client.trace({
        decisionType: "simple",
        outcome: "done",
        confidence: 1.0,
      });

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.decision.reasoning).toBeUndefined();
      expect(body.decision.alternatives).toBeUndefined();
      expect(body.decision.evidence).toBeUndefined();
    });
  });

  describe("query", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("queries with filters", async () => {
      const decisions: Decision[] = [
        {
          id: "dec-1",
          agent_id: "test",
          run_id: "run-1",
          decision_type: "model",
          outcome: "gpt-4o",
          confidence: 0.9,
          valid_from: "2024-01-01T00:00:00Z",
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { decisions, total: 1 } }),
      );

      const result = await client.query(
        { agentId: "test", decisionType: "model", minConfidence: 0.8 },
        { limit: 20, offset: 10 },
      );

      expect(result.decisions).toEqual(decisions);
      expect(result.total).toBe(1);

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.filters.agentId).toBe("test");
      expect(body.limit).toBe(20);
      expect(body.offset).toBe(10);
    });

    it("uses defaults for optional parameters", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { decisions: [], total: 0 } }),
      );

      await client.query();

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.limit).toBe(50);
      expect(body.offset).toBe(0);
      expect(body.order_by).toBe("valid_from");
      expect(body.order_dir).toBe("desc");
    });
  });

  describe("search", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("performs semantic search", async () => {
      const results = [
        {
          decision_id: "dec-1",
          decision_type: "model",
          outcome: "gpt-4o",
          confidence: 0.9,
          similarity: 0.95,
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { results } }),
      );

      const response = await client.search("which model for text tasks", 10);

      expect(response.results).toEqual(results);

      const lastCall = mockFetch.mock.calls[mockFetch.mock.calls.length - 1];
      const body = JSON.parse(lastCall[1].body);
      expect(body.query).toBe("which model for text tasks");
      expect(body.limit).toBe(10);
    });
  });

  describe("recent", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("fetches recent decisions with query params", async () => {
      const decisions: Decision[] = [
        {
          id: "dec-1",
          agent_id: "test",
          run_id: "run-1",
          decision_type: "model",
          outcome: "gpt-4o",
          confidence: 0.9,
          valid_from: "2024-01-01T00:00:00Z",
        },
      ];
      mockFetch.mockResolvedValueOnce(
        mockResponse(200, { data: { decisions } }),
      );

      const result = await client.recent({
        limit: 5,
        agentId: "test",
        decisionType: "model",
      });

      expect(result).toEqual(decisions);
      expect(mockFetch).toHaveBeenLastCalledWith(
        expect.stringContaining("/v1/decisions/recent?"),
        expect.objectContaining({ method: "GET" }),
      );
      const url = mockFetch.mock.calls[mockFetch.mock.calls.length - 1][0];
      expect(url).toContain("limit=5");
      expect(url).toContain("agent_id=test");
      expect(url).toContain("decision_type=model");
    });
  });

  describe("error handling", () => {
    beforeEach(() => {
      mockFetch.mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE));
    });

    it("maps 400 to ValidationError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(400, { error: { message: "Invalid field" } }, false),
      );

      await expect(client.check("x")).rejects.toSatisfy((err: unknown) => {
        return (
          err instanceof ValidationError && err.message === "Invalid field"
        );
      });
    });

    it("maps 401 to AuthenticationError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(401, { error: { message: "Bad token" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(AuthenticationError);
    });

    it("maps 403 to AuthorizationError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(403, { error: { message: "No access" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(AuthorizationError);
    });

    it("maps 404 to NotFoundError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(404, { error: { message: "Not found" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(NotFoundError);
    });

    it("maps 409 to ConflictError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(409, { error: { message: "Duplicate" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(ConflictError);
    });

    it("maps 5xx to ServerError", async () => {
      mockFetch.mockResolvedValueOnce(
        mockResponse(503, { error: { message: "Overloaded" } }, false),
      );

      await expect(client.check("x")).rejects.toThrow(ServerError);
    });

    it("maps unknown 4xx to AkashiError", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(418, {}, false));

      await expect(client.check("x")).rejects.toThrow(AkashiError);
    });

    it("uses fallback message when body has no error", async () => {
      mockFetch.mockResolvedValueOnce(mockResponse(401, {}, false));

      try {
        await client.check("x");
      } catch (e) {
        expect(e).toBeInstanceOf(AuthenticationError);
        expect((e as Error).message).toBe("Authentication failed");
      }
    });
  });
});

describe("withAkashi middleware", () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  it("calls check, executes function, and traces result", async () => {
    // Token, check, trace.
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(
        mockResponse(200, {
          data: { has_precedent: false, precedents: [] },
        }),
      )
      .mockResolvedValueOnce(
        mockResponse(200, { data: { run_id: "r", decision_id: "d" } }),
      );

    const client = new AkashiClient({
      baseUrl: "http://localhost:8080",
      agentId: "agent",
      apiKey: "key",
    });

    const result = await withAkashi(
      client,
      "model_selection",
      async (precedents: CheckResponse) => {
        expect(precedents.has_precedent).toBe(false);
        return {
          value: "gpt-4o",
          toTrace: (): TraceRequest => ({
            decisionType: "model_selection",
            outcome: "chose gpt-4o",
            confidence: 0.85,
          }),
        };
      },
    );

    expect(result.value).toBe("gpt-4o");
    expect(mockFetch).toHaveBeenCalledTimes(3);

    // Verify trace was called with correct body.
    const traceCall = mockFetch.mock.calls[2];
    expect(traceCall[0]).toBe("http://localhost:8080/v1/trace");
    const body = JSON.parse(traceCall[1].body);
    expect(body.decision.decision_type).toBe("model_selection");
    expect(body.decision.outcome).toBe("chose gpt-4o");
  });

  it("does not trace if function throws", async () => {
    mockFetch
      .mockResolvedValueOnce(mockResponse(200, TOKEN_RESPONSE))
      .mockResolvedValueOnce(
        mockResponse(200, { data: { has_precedent: false, precedents: [] } }),
      );

    const client = new AkashiClient({
      baseUrl: "http://localhost:8080",
      agentId: "agent",
      apiKey: "key",
    });

    await expect(
      withAkashi(client, "test", async () => {
        throw new Error("boom");
      }),
    ).rejects.toThrow("boom");

    // Only token + check, no trace.
    expect(mockFetch).toHaveBeenCalledTimes(2);
  });
});

describe("error classes", () => {
  it("AkashiError sets name and statusCode", () => {
    const err = new AkashiError("test", 500);
    expect(err.name).toBe("AkashiError");
    expect(err.statusCode).toBe(500);
    expect(err.message).toBe("test");
  });

  it("AuthenticationError defaults message", () => {
    const err = new AuthenticationError();
    expect(err.message).toBe("Authentication failed");
    expect(err.statusCode).toBe(401);
  });

  it("ServerError takes status code", () => {
    const err = new ServerError(502, "Bad gateway");
    expect(err.statusCode).toBe(502);
    expect(err.message).toBe("Bad gateway");
  });
});
