#!/usr/bin/env python3
"""CrewAI + Akashi integration example — automatic per-task tracing.

A two-agent research-then-write pipeline with the akashi-crewai integration
wired in via ``AkashiCrew``. Each task completion is automatically traced,
per-step tool selections trigger precedent checks, and the entire crew run
gets a check-before / trace-after wrapper — all from a single proxy object.

Prerequisites:
    docker compose -f docker-compose.complete.yml up -d
    pip install -e sdk/python
    pip install -e sdk/integrations/crewai

Run:
    # Pre-scripted (no LLM needed):
    python examples/crewai/sdk_example.py

    # With real CrewAI agents:
    OPENAI_API_KEY=sk-... python examples/crewai/sdk_example.py --live
"""
from __future__ import annotations

import os
import sys

from akashi import AkashiSyncClient, ConflictError, CreateAgentRequest
from akashi_crewai import AkashiCrew

URL = os.environ.get("AKASHI_URL", "http://localhost:8080")
ADMIN_KEY = os.environ.get("AKASHI_ADMIN_API_KEY", "admin")
TOPIC = "the trade-offs between fine-tuning open-source LLMs vs using proprietary API providers"


# ---------------------------------------------------------------------------
# Stub crew (runs without an LLM)
# ---------------------------------------------------------------------------


class _StubTaskOutput:
    def __init__(self, raw: str, agent: str, description: str) -> None:
        self.raw = raw
        self.agent = agent
        self.description = description


class _StubCrew:
    """Minimal duck-typed Crew for running without an LLM."""

    task_callback = None
    step_callback = None

    def kickoff(self, inputs: dict | None = None) -> str:
        if self.task_callback:
            self.task_callback(_StubTaskOutput(
                raw=(
                    "Fine-tuning open-source LLMs offers better accuracy on domain tasks "
                    "(94% vs 89% zero-shot), eliminates per-token API costs (~$180K/year "
                    "savings at 50K conversations/day), and keeps data on-prem for compliance."
                ),
                agent="Technical Researcher",
                description=f"Research {TOPIC}.",
            ))
        if self.task_callback:
            self.task_callback(_StubTaskOutput(
                raw=(
                    "Recommend starting with a proprietary API (GPT-4o) for time-to-market, "
                    "then migrating to a fine-tuned open-source model once you have enough "
                    "domain data and the operational maturity to self-host."
                ),
                agent="Technical Writer",
                description="Write an executive briefing based on the research.",
            ))
        return (
            "Start with GPT-4o via API for initial launch (ships in 2 weeks), then "
            "build a fine-tuning pipeline for Llama 3.1 70B as a 6-month milestone. "
            "The hybrid approach captures time-to-market benefits while building toward "
            "cost savings and data sovereignty at scale."
        )


# ---------------------------------------------------------------------------
# Live crew (real CrewAI agents)
# ---------------------------------------------------------------------------


def _build_live_crew():  # type: ignore[no-untyped-def]
    try:
        from crewai import Agent, Crew, Process, Task
    except ImportError:
        sys.exit("crewai is not installed. Run: pip install crewai")

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
    return Crew(
        agents=[researcher, writer],
        tasks=[research_task, write_task],
        process=Process.sequential,
        verbose=True,
    )


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    live = "--live" in sys.argv

    if live and not os.environ.get("OPENAI_API_KEY"):
        print(
            "Error: OPENAI_API_KEY is not set.\n"
            "CrewAI requires an LLM provider. Set the env var and retry.\n"
            "Or run without --live for the pre-scripted demo.",
            file=sys.stderr,
        )
        sys.exit(1)

    print("=== CrewAI + Akashi Integration Example ===\n")

    # --- Akashi setup: provision an agent identity ---
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

    # --- Build and wrap the crew ---
    crew = _build_live_crew() if live else _StubCrew()

    # AkashiCrew wraps the crew with full tracing in one line:
    #   - Installs task_callback and step_callback (composing with existing ones)
    #   - Calls check() before kickoff() to surface precedents
    #   - Calls trace() after kickoff() to record the crew's output
    traced = AkashiCrew(crew, client, decision_type="content_pipeline")

    # Use it exactly like a normal crew
    print("\n==> Starting crew...\n")
    result = traced.kickoff(inputs={"topic": TOPIC})

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
