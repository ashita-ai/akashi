#!/usr/bin/env python3
"""Akashi middleware pattern — automatic check-before / record-after.

Demonstrates AkashiSyncMiddleware.wrap(), which:
  1. Calls check() for precedents
  2. Passes precedents to your decision function
  3. Calls trace() with the result

Your function must return an object with a to_trace() method (the Traceable protocol).

Prerequisites:
    docker compose -f docker-compose.complete.yml up -d
    pip install -e sdk/python   # from repo root

Run:
    python examples/python/middleware.py
"""

import os
import sys
from dataclasses import dataclass

from akashi import (
    AkashiSyncClient,
    AkashiSyncMiddleware,
    CheckResponse,
    ConflictError,
    CreateAgentRequest,
    TraceAlternative,
    TraceRequest,
)

URL = os.environ.get("AKASHI_URL", "http://localhost:8080")
ADMIN_KEY = os.environ.get("AKASHI_ADMIN_API_KEY", "admin")

DATABASES = ["PostgreSQL", "MongoDB", "DynamoDB"]


@dataclass
class DatabaseChoice:
    """A decision result that satisfies the Traceable protocol."""

    database: str
    confidence: float
    reasoning: str

    def to_trace(self) -> TraceRequest:
        return TraceRequest(
            decision_type="database_selection",
            outcome=f"chose {self.database}",
            confidence=self.confidence,
            reasoning=self.reasoning,
            alternatives=[
                TraceAlternative(label=db, selected=(db == self.database))
                for db in DATABASES
            ],
        )


def choose_database(*, precedents: CheckResponse) -> DatabaseChoice:
    """Pick a database, informed by any prior decisions."""
    if precedents.has_precedent:
        best = max(precedents.decisions, key=lambda d: d.confidence)
        if best.confidence > 0.7:
            print(f"    Reusing precedent: {best.outcome} (confidence={best.confidence:.2f})")
            return DatabaseChoice(
                database=best.outcome.removeprefix("chose "),
                confidence=best.confidence,
                reasoning=f"Reused precedent from {best.agent_id}",
            )

    # No strong precedent — make a fresh decision.
    print("    No strong precedent — deciding from scratch.")
    return DatabaseChoice(
        database="PostgreSQL",
        confidence=0.9,
        reasoning="ACID compliance, mature ecosystem, and excellent extension support",
    )


def main() -> None:
    admin = AkashiSyncClient(base_url=URL, agent_id="admin", api_key=ADMIN_KEY)
    health = admin.health()
    print(f"==> Connected to Akashi {health.version}")

    try:
        admin.create_agent(CreateAgentRequest(
            agent_id="middleware-agent",
            name="Middleware Agent",
            role="agent",
            api_key="middleware-secret",
        ))
        print("==> Created agent 'middleware-agent'")
    except ConflictError:
        print("==> Agent 'middleware-agent' already exists")

    client = AkashiSyncClient(
        base_url=URL, agent_id="middleware-agent", api_key="middleware-secret",
    )
    middleware = AkashiSyncMiddleware(client=client)

    # First call — no precedents exist yet.
    print("\n==> First decision (no precedents)...")
    result1 = middleware.wrap("database_selection", choose_database)
    print(f"    Result: {result1.database} (confidence={result1.confidence:.2f})")

    # Second call — the first decision is now a precedent.
    print("\n==> Second decision (should find precedent)...")
    result2 = middleware.wrap("database_selection", choose_database)
    print(f"    Result: {result2.database} (confidence={result2.confidence:.2f})")

    print("\n==> Done. Both decisions are now in the audit trail.")


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
