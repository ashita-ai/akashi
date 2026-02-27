import { describe, it, expect, vi, beforeEach } from "vitest";
import { createAkashiMiddleware } from "../src/middleware.js";
import type { AkashiClient } from "akashi";
import type {
  LanguageModelV1CallOptions,
  LanguageModelV1GenerateResponse,
  LanguageModelV1StreamPart,
  LanguageModelV1StreamResponse,
} from "ai";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeClient(): AkashiClient {
  return {
    check: vi.fn().mockResolvedValue({ has_precedent: false, decisions: [] }),
    trace: vi.fn().mockResolvedValue({ run_id: "r1", decision_id: "d1", event_count: 1 }),
  } as unknown as AkashiClient;
}

function makePrompt(userText: string): LanguageModelV1CallOptions["prompt"] {
  return [{ role: "user", content: [{ type: "text", text: userText }] }];
}

function makeGenerateResult(text: string): LanguageModelV1GenerateResponse {
  return {
    text,
    toolCalls: [],
    finishReason: "stop",
    usage: { promptTokens: 10, completionTokens: 5 },
    rawCall: { rawPrompt: "", rawSettings: {} },
    rawResponse: undefined,
    warnings: undefined,
    request: undefined,
    response: undefined,
    logprobs: undefined,
    providerMetadata: undefined,
  } as unknown as LanguageModelV1GenerateResponse;
}

function makeStreamResult(parts: LanguageModelV1StreamPart[]): LanguageModelV1StreamResponse {
  const stream = new ReadableStream<LanguageModelV1StreamPart>({
    start(controller) {
      for (const part of parts) {
        controller.enqueue(part);
      }
      controller.close();
    },
  });
  return {
    stream,
    rawCall: { rawPrompt: "", rawSettings: {} },
    rawResponse: undefined,
    warnings: undefined,
    request: undefined,
  } as unknown as LanguageModelV1StreamResponse;
}

async function drainStream(stream: ReadableStream<LanguageModelV1StreamPart>): Promise<LanguageModelV1StreamPart[]> {
  const reader = stream.getReader();
  const parts: LanguageModelV1StreamPart[] = [];
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    parts.push(value);
  }
  return parts;
}

// ---------------------------------------------------------------------------
// wrapGenerate
// ---------------------------------------------------------------------------

describe("createAkashiMiddleware — wrapGenerate", () => {
  it("calls check before generation with the last user query", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client, { decisionType: "test" });

    const doGenerate = vi.fn().mockResolvedValue(makeGenerateResult("Paris"));
    await middleware.wrapGenerate!({
      doGenerate,
      doStream: vi.fn(),
      params: { prompt: makePrompt("What is the capital of France?") } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    expect(client.check).toHaveBeenCalledOnce();
    const [dt, query] = vi.mocked(client.check).mock.calls[0] as [string, string];
    expect(dt).toBe("test");
    expect(query).toContain("capital");
  });

  it("calls trace with the generated text", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client, { decisionType: "my_agent", confidence: 0.85 });

    await middleware.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(makeGenerateResult("The answer is 42")),
      doStream: vi.fn(),
      params: { prompt: makePrompt("What?") } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    expect(client.trace).toHaveBeenCalledOnce();
    const req = vi.mocked(client.trace).mock.calls[0]![0];
    expect(req.outcome).toBe("The answer is 42");
    expect(req.decisionType).toBe("my_agent");
    expect(req.confidence).toBe(0.85);
  });

  it("returns the original generate result unchanged", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client);
    const expected = makeGenerateResult("unchanged");

    const result = await middleware.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(expected),
      doStream: vi.fn(),
      params: { prompt: [] } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    expect(result).toBe(expected);
  });

  it("skips check when checkBeforeGenerate is false", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client, { checkBeforeGenerate: false });

    await middleware.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(makeGenerateResult("ok")),
      doStream: vi.fn(),
      params: { prompt: makePrompt("question") } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    expect(client.check).not.toHaveBeenCalled();
  });

  it("skips trace when traceGenerations is false", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client, { traceGenerations: false });

    await middleware.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(makeGenerateResult("ok")),
      doStream: vi.fn(),
      params: { prompt: [] } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    expect(client.trace).not.toHaveBeenCalled();
  });

  it("akashi check failure does not interrupt generation", async () => {
    const client = makeClient();
    vi.mocked(client.check).mockRejectedValue(new Error("akashi is down"));
    const middleware = createAkashiMiddleware(client);

    // Must not throw.
    const result = await middleware.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(makeGenerateResult("result")),
      doStream: vi.fn(),
      params: { prompt: makePrompt("q") } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    expect(result.text).toBe("result");
  });

  it("akashi trace failure does not interrupt generation", async () => {
    const client = makeClient();
    vi.mocked(client.trace).mockRejectedValue(new Error("akashi is down"));
    const middleware = createAkashiMiddleware(client);

    // Must not throw.
    const result = await middleware.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(makeGenerateResult("result")),
      doStream: vi.fn(),
      params: { prompt: [] } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    expect(result.text).toBe("result");
  });

  it("truncates outcome to 500 characters", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client);
    const longText = "x".repeat(1000);

    await middleware.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(makeGenerateResult(longText)),
      doStream: vi.fn(),
      params: { prompt: [] } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    const req = vi.mocked(client.trace).mock.calls[0]![0];
    expect(req.outcome.length).toBe(500);
  });
});

