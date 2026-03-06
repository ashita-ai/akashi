#!/usr/bin/env python3
"""Multi-agent conflict detection — two agents, contradictory decisions, automatic detection.

Two architect agents independently decide on the payment platform's architecture.
One picks microservices, the other picks a modular monolith. Akashi detects the
conflict automatically via embedding similarity and (optionally) LLM validation.

This example proves the SDK handles multi-agent identity correctly: each agent
authenticates with its own credentials and traces under its own identity.

Prerequisites:
    docker compose -f docker-compose.complete.yml up -d   # full stack with Qdrant + Ollama
    pip install -e sdk/python                              # from repo root

Run:
    python examples/python/multi_agent_conflicts.py
"""

import os
import sys
import time

from akashi import (
    AkashiSyncClient,
    ConflictError,
    CreateAgentRequest,
    TraceAlternative,
    TraceRequest,
)

URL = os.environ.get("AKASHI_URL", "http://localhost:8080")
ADMIN_KEY = os.environ.get("AKASHI_ADMIN_API_KEY", "admin")


def ensure_agent(admin: AkashiSyncClient, agent_id: str, name: str, api_key: str) -> None:
    """Create an agent, ignoring 409 if it already exists."""
    try:
        admin.create_agent(CreateAgentRequest(
            agent_id=agent_id, name=name, role="agent", api_key=api_key,
        ))
        print(f"    Created agent '{agent_id}'")
    except ConflictError:
        print(f"    Agent '{agent_id}' already exists")


def main() -> None:
    print("=== Multi-Agent Conflict Detection ===\n")
    print("Two architect agents will independently decide on payment platform")
    print("architecture. They will reach opposing conclusions, and Akashi will")
    print("detect the conflict automatically.\n")

    # --- Setup ---
    admin = AkashiSyncClient(base_url=URL, agent_id="admin", api_key=ADMIN_KEY)
    health = admin.health()
    print(f"==> Connected to Akashi {health.version}")

    # Conflict detection requires Qdrant for embedding-based similarity.
    if health.qdrant != "connected":
        print(
            "\nError: Qdrant is not connected. Conflict detection requires the full stack:\n"
            "  docker compose -f docker-compose.complete.yml up -d\n"
            "\nWait for the ollama-init container to finish downloading models,\n"
            "then retry.",
            file=sys.stderr,
        )
        sys.exit(1)

    print("\n==> Setting up agents...")
    ensure_agent(admin, "architect-alpha", "Architect Alpha", "alpha-secret")
    ensure_agent(admin, "architect-beta", "Architect Beta", "beta-secret")

    # Each agent gets its own SDK client with its own credentials.
    alpha = AkashiSyncClient(base_url=URL, agent_id="architect-alpha", api_key="alpha-secret")
    beta = AkashiSyncClient(base_url=URL, agent_id="architect-beta", api_key="beta-secret")

    # --- Agent Alpha decides: microservices ---
    print("\n==> Architect Alpha is deciding...")
    resp_a = alpha.trace(TraceRequest(
        decision_type="architecture",
        outcome="Adopt microservices with Kafka for the payment processing platform",
        confidence=0.88,
        reasoning=(
            "Independent scaling of payment, fraud, and notification services. "
            "PCI-DSS scope reduction by isolating card-data handling into a "
            "dedicated service with its own network boundary."
        ),
        alternatives=[
            TraceAlternative(label="Microservices + Kafka", score=0.88, selected=True),
            TraceAlternative(
                label="Modular monolith", score=0.65, selected=False,
                rejection_reason="Cannot isolate PCI scope at the network level",
            ),
        ],
    ))
    print(f"    Alpha traced: id={resp_a.decision_id}")

    # Wait for the embedding pipeline to index Alpha's decision in Qdrant.
    # Without this, Beta's decision can't be compared against Alpha's.
    print("\n==> Waiting for embedding indexing (4s)...")
    time.sleep(4)

    # --- Agent Beta decides: modular monolith (conflicting) ---
    print("\n==> Architect Beta is deciding...")
    resp_b = beta.trace(TraceRequest(
        decision_type="architecture",
        outcome="Use a modular monolith with domain boundaries for the payment processing platform",
        confidence=0.82,
        reasoning=(
            "Team of 8 engineers cannot sustain microservices operational overhead. "
            "A well-structured monolith with module boundaries achieves the same "
            "PCI isolation via code-level access controls and schema separation."
        ),
        alternatives=[
            TraceAlternative(label="Modular monolith", score=0.82, selected=True),
            TraceAlternative(
                label="Microservices", score=0.60, selected=False,
                rejection_reason="Operational overhead exceeds team capacity",
            ),
        ],
    ))
    print(f"    Beta traced: id={resp_b.decision_id}")

    # --- Poll for conflicts ---
    print("\n==> Waiting for conflict detection...")
    max_wait = 60
    interval = 5
    elapsed = 0

    conflicts = []
    while elapsed < max_wait:
        conflicts = admin.list_conflicts(decision_type="architecture")
        if conflicts:
            break
        elapsed += interval
        print(f"    Polling... ({elapsed}s/{max_wait}s)")
        time.sleep(interval)

    if not conflicts:
        print("\n    No conflicts detected within the timeout.")
        print("    This can happen if Ollama models are still loading.")
        print("    Check: docker compose logs -f ollama-init")
        sys.exit(1)

    # --- Display the conflict ---
    print(f"\n==> Conflict detected! Found {len(conflicts)} conflict(s).\n")
    for c in conflicts:
        print(f"    Kind:               {c.conflict_kind}")
        print(f"    Decision type:      {c.decision_type}")
        print(f"    Agent A:            {c.agent_a}")
        print(f"    Outcome A:          {c.outcome_a}")
        print(f"    Agent B:            {c.agent_b}")
        print(f"    Outcome B:          {c.outcome_b}")
        if c.topic_similarity is not None:
            print(f"    Topic similarity:   {c.topic_similarity:.3f}")
        if c.outcome_divergence is not None:
            print(f"    Outcome divergence: {c.outcome_divergence:.3f}")
        if c.significance is not None:
            print(f"    Significance:       {c.significance:.3f}")
        print()

    print(f"==> View conflicts in the dashboard: {URL}")


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
