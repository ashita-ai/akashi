/**
 * Akashi middleware pattern — automatic check-before / record-after.
 *
 * Demonstrates withAkashi(), which:
 *   1. Calls check() for precedents
 *   2. Passes precedents to your decision function
 *   3. Calls trace() with the result
 *
 * Your function must return an object with a toTrace() method (the Traceable interface).
 *
 * Prerequisites:
 *   docker compose -f docker-compose.complete.yml up -d
 *   cd examples/typescript && npm install
 *
 * Run:
 *   cd examples/typescript && npm run middleware
 */

import { AkashiClient, ConflictError, withAkashi } from "akashi";
import type { CheckResponse, Traceable, TraceRequest } from "akashi";

const URL = process.env.AKASHI_URL ?? "http://localhost:8080";
const ADMIN_KEY = process.env.AKASHI_ADMIN_API_KEY ?? "admin";

const DATABASES = ["PostgreSQL", "MongoDB", "DynamoDB"];

interface DatabaseChoice extends Traceable {
  database: string;
  confidence: number;
  reasoning: string;
}

function makeDatabaseChoice(
  database: string,
  confidence: number,
  reasoning: string,
): DatabaseChoice {
  return {
    database,
    confidence,
    reasoning,
    toTrace(): TraceRequest {
      return {
        decisionType: "database_selection",
        outcome: `chose ${database}`,
        confidence,
        reasoning,
        alternatives: DATABASES.map((db) => ({
          label: db,
          selected: db === database,
        })),
      };
    },
  };
}

async function main(): Promise<void> {
  const admin = new AkashiClient({ baseUrl: URL, agentId: "admin", apiKey: ADMIN_KEY });
  const health = await admin.health();
  console.log(`==> Connected to Akashi ${health.version}`);

  try {
    await admin.createAgent({
      agentId: "middleware-agent-ts",
      name: "Middleware Agent (TS)",
      role: "agent",
      apiKey: "middleware-secret-ts",
    });
    console.log("==> Created agent 'middleware-agent-ts'");
  } catch (err) {
    if (err instanceof ConflictError) {
      console.log("==> Agent 'middleware-agent-ts' already exists");
    } else {
      throw err;
    }
  }

  const client = new AkashiClient({
    baseUrl: URL,
    agentId: "middleware-agent-ts",
    apiKey: "middleware-secret-ts",
  });

  // First call — no precedents exist yet.
  console.log("\n==> First decision (no precedents)...");
  const result1 = await withAkashi(client, "database_selection", async (precedents: CheckResponse) => {
    if (precedents.has_precedent) {
      const best = precedents.decisions.reduce((a, b) =>
        a.confidence > b.confidence ? a : b,
      );
      if (best.confidence > 0.7) {
        console.log(`    Reusing precedent: ${best.outcome} (confidence=${best.confidence.toFixed(2)})`);
        return makeDatabaseChoice(
          best.outcome.replace("chose ", ""),
          best.confidence,
          `Reused precedent from ${best.agent_id}`,
        );
      }
    }
    console.log("    No strong precedent — deciding from scratch.");
    return makeDatabaseChoice(
      "PostgreSQL",
      0.9,
      "ACID compliance, mature ecosystem, and excellent extension support",
    );
  });
  console.log(`    Result: ${result1.database} (confidence=${result1.confidence.toFixed(2)})`);

  // Second call — the first decision is now a precedent.
  console.log("\n==> Second decision (should find precedent)...");
  const result2 = await withAkashi(client, "database_selection", async (precedents: CheckResponse) => {
    if (precedents.has_precedent) {
      const best = precedents.decisions.reduce((a, b) =>
        a.confidence > b.confidence ? a : b,
      );
      if (best.confidence > 0.7) {
        console.log(`    Reusing precedent: ${best.outcome} (confidence=${best.confidence.toFixed(2)})`);
        return makeDatabaseChoice(
          best.outcome.replace("chose ", ""),
          best.confidence,
          `Reused precedent from ${best.agent_id}`,
        );
      }
    }
    console.log("    No strong precedent — deciding from scratch.");
    return makeDatabaseChoice(
      "PostgreSQL",
      0.9,
      "ACID compliance, mature ecosystem, and excellent extension support",
    );
  });
  console.log(`    Result: ${result2.database} (confidence=${result2.confidence.toFixed(2)})`);

  console.log("\n==> Done. Both decisions are now in the audit trail.");
}

main().catch((err) => {
  console.error("Error:", err);
  process.exit(1);
});
