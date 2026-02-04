import type { KyoyuClient } from "./client.js";
import type { CheckResponse, Traceable } from "./types.js";

/**
 * Wraps a decision-making function with automatic check-before/record-after.
 *
 * 1. Calls `client.check()` for the given decision type.
 * 2. Invokes `fn` with the precedent check results.
 * 3. Calls `client.trace()` with the result's trace representation.
 *
 * The function must return an object implementing the `Traceable` interface
 * (i.e., it has a `toTrace()` method that returns a `TraceRequest`).
 *
 * @example
 * ```ts
 * const result = await withKyoyu(client, "model_selection", async (precedents) => {
 *   // Use precedents to inform your decision...
 *   return {
 *     value: "gpt-4o",
 *     toTrace: () => ({
 *       decisionType: "model_selection",
 *       outcome: "chose gpt-4o for summarization",
 *       confidence: 0.85,
 *       reasoning: "Best quality-to-cost ratio",
 *     }),
 *   };
 * });
 * ```
 */
export async function withKyoyu<T extends Traceable>(
  client: KyoyuClient,
  decisionType: string,
  fn: (precedents: CheckResponse) => Promise<T>,
): Promise<T> {
  const precedents = await client.check(decisionType);
  const result = await fn(precedents);
  await client.trace(result.toTrace());
  return result;
}
