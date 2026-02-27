/**
 * Vercel AI SDK middleware for Akashi decision tracing.
 *
 * Wraps a language model so that:
 * - Before each generation, `check()` is called to surface relevant precedents.
 * - After each generation (streaming or not), `trace()` records the LLM output
 *   as a decision.
 *
 * All Akashi calls are fire-and-forget: errors are swallowed silently so they
 * never interrupt the model call.
 */

import type { AkashiClient } from "akashi";
import type {
  LanguageModelV1Middleware,
  LanguageModelV1StreamPart,
} from "ai";

const MAX_OUTCOME = 500;
const MAX_QUERY = 200;

/** Options for {@link createAkashiMiddleware}. */
export interface AkashiMiddlewareOptions {
  /**
   * Decision type label recorded for every trace. Defaults to `"llm_call"`.
   */
  decisionType?: string;

  /**
   * Default confidence score (0–1) for traced decisions. Defaults to `0.7`.
   */
  confidence?: number;

  /**
   * If `true`, call `check()` before each generation to surface precedents.
   * Defaults to `true`.
   */
  checkBeforeGenerate?: boolean;

  /**
   * If `true`, call `trace()` after each completed non-streaming generation.
   * Defaults to `true`.
   */
  traceGenerations?: boolean;

  /**
   * If `true`, call `trace()` after each completed streaming generation.
   * Defaults to `true`.
   */
  traceStreams?: boolean;
}

/**
 * Extract the text of the last user message from the prompt, for use as the
 * `check()` query.  Returns `undefined` if the prompt is empty or the last
 * message has no extractable text.
 */
function extractLastUserQuery(
  prompt: ReadonlyArray<{ role: string; content: unknown }>,
): string | undefined {
  for (let i = prompt.length - 1; i >= 0; i--) {
    const msg = prompt[i];
    if (msg?.role !== "user") continue;

    const content = msg.content;
    if (typeof content === "string") {
      return content.slice(0, MAX_QUERY);
    }
    if (Array.isArray(content)) {
      const text = (content as Array<{ type?: string; text?: string }>)
        .filter((p) => p.type === "text" && typeof p.text === "string")
        .map((p) => p.text as string)
        .join(" ");
      return text.slice(0, MAX_QUERY) || undefined;
    }
  }
  return undefined;
}

async function safeCheck(
  client: AkashiClient,
  decisionType: string,
  query: string | undefined,
): Promise<void> {
  try {
    await client.check(decisionType, query);
  } catch {
    // Never interrupt the model call.
  }
}

async function safeTrace(
  client: AkashiClient,
  decisionType: string,
  outcome: string,
  confidence: number,
  reasoning?: string,
): Promise<void> {
  try {
    await client.trace({
      decisionType,
      outcome: outcome.slice(0, MAX_OUTCOME),
      confidence,
      reasoning,
    });
  } catch {
    // Never interrupt the model call.
  }
}

/**
 * Build a Vercel AI SDK middleware that integrates with Akashi.
 *
 * Usage:
 * ```ts
 * import { wrapLanguageModel } from "ai";
 * import { openai } from "@ai-sdk/openai";
 * import { AkashiClient } from "akashi";
 * import { createAkashiMiddleware } from "akashi-vercel-ai";
 *
 * const akashi = new AkashiClient({
 *   baseUrl: "https://your-akashi.example.com",
 *   agentId: "my-agent",
 *   apiKey: process.env.AKASHI_API_KEY!,
 * });
 *
 * const model = wrapLanguageModel({
 *   model: openai("gpt-4o"),
 *   middleware: createAkashiMiddleware(akashi),
 * });
 *
 * // Use model in generateText, streamText, etc. — Akashi tracing is automatic.
 * ```
 */
export function createAkashiMiddleware(
  client: AkashiClient,
  options: AkashiMiddlewareOptions = {},
): LanguageModelV1Middleware {
  const {
    decisionType = "llm_call",
    confidence = 0.7,
    checkBeforeGenerate = true,
    traceGenerations = true,
    traceStreams = true,
  } = options;

  return {
    // ------------------------------------------------------------------
    // Non-streaming (generateText / generateObject)
    // ------------------------------------------------------------------
    wrapGenerate: async ({ doGenerate, params }) => {
      if (checkBeforeGenerate) {
        const query = extractLastUserQuery(
          params.prompt as Array<{ role: string; content: unknown }>,
        );
        await safeCheck(client, decisionType, query);
      }

      const result = await doGenerate();

      if (traceGenerations) {
        // Prefer generated text; fall back to listing tool calls.
        const text =
          result.text ??
          result.toolCalls
            ?.map((c) => `tool:${c.toolName}(${JSON.stringify(c.args).slice(0, 100)})`)
            .join(", ") ??
          "";
        await safeTrace(client, decisionType, text, confidence);
      }

      return result;
    },

    // ------------------------------------------------------------------
    // Streaming (streamText / streamObject)
    // ------------------------------------------------------------------
    wrapStream: async ({ doStream, params }) => {
      if (checkBeforeGenerate) {
        const query = extractLastUserQuery(
          params.prompt as Array<{ role: string; content: unknown }>,
        );
        await safeCheck(client, decisionType, query);
      }

      const result = await doStream();

      if (!traceStreams) {
        return result;
      }

      // Pipe the original stream through a TransformStream that accumulates
      // text-delta parts and traces on flush (i.e., when the stream closes).
      let accumulated = "";

      const tracingTransform = new TransformStream<
        LanguageModelV1StreamPart,
        LanguageModelV1StreamPart
      >({
        transform(chunk, controller) {
          if (chunk.type === "text-delta") {
            accumulated += chunk.textDelta;
          }
          controller.enqueue(chunk);
        },
        async flush() {
          if (accumulated) {
            await safeTrace(client, decisionType, accumulated, confidence);
          }
        },
      });

      return {
        ...result,
        stream: result.stream.pipeThrough(tracingTransform),
      };
    },
  };
}
