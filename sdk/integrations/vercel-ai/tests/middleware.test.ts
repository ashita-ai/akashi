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
// Factories
// ---------------------------------------------------------------------------

function makeClient(): AkashiClient {
  return {
    check: vi.fn().mockResolvedValue({ has_precedent: false, decisions: [] }),
    trace: vi.fn().mockResolvedValue({ run_id: "r1", decision_id: "d1", event_count: 1 }),
  } as unknown as AkashiClient;
}

function makeTextPrompt(text: string): LanguageModelV1CallOptions["prompt"] {
  return [{ role: "user", content: [{ type: "text", text }] }];
}

function makeStringPrompt(text: string): LanguageModelV1CallOptions["prompt"] {
  // Some messages pass content as a plain string (system messages).
  return [{ role: "user", content: text as unknown as Array<{ type: string; text: string }> }];
}

function makeSystemPrompt(text: string): LanguageModelV1CallOptions["prompt"] {
  return [{ role: "system", content: text }];
}

function makeMultiTurnPrompt(userText: string): LanguageModelV1CallOptions["prompt"] {
  return [
    { role: "user", content: [{ type: "text", text: "first message" }] },
    { role: "assistant", content: [{ type: "text", text: "response" }] },
    { role: "user", content: [{ type: "text", text: userText }] },
  ];
}

function generateResult(overrides: Partial<LanguageModelV1GenerateResponse> = {}): LanguageModelV1GenerateResponse {
  return {
    text: "the answer",
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
    ...overrides,
  } as unknown as LanguageModelV1GenerateResponse;
}

function streamResult(parts: LanguageModelV1StreamPart[]): LanguageModelV1StreamResponse {
  return {
    stream: new ReadableStream({
      start(controller) {
        for (const p of parts) controller.enqueue(p);
        controller.close();
      },
    }),
    rawCall: { rawPrompt: "", rawSettings: {} },
    rawResponse: undefined,
    warnings: undefined,
    request: undefined,
  } as unknown as LanguageModelV1StreamResponse;
}

async function drain(stream: ReadableStream<LanguageModelV1StreamPart>): Promise<LanguageModelV1StreamPart[]> {
  const reader = stream.getReader();
  const out: LanguageModelV1StreamPart[] = [];
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    out.push(value);
  }
  return out;
}

function callParams(prompt: LanguageModelV1CallOptions["prompt"]): LanguageModelV1CallOptions {
  return { prompt } as unknown as LanguageModelV1CallOptions;
}

// Convenience: run wrapGenerate with a simple text prompt.
async function runGenerate(
  client: AkashiClient,
  text: string,
  resultOverrides: Partial<LanguageModelV1GenerateResponse> = {},
  options = {},
) {
  const mw = createAkashiMiddleware(client, options);
  return mw.wrapGenerate!({
    doGenerate: vi.fn().mockResolvedValue(generateResult(resultOverrides)),
    doStream: vi.fn(),
    params: callParams(makeTextPrompt(text)),
    model: {} as never,
  });
}

// ---------------------------------------------------------------------------
// wrapGenerate — check() behaviour
// ---------------------------------------------------------------------------

describe("wrapGenerate — check", () => {
  it("calls check once per generation", async () => {
    const client = makeClient();
    await runGenerate(client, "question");
    expect(client.check).toHaveBeenCalledOnce();
  });

  it("passes decision_type to check", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client, { decisionType: "my_llm_call" });
    await mw.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(generateResult()),
      doStream: vi.fn(),
      params: callParams(makeTextPrompt("q")),
      model: {} as never,
    });
    const [dt] = vi.mocked(client.check).mock.calls[0] as [string, string | undefined];
    expect(dt).toBe("my_llm_call");
  });

  it("extracts last user message text as query", async () => {
    const client = makeClient();
    await runGenerate(client, "What is the weather?");
    const [, query] = vi.mocked(client.check).mock.calls[0] as [string, string];
    expect(query).toContain("weather");
  });

  it("extracts last user message from multi-turn prompt", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    await mw.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(generateResult()),
      doStream: vi.fn(),
      params: callParams(makeMultiTurnPrompt("final question")),
      model: {} as never,
    });
    const [, query] = vi.mocked(client.check).mock.calls[0] as [string, string];
    expect(query).toContain("final question");
    expect(query).not.toContain("first message");
  });

  it("uses undefined query when prompt has no user message", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    await mw.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(generateResult()),
      doStream: vi.fn(),
      params: callParams(makeSystemPrompt("be helpful")),
      model: {} as never,
    });
    const [, query] = vi.mocked(client.check).mock.calls[0] as [string, string | undefined];
    expect(query).toBeUndefined();
  });

  it("uses undefined query when prompt is empty", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    await mw.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(generateResult()),
      doStream: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    const [, query] = vi.mocked(client.check).mock.calls[0] as [string, string | undefined];
    expect(query).toBeUndefined();
  });

  it("truncates check query to 200 chars", async () => {
    const client = makeClient();
    await runGenerate(client, "x".repeat(500));
    const [, query] = vi.mocked(client.check).mock.calls[0] as [string, string];
    expect(query!.length).toBeLessThanOrEqual(200);
  });

  it("skips check when checkBeforeGenerate is false", async () => {
    const client = makeClient();
    await runGenerate(client, "q", {}, { checkBeforeGenerate: false });
    expect(client.check).not.toHaveBeenCalled();
  });

  it("check failure does not interrupt doGenerate", async () => {
    const client = makeClient();
    vi.mocked(client.check).mockRejectedValue(new Error("check down"));
    const result = await runGenerate(client, "q", { text: "safe answer" });
    expect(result.text).toBe("safe answer");
  });
});