// ---------------------------------------------------------------------------
// wrapStream
// ---------------------------------------------------------------------------

describe("createAkashiMiddleware — wrapStream", () => {
  it("passes stream parts through unchanged", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client);
    const parts: LanguageModelV1StreamPart[] = [
      { type: "text-delta", textDelta: "Hello" },
      { type: "text-delta", textDelta: " world" },
      { type: "finish", finishReason: "stop", usage: { promptTokens: 5, completionTokens: 2 } },
    ];

    const { stream } = await middleware.wrapStream!({
      doStream: vi.fn().mockResolvedValue(makeStreamResult(parts)),
      doGenerate: vi.fn(),
      params: { prompt: [] } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    const emitted = await drainStream(stream);
    expect(emitted).toEqual(parts);
  });

  it("traces accumulated text after stream is drained", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client, { decisionType: "stream_test" });
    const parts: LanguageModelV1StreamPart[] = [
      { type: "text-delta", textDelta: "Hello " },
      { type: "text-delta", textDelta: "world" },
    ];

    const { stream } = await middleware.wrapStream!({
      doStream: vi.fn().mockResolvedValue(makeStreamResult(parts)),
      doGenerate: vi.fn(),
      params: { prompt: [] } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    await drainStream(stream);

    expect(client.trace).toHaveBeenCalledOnce();
    const req = vi.mocked(client.trace).mock.calls[0]![0];
    expect(req.outcome).toBe("Hello world");
    expect(req.decisionType).toBe("stream_test");
  });

  it("calls check before stream starts", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client, { decisionType: "check_stream" });

    const { stream } = await middleware.wrapStream!({
      doStream: vi.fn().mockResolvedValue(makeStreamResult([])),
      doGenerate: vi.fn(),
      params: { prompt: makePrompt("stream question") } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    await drainStream(stream);

    expect(client.check).toHaveBeenCalledOnce();
    const [dt] = vi.mocked(client.check).mock.calls[0] as [string, string];
    expect(dt).toBe("check_stream");
  });

  it("skips stream trace when traceStreams is false", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client, { traceStreams: false });
    const parts: LanguageModelV1StreamPart[] = [
      { type: "text-delta", textDelta: "text" },
    ];

    const { stream } = await middleware.wrapStream!({
      doStream: vi.fn().mockResolvedValue(makeStreamResult(parts)),
      doGenerate: vi.fn(),
      params: { prompt: [] } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    await drainStream(stream);

    expect(client.trace).not.toHaveBeenCalled();
  });

  it("does not trace when stream produces no text", async () => {
    const client = makeClient();
    const middleware = createAkashiMiddleware(client);

    const { stream } = await middleware.wrapStream!({
      doStream: vi.fn().mockResolvedValue(makeStreamResult([
        { type: "finish", finishReason: "stop", usage: { promptTokens: 1, completionTokens: 0 } },
      ])),
      doGenerate: vi.fn(),
      params: { prompt: [] } as unknown as LanguageModelV1CallOptions,
      model: {} as never,
    });

    await drainStream(stream);

    expect(client.trace).not.toHaveBeenCalled();
  });
});
