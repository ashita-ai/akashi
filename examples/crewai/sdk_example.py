#!/usr/bin/env python3
"""CrewAI + Akashi integration example — automatic per-task tracing.

A two-agent research-then-write pipeline with the akashi-crewai integration
wired in. Each task completion is automatically traced, and the entire crew
run gets a check-before / trace-after wrapper.

This demonstrates the actual integration package (akashi-crewai), not raw HTTP.

Prerequisites:
    docker compose -f docker-compose.complete.yml up -d
    pip install -e sdk/python
    pip install -e sdk/integrations/crewai
    export OPENAI_API_KEY=...

Run:
    python examples/crewai/sdk_example.py
"""

import os
import sys

from crewai import Agent, Crew, Process, Task

from akashi import AkashiSyncClient, ConflictError, CreateAgentRequest
from akashi_crewai import make_hooks, run_with_akashi

URL = os.environ.get("AKASHI_URL", "http://localhost:8080")
ADMIN_KEY = os.environ.get("AKASHI_ADMIN_API_KEY", "admin")
TOPIC = "the trade-offs between fine-tuning open-source LLMs vs using proprietary API providers"


def main() -> None:
    if not os.environ.get("OPENAI_API_KEY"):
        print(
            "Error: OPENAI_API_KEY is not set.\n"
            "CrewAI requires an LLM provider. Set the env var and retry.",
            file=sys.stderr,
        )
        sys.exit(1)

    print("=== CrewAI + Akashi Integration Example ===\n")

    # --- Akashi setup ---
    admin = AkashiSyncClient(base_url=URL, agent_id="admin", api_key=ADMIN_KEY)
    health = admin.health()
    print(f"==> Connected to Akashi {health.version}")

    try:
        admin.create_agent(CreateAgentRequest(
            agent_id="crewai-example-agent",
            name="CrewAI Example Agent",
            role="agent",
            api_key="crewai-secret",
        ))
        print("==> Created agent 'crewai-example-agent'")
    except ConflictError:
        print("==> Agent 'crewai-example-agent' already exists")

    client = AkashiSyncClient(
        base_url=URL, agent_id="crewai-example-agent", api_key="crewai-secret",
    )

    # --- CrewAI agents ---
    researcher = Agent(
        role="Technical Researcher",
        goal="Produce a thorough analysis of the assigned topic with concrete data points",
        backstory=(
            "You are a senior ML engineer who has deployed models at scale. "
            "You favor evidence-based reasoning and always cite trade-offs."
        ),
        verbose=True,
        allow_delegation=False,
    )

    writer = Agent(
        role="Technical Writer",
        goal="Distill complex research into a clear, actionable briefing",
        backstory=(
            "You are a developer advocate who writes for engineering audiences. "
            "You focus on clarity, structure, and practical recommendations."
        ),
        verbose=True,
        allow_delegation=False,
    )

    # --- Tasks ---
    research_task = Task(
        description=(
            f"Research {TOPIC}. "
            "Cover cost, latency, data privacy, customization depth, and operational burden. "
            "Include at least three specific examples of each approach."
        ),
        expected_output="A structured analysis with sections for each trade-off dimension.",
        agent=researcher,
    )

    write_task = Task(
        description=(
            "Using the research provided, write a 300-word executive briefing. "
            "Open with the key recommendation, then support it with the strongest "
            "evidence from the research. End with caveats and when the opposite "
            "approach might be better."
        ),
        expected_output="A concise executive briefing in markdown format.",
        agent=writer,
    )

    # --- Build the crew with Akashi hooks ---
    # make_hooks() returns {"task_callback": ..., "step_callback": ...}
    # which wires automatic per-task tracing and per-step precedent checks.
    crew = Crew(
        agents=[researcher, writer],
        tasks=[research_task, write_task],
        process=Process.sequential,
        verbose=True,
        **make_hooks(client, decision_type="content_pipeline"),
    )

    # run_with_akashi() adds a crew-level check/trace around the entire run.
    # Combined with make_hooks, you get both per-task and crew-level tracing.
    print("\n==> Starting crew (this will take a minute or two)...\n")
    result = run_with_akashi(
        crew, client,
        inputs={"topic": TOPIC},
        decision_type="content_pipeline",
    )

    # --- Show the output ---
    print("\n" + "=" * 60)
    print("CREW OUTPUT")
    print("=" * 60)
    print(result)

    # --- Query the audit trail ---
    print("\n==> Decisions recorded in Akashi:")
    decisions = client.recent(decision_type="content_pipeline", limit=10)
    for i, d in enumerate(decisions, 1):
        outcome_preview = d.outcome[:100] + "..." if len(d.outcome) > 100 else d.outcome
        print(f"    {i}. [{d.decision_type}] {outcome_preview}")
        print(f"       confidence={d.confidence:.2f}, agent={d.agent_id}")

    print(f"\n==> {len(decisions)} decision(s) traced. View at {URL}")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\nInterrupted.")
        sys.exit(130)
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
