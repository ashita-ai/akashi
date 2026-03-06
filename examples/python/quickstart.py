#!/usr/bin/env python3
"""Akashi Python SDK quickstart — check, trace, and query decisions.

Prerequisites:
    docker compose -f docker-compose.complete.yml up -d
    pip install -e sdk/python   # from repo root

Run:
    python examples/python/quickstart.py
"""

import os
import sys

from akashi import (
    AkashiSyncClient,
    ConflictError,
    CreateAgentRequest,
    QueryFilters,
    TraceAlternative,
    TraceEvidence,
    TraceRequest,
)

URL = os.environ.get("AKASHI_URL", "http://localhost:8080")
ADMIN_KEY = os.environ.get("AKASHI_ADMIN_API_KEY", "admin")


def main() -> None:
    # --- Connect as admin and verify the server is up ---
    admin = AkashiSyncClient(base_url=URL, agent_id="admin", api_key=ADMIN_KEY)
    health = admin.health()
    print(f"==> Connected to Akashi {health.version} (postgres: {health.postgres})")

    # --- Create a demo agent (idempotent — ignores 409 if it already exists) ---
    try:
        admin.create_agent(CreateAgentRequest(
            agent_id="quickstart-agent",
            name="Quickstart Agent",
            role="agent",
            api_key="quickstart-secret",
        ))
        print("==> Created agent 'quickstart-agent'")
    except ConflictError:
        print("==> Agent 'quickstart-agent' already exists")

    # --- Switch to the agent identity ---
    client = AkashiSyncClient(
        base_url=URL, agent_id="quickstart-agent", api_key="quickstart-secret",
    )

    # --- Check: are there existing decisions about model selection? ---
    print("\n==> Checking for precedents on 'model_selection'...")
    check = client.check("model_selection")
    if check.has_precedent:
        print(f"    Found {len(check.decisions)} prior decision(s)")
        for d in check.decisions:
            print(f"    - {d.outcome} (confidence={d.confidence:.2f})")
    else:
        print("    No prior decisions found — this will be the first.")

    # --- Trace: record a new decision ---
    print("\n==> Tracing a model selection decision...")
    resp = client.trace(TraceRequest(
        decision_type="model_selection",
        outcome="Use GPT-4o for summarization tasks",
        confidence=0.85,
        reasoning=(
            "GPT-4o offers the best quality-to-cost ratio for summarization. "
            "Benchmarked against Claude and Gemini on 200 sample documents."
        ),
        alternatives=[
            TraceAlternative(label="GPT-4o", score=0.85, selected=True),
            TraceAlternative(
                label="Claude 3.5 Sonnet", score=0.80, selected=False,
                rejection_reason="Slightly higher latency on long documents",
            ),
            TraceAlternative(
                label="Gemini 1.5 Pro", score=0.70, selected=False,
                rejection_reason="Inconsistent formatting in structured output",
            ),
        ],
        evidence=[
            TraceEvidence(
                source_type="benchmark",
                content="GPT-4o ROUGE-L: 0.47, Claude: 0.45, Gemini: 0.41 on CNN/DailyMail",
                relevance_score=0.9,
            ),
        ],
    ))
    print(f"    Decision recorded: id={resp.decision_id}")

    # --- Query: retrieve decisions matching our filter ---
    print("\n==> Querying model_selection decisions...")
    query = client.query(QueryFilters(decision_type="model_selection"))
    print(f"    Found {query.total} decision(s)")
    for d in query.decisions:
        print(f"    - [{d.agent_id}] {d.outcome} (confidence={d.confidence:.2f})")

    # --- Recent: fetch the latest decisions across all types ---
    print("\n==> Fetching 5 most recent decisions...")
    recent = client.recent(limit=5)
    print(f"    Got {len(recent)} decision(s)")
    for d in recent:
        print(f"    - [{d.decision_type}] {d.outcome}")

    print("\n==> Done. View your decisions at", URL)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
