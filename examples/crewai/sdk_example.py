#!/usr/bin/env python3
"""
Akashi + CrewAI SDK Example
============================
Shows the recommended way to integrate Akashi tracing into a CrewAI crew
using the ``AkashiCrew`` proxy.

AkashiCrew wraps an existing Crew and:
  1. Installs task/step callbacks (composing with any you already set)
  2. Calls check() before kickoff() to surface precedents
  3. Calls trace() after kickoff() to record the crew's output

All other crew attributes and methods pass through unchanged.

Usage:
    # Requires a running Akashi server and an agent API key.
    pip install crewai
    pip install -e ../../sdk/python
    pip install -e ../../sdk/integrations/crewai

    # Pre-scripted (no LLM needed):
    python sdk_example.py

    # With a real LLM:
    OPENAI_API_KEY=sk-... python sdk_example.py --live
"""
from __future__ import annotations

import os
import sys

from akashi import AkashiSyncClient
from akashi_crewai import AkashiCrew

AKASHI_URL = os.environ.get("AKASHI_URL", "http://localhost:8080")
AKASHI_AGENT_ID = os.environ.get("AKASHI_AGENT_ID", "crewai-demo")
AKASHI_API_KEY = os.environ.get("AKASHI_API_KEY", "")


def _build_crew(live: bool):  # type: ignore[no-untyped-def]
    """Build a simple two-agent crew.

    With ``live=True``, agents use a real LLM (requires OPENAI_API_KEY).
    With ``live=False``, a stub crew is returned that produces canned output.
    """
    if not live:
        return _StubCrew()

    try:
        from crewai import Agent, Crew, Process, Task
    except ImportError:
        sys.exit("crewai is not installed. Run: pip install crewai")

    researcher = Agent(
        role="Research Analyst",
        goal="Find accurate, concise answers to technical questions.",
        backstory="You are a senior research analyst specializing in technology.",
        verbose=False,
        allow_delegation=False,
    )
    writer = Agent(
        role="Technical Writer",
        goal="Synthesize research into clear, actionable recommendations.",
        backstory="You are a technical writer who turns research into decisions.",
        verbose=False,
        allow_delegation=False,
    )
    research_task = Task(
        description="Research the trade-offs of using PostgreSQL vs. ClickHouse for a 1TB/day metrics pipeline.",
        expected_output="A bullet-point summary of pros and cons for each option.",
        agent=researcher,
    )
    write_task = Task(
        description="Based on the research, recommend one database with a clear rationale.",
        expected_output="A one-paragraph recommendation starting with the chosen database.",
        agent=writer,
    )
    return Crew(
        agents=[researcher, writer],
        tasks=[research_task, write_task],
        process=Process.sequential,
        verbose=False,
    )


class _StubCrew:
    """Minimal duck-typed Crew for running without an LLM."""

    task_callback = None
    step_callback = None

    def kickoff(self, inputs: dict | None = None) -> str:
        # Simulate CrewAI calling its callbacks
        if self.task_callback:
            self.task_callback(_StubTaskOutput(
                raw="PostgreSQL with TimescaleDB: lower ops burden, SQL compatibility, 10-20x compression.",
                agent="Research Analyst",
                description="Research database trade-offs for metrics pipeline.",
            ))
        if self.task_callback:
            self.task_callback(_StubTaskOutput(
                raw="Recommend TimescaleDB. Operational simplicity wins for a small team.",
                agent="Technical Writer",
                description="Synthesize research into a recommendation.",
            ))
        return "Recommend TimescaleDB for the metrics pipeline. It extends PostgreSQL (which the team already operates), achieves 10-20x compression on time-series data, and avoids introducing a new database technology for a 4-engineer team."


class _StubTaskOutput:
    def __init__(self, raw: str, agent: str, description: str) -> None:
        self.raw = raw
        self.agent = agent
        self.description = description


def main() -> None:
    live = "--live" in sys.argv

    if not AKASHI_API_KEY:
        print("NOTE: AKASHI_API_KEY not set. Tracing will fail (non-fatal).")
        print("      Set it to see decisions in the Akashi dashboard.\n")

    # Build the crew
    crew = _build_crew(live)

    # Wrap it with AkashiCrew — one line for full tracing
    client = AkashiSyncClient(
        base_url=AKASHI_URL,
        agent_id=AKASHI_AGENT_ID,
        api_key=AKASHI_API_KEY,
    )
    traced = AkashiCrew(crew, client, decision_type="database_selection")

    # Use it exactly like a normal crew
    print("Running crew...\n")
    result = traced.kickoff(inputs={"scale": "1TB/day"})

    print(f"Result:\n  {result}\n")

    # You can still access crew attributes through the proxy
    print(f"Crew type: {type(traced._crew).__name__}")

    client.close()


if __name__ == "__main__":
    main()