// ---------------------------------------------------------------------------
// wrapGenerate — trace() behaviour
// ---------------------------------------------------------------------------

describe("wrapGenerate — trace", () => {
  it("calls trace once per generation", async () => {
    const client = makeClient();
    await runGenerate(client, "q");
    expect(client.trace).toHaveBeenCalledOnce();
  });

  it("traces generated text as outcome", async () => {
    const client = makeClient();
    await runGenerate(client, "q", { text: "Paris" });
    const req = vi.mocked(client.trace).mock.calls[0]![0];
    expect(req.outcome).toBe("Paris");
  });

  it("traces decision_type from options", async () => {
    const client = makeClient();
    await runGenerate(client, "q", {}, { decisionType: "test_dt" });
    expect(vi.mocked(client.trace).mock.calls[0]![0].decisionType).toBe("test_dt");
  });

  it("default decision_type is llm_call", async () => {
    const client = makeClient();
    await runGenerate(client, "q");
    expect(vi.mocked(client.trace).mock.calls[0]![0].decisionType).toBe("llm_call");
  });

  it("traces confidence from options", async () => {
    const client = makeClient();
    await runGenerate(client, "q", {}, { confidence: 0.9 });
    expect(vi.mocked(client.trace).mock.calls[0]![0].confidence).toBe(0.9);
  });

  it("default confidence is 0.7", async () => {
    const client = makeClient();
    await runGenerate(client, "q");
    expect(vi.mocked(client.trace).mock.calls[0]![0].confidence).toBe(0.7);
  });

  it("truncates outcome to 500 chars", async () => {
    const client = makeClient();
    await runGenerate(client, "q", { text: "z".repeat(1000) });
    expect(vi.mocked(client.trace).mock.calls[0]![0].outcome.length).toBe(500);
  });

  it("does not truncate outcome under 500 chars", async () => {
    const client = makeClient();
    await runGenerate(client, "q", { text: "short answer" });
    expect(vi.mocked(client.trace).mock.calls[0]![0].outcome).toBe("short answer");
  });

  it("traces tool call names when text is absent", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    await mw.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(generateResult({
        text: undefined,
        toolCalls: [
          { toolCallType: "function" as const, toolCallId: "1", toolName: "search", args: { q: "hi" } },
          { toolCallType: "function" as const, toolCallId: "2", toolName: "calc", args: {} },
        ],
      })),
      doStream: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    const outcome = vi.mocked(client.trace).mock.calls[0]![0].outcome;
    expect(outcome).toContain("search");
    expect(outcome).toContain("calc");
  });

  it("skips trace when traceGenerations is false", async () => {
    const client = makeClient();
    await runGenerate(client, "q", {}, { traceGenerations: false });
    expect(client.trace).not.toHaveBeenCalled();
  });

  it("trace failure does not interrupt generation", async () => {
    const client = makeClient();
    vi.mocked(client.trace).mockRejectedValue(new Error("trace down"));
    const result = await runGenerate(client, "q", { text: "still works" });
    expect(result.text).toBe("still works");
  });

  it("returns the exact doGenerate result", async () => {
    const client = makeClient();
    const expected = generateResult({ text: "exact" });
    const mw = createAkashiMiddleware(client);
    const result = await mw.wrapGenerate!({
      doGenerate: vi.fn().mockResolvedValue(expected),
      doStream: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    expect(result).toBe(expected);
  });
});

// ---------------------------------------------------------------------------
// wrapStream — passthrough
// ---------------------------------------------------------------------------

describe("wrapStream — passthrough", () => {
  it("emits all stream parts unchanged", async () => {
    const client = makeClient();
    const parts: LanguageModelV1StreamPart[] = [
      { type: "text-delta", textDelta: "Hello" },
      { type: "text-delta", textDelta: " world" },
      { type: "finish", finishReason: "stop", usage: { promptTokens: 5, completionTokens: 3 } },
    ];
    const mw = createAkashiMiddleware(client);
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult(parts)),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    const emitted = await drain(stream);
    expect(emitted).toEqual(parts);
  });

  it("preserves all properties of the stream result besides stream", async () => {
    const client = makeClient();
    const original = streamResult([]);
    const mw = createAkashiMiddleware(client);
    const result = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(original),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    // rawCall should be preserved.
    expect(result.rawCall).toBe(original.rawCall);
  });
});

