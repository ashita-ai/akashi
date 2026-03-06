/**
 * Akashi TypeScript SDK quickstart — check, trace, and query decisions.
 *
 * Prerequisites:
 *   docker compose -f docker-compose.complete.yml up -d
 *   cd examples/typescript && npm install
 *
 * Run:
 *   cd examples/typescript && npm run quickstart
 */

import { AkashiClient, ConflictError } from "akashi";
import type { TraceRequest } from "akashi";

const URL = process.env.AKASHI_URL ?? "http://localhost:8080";
const ADMIN_KEY = process.env.AKASHI_ADMIN_API_KEY ?? "admin";

async function main(): Promise<void> {
  // --- Connect as admin and verify the server is up ---
  const admin = new AkashiClient({ baseUrl: URL, agentId: "admin", apiKey: ADMIN_KEY });
  const health = await admin.health();
  console.log(`==> Connected to Akashi ${health.version} (postgres: ${health.postgres})`);

  // --- Create a demo agent (idempotent — ignores 409 if it already exists) ---
  try {
    await admin.createAgent({
      agentId: "quickstart-agent-ts",
      name: "Quickstart Agent (TS)",
      role: "agent",
      apiKey: "quickstart-secret-ts",
    });
    console.log("==> Created agent 'quickstart-agent-ts'");
  } catch (err) {
    if (err instanceof ConflictError) {
      console.log("==> Agent 'quickstart-agent-ts' already exists");
    } else {
      throw err;
    }
  }

  // --- Switch to the agent identity ---
  const client = new AkashiClient({
    baseUrl: URL,
    agentId: "quickstart-agent-ts",
    apiKey: "quickstart-secret-ts",
  });

  // --- Check: are there existing decisions about model selection? ---
  console.log("\n==> Checking for precedents on 'model_selection'...");
  const check = await client.check("model_selection");
  if (check.has_precedent) {
    console.log(`    Found ${check.decisions.length} prior decision(s)`);
    for (const d of check.decisions) {
      console.log(`    - ${d.outcome} (confidence=${d.confidence.toFixed(2)})`);
    }
  } else {
    console.log("    No prior decisions found — this will be the first.");
  }

  // --- Trace: record a new decision ---
  console.log("\n==> Tracing a model selection decision...");
  const traceReq: TraceRequest = {
    decisionType: "model_selection",
    outcome: "Use GPT-4o for summarization tasks",
    confidence: 0.85,
    reasoning:
      "GPT-4o offers the best quality-to-cost ratio for summarization. " +
      "Benchmarked against Claude and Gemini on 200 sample documents.",
    alternatives: [
      { label: "GPT-4o", score: 0.85, selected: true },
      {
        label: "Claude 3.5 Sonnet",
        score: 0.8,
        selected: false,
        rejection_reason: "Slightly higher latency on long documents",
      },
      {
        label: "Gemini 1.5 Pro",
        score: 0.7,
        selected: false,
        rejection_reason: "Inconsistent formatting in structured output",
      },
    ],
    evidence: [
      {
        source_type: "benchmark",
        content: "GPT-4o ROUGE-L: 0.47, Claude: 0.45, Gemini: 0.41 on CNN/DailyMail",
        relevance_score: 0.9,
      },
    ],
  };
  const resp = await client.trace(traceReq);
  console.log(`    Decision recorded: id=${resp.decision_id}`);

  // --- Query: retrieve decisions matching our filter ---
  console.log("\n==> Querying model_selection decisions...");
  const query = await client.query({ decision_type: "model_selection" });
  console.log(`    Found ${query.total} decision(s)`);
  for (const d of query.decisions) {
    console.log(`    - [${d.agent_id}] ${d.outcome} (confidence=${d.confidence.toFixed(2)})`);
  }

  // --- Recent: fetch the latest decisions across all types ---
  console.log("\n==> Fetching 5 most recent decisions...");
  const recent = await client.recent({ limit: 5 });
  console.log(`    Got ${recent.length} decision(s)`);
  for (const d of recent) {
    console.log(`    - [${d.decision_type}] ${d.outcome}`);
  }

  console.log(`\n==> Done. View your decisions at ${URL}`);
}

main().catch((err) => {
  console.error("Error:", err);
  process.exit(1);
});
