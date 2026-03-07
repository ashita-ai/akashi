/**
 * Akashi + Vercel AI SDK — automatic decision tracing via middleware.
 *
 * Demonstrates createAkashiMiddleware, which wraps any Vercel AI SDK model
 * so that every generateText / streamText call is automatically traced to
 * Akashi without any explicit trace() calls.
 *
 * Prerequisites:
 *   docker compose -f docker-compose.complete.yml up -d
 *   export OPENAI_API_KEY=sk-...
 *   cd examples/vercel-ai && npm install
 *
 * Run:
 *   cd examples/vercel-ai && npm start
 */

import { generateText, streamText, wrapLanguageModel } from "ai";
import { openai } from "@ai-sdk/openai";
import { AkashiClient, ConflictError } from "akashi";
import { createAkashiMiddleware } from "akashi-vercel-ai";

const AKASHI_URL = process.env.AKASHI_URL ?? "http://localhost:8080";
const ADMIN_KEY = process.env.AKASHI_ADMIN_API_KEY ?? "admin";
const AGENT_ID = "vercel-ai-example";
const AGENT_KEY = "vercel-ai-secret";

async function main(): Promise<void> {
  // -----------------------------------------------------------------------
  // 1. Connect to Akashi and create a demo agent
  // -----------------------------------------------------------------------
  const admin = new AkashiClient({ baseUrl: AKASHI_URL, agentId: "admin", apiKey: ADMIN_KEY });
  const health = await admin.health();
  console.log(`==> Connected to Akashi ${health.version} (postgres: ${health.postgres})`);

  try {
    await admin.createAgent({
      agentId: AGENT_ID,
      name: "Vercel AI Example Agent",
      role: "agent",
      apiKey: AGENT_KEY,
    });
    console.log(`==> Created agent '${AGENT_ID}'`);
  } catch (err) {
    if (err instanceof ConflictError) {
      console.log(`==> Agent '${AGENT_ID}' already exists`);
    } else {
      throw err;
    }
  }

  const client = new AkashiClient({
    baseUrl: AKASHI_URL,
    agentId: AGENT_ID,
    apiKey: AGENT_KEY,
  });

  // -----------------------------------------------------------------------
  // 2. Wrap the OpenAI model with Akashi middleware
  // -----------------------------------------------------------------------
  const model = wrapLanguageModel({
    model: openai("gpt-4o-mini"),
    middleware: createAkashiMiddleware(client, {
      decisionType: "llm_call",
      confidence: 0.8,
    }),
  });

  console.log("\n--- generateText (non-streaming) ---\n");

  // -----------------------------------------------------------------------
  // 3. generateText — middleware calls check() before and trace() after
  // -----------------------------------------------------------------------
  const { text } = await generateText({
    model,
    prompt: "In one sentence, what is the CAP theorem?",
  });
  console.log(`Response: ${text}`);

  console.log("\n--- streamText (streaming) ---\n");

  // -----------------------------------------------------------------------
  // 4. streamText — middleware traces after the stream closes
  // -----------------------------------------------------------------------
  process.stdout.write("Response: ");
  const stream = await streamText({
    model,
    prompt: "List three benefits of decision audit trails in two sentences.",
  });
  for await (const chunk of stream.textStream) {
    process.stdout.write(chunk);
  }
  console.log("\n");

  // -----------------------------------------------------------------------
  // 5. Verify — query the audit trail to confirm auto-traced decisions
  // -----------------------------------------------------------------------
  // Small delay so async trace from stream flush completes.
  await new Promise((resolve) => setTimeout(resolve, 1000));

  console.log("--- Verifying audit trail ---\n");
  const recent = await client.recent({ limit: 5, decisionType: "llm_call" });
  console.log(`Found ${recent.length} auto-traced decision(s):`);
  for (const d of recent) {
    const outcome = d.outcome.length > 80 ? d.outcome.slice(0, 80) + "..." : d.outcome;
    console.log(`  - [${d.decision_type}] ${outcome}`);
    console.log(`    confidence=${d.confidence.toFixed(2)}, agent=${d.agent_id}`);
  }

  console.log(`\n==> Done. No explicit trace() calls were made — the middleware handled it.`);
  console.log(`==> View your decisions at ${AKASHI_URL}`);
}

main().catch((err) => {
  console.error("Error:", err);
  process.exit(1);
});