// ---------------------------------------------------------------------------
// wrapStream — check() behaviour
// ---------------------------------------------------------------------------

describe("wrapStream — check", () => {
  it("calls check before stream starts", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([])),
      doGenerate: vi.fn(),
      params: callParams(makeTextPrompt("stream question")),
      model: {} as never,
    });
    // check is called at wrapStream invocation time, before any reading.
    expect(client.check).toHaveBeenCalledOnce();
    await drain(stream);
  });

  it("extracts last user message as check query", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client, { decisionType: "sd" });
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([])),
      doGenerate: vi.fn(),
      params: callParams(makeTextPrompt("streaming topic")),
      model: {} as never,
    });
    await drain(stream);
    const [, query] = vi.mocked(client.check).mock.calls[0] as [string, string];
    expect(query).toContain("streaming topic");
  });

  it("skips check when checkBeforeGenerate is false", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client, { checkBeforeGenerate: false });
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([])),
      doGenerate: vi.fn(),
      params: callParams(makeTextPrompt("q")),
      model: {} as never,
    });
    await drain(stream);
    expect(client.check).not.toHaveBeenCalled();
  });

  it("check failure does not interrupt stream", async () => {
    const client = makeClient();
    vi.mocked(client.check).mockRejectedValue(new Error("check down"));
    const mw = createAkashiMiddleware(client);
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([
        { type: "text-delta", textDelta: "text" },
      ])),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    const parts = await drain(stream);
    expect(parts).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// wrapStream — trace() behaviour
// ---------------------------------------------------------------------------

describe("wrapStream — trace", () => {
  it("traces accumulated text after stream is drained", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client, { decisionType: "stream_dt" });
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([
        { type: "text-delta", textDelta: "Hello " },
        { type: "text-delta", textDelta: "world" },
      ])),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    await drain(stream);
    expect(client.trace).toHaveBeenCalledOnce();
    const req = vi.mocked(client.trace).mock.calls[0]![0];
    expect(req.outcome).toBe("Hello world");
    expect(req.decisionType).toBe("stream_dt");
  });

  it("accumulates text-delta parts in order", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([
        { type: "text-delta", textDelta: "one" },
        { type: "text-delta", textDelta: "two" },
        { type: "text-delta", textDelta: "three" },
      ])),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    await drain(stream);
    expect(vi.mocked(client.trace).mock.calls[0]![0].outcome).toBe("onetwothree");
  });

  it("truncates stream outcome to 500 chars", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([
        { type: "text-delta", textDelta: "a".repeat(1000) },
      ])),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    await drain(stream);
    expect(vi.mocked(client.trace).mock.calls[0]![0].outcome.length).toBe(500);
  });

  it("confidence from options flows through to trace", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client, { confidence: 0.88 });
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([
        { type: "text-delta", textDelta: "t" },
      ])),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    await drain(stream);
    expect(vi.mocked(client.trace).mock.calls[0]![0].confidence).toBe(0.88);
  });

  it("does not trace when stream produces no text", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([
        { type: "finish", finishReason: "stop", usage: { promptTokens: 1, completionTokens: 0 } },
      ])),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    await drain(stream);
    expect(client.trace).not.toHaveBeenCalled();
  });

  it("ignores non-text-delta parts when accumulating", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client);
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([
        { type: "text-delta", textDelta: "A" },
        { type: "finish", finishReason: "stop", usage: { promptTokens: 1, completionTokens: 1 } },
        { type: "text-delta", textDelta: "B" },
      ])),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    await drain(stream);
    expect(vi.mocked(client.trace).mock.calls[0]![0].outcome).toBe("AB");
  });

  it("skips trace when traceStreams is false", async () => {
    const client = makeClient();
    const mw = createAkashiMiddleware(client, { traceStreams: false });
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult([
        { type: "text-delta", textDelta: "text" },
      ])),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    await drain(stream);
    expect(client.trace).not.toHaveBeenCalled();
  });

  it("trace failure does not corrupt the stream output", async () => {
    const client = makeClient();
    vi.mocked(client.trace).mockRejectedValue(new Error("trace down"));
    const mw = createAkashiMiddleware(client);
    const parts: LanguageModelV1StreamPart[] = [
      { type: "text-delta", textDelta: "safe" },
    ];
    const { stream } = await mw.wrapStream!({
      doStream: vi.fn().mockResolvedValue(streamResult(parts)),
      doGenerate: vi.fn(),
      params: callParams([]),
      model: {} as never,
    });
    const emitted = await drain(stream);
    expect(emitted).toEqual(parts);
  });
});

// ---------------------------------------------------------------------------
// Both disabled
// ---------------------------------------------------------------------------

describe("all disabled", () => {
  it("no check and no trace when both are disabled", async () => {
    const client = makeClient();
    await runGenerate(client, "q", {}, {
      checkBeforeGenerate: false,
      traceGenerations: false,
    });
    expect(client.check).not.toHaveBeenCalled();
    expect(client.trace).not.toHaveBeenCalled();
  });
});
