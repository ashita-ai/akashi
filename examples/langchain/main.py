#!/usr/bin/env python3
"""LangChain + Akashi integration example — automatic decision tracing.

Demonstrates wiring ``AkashiCallbackHandler`` into a LangChain agent so that
every tool selection and final answer is automatically traced to Akashi,
with zero explicit ``trace()`` calls.

Prerequisites:
    docker compose -f docker-compose.complete.yml up -d
    pip install -e sdk/python
    pip install -e sdk/integrations/langchain

Run:
    # Pre-scripted (no LLM needed):
    python examples/langchain/main.py

    # With a real LLM:
    OPENAI_API_KEY=sk-... python examples/langchain/main.py --live
"""
from __future__ import annotations

import os
import sys

from akashi import AkashiSyncClient, ConflictError, CreateAgentRequest
from akashi_langchain import AkashiCallbackHandler

URL = os.environ.get("AKASHI_URL", "http://localhost:8080")
ADMIN_KEY = os.environ.get("AKASHI_ADMIN_API_KEY", "admin")
AGENT_ID = "langchain-example-agent"
AGENT_KEY = "langchain-secret"
DECISION_TYPE = "tech_recommendation"


# ---------------------------------------------------------------------------
# Stub agent (runs without an LLM)
# ---------------------------------------------------------------------------


class _StubAction:
    """Mimics a LangChain AgentAction."""

    def __init__(self, tool: str, tool_input: str, log: str) -> None:
        self.tool = tool
        self.tool_input = tool_input
        self.log = log


class _StubFinish:
    """Mimics a LangChain AgentFinish."""

    def __init__(self, output: str, log: str) -> None:
        self.return_values = {"output": output}
        self.log = log


def _run_stub(handler: AkashiCallbackHandler) -> str:
    """Simulate an agent run by firing the callback lifecycle manually."""
    from uuid import uuid4

    # Step 1: agent selects a tool
    agent_run_id = uuid4()
    handler.on_agent_action(
        _StubAction(
            tool="web_search",
            tool_input="best technology stack for real-time analytics 2025",
            log="I need to research current technology options before making a recommendation.",
        ),
        run_id=agent_run_id,
    )

    # Step 2: tool returns a result
    handler.on_tool_end(
        "Top picks: Apache Kafka + ClickHouse for throughput; "
        "Apache Flink for stateful stream processing; "
        "DuckDB for embedded OLAP on moderate volumes.",
        run_id=uuid4(),
        parent_run_id=agent_run_id,
    )

    # Step 3: agent produces a final answer
    final_output = (
        "For real-time analytics at scale, I recommend Apache Kafka for ingestion "
        "paired with ClickHouse for the analytical store. Kafka handles sustained "
        "throughput of millions of events/sec with exactly-once semantics, while "
        "ClickHouse delivers sub-second queries over petabyte-scale data using "
        "columnar storage and vectorized execution. For teams with smaller volumes "
        "(<100K events/sec), DuckDB is a compelling embedded alternative that "
        "eliminates operational overhead."
    )
    handler.on_agent_finish(
        _StubFinish(
            output=final_output,
            log="Based on the search results, I'm recommending Kafka + ClickHouse as the primary stack.",
        ),
        run_id=uuid4(),
    )
    return final_output


# ---------------------------------------------------------------------------
# Live agent (real LangChain + OpenAI)
# ---------------------------------------------------------------------------


def _run_live(handler: AkashiCallbackHandler) -> str:
    """Run a real LangChain agent with tools, traced via the callback."""
    try:
        from langchain.agents import AgentExecutor, create_react_agent
        from langchain_core.prompts import PromptTemplate
        from langchain_core.tools import tool
        from langchain_openai import ChatOpenAI
    except ImportError:
        sys.exit(
            "Missing dependencies for --live mode.\n"
            "Run: pip install langchain langchain-openai"
        )

    @tool
    def recommend_stack(query: str) -> str:
        """Look up technology recommendations for a given use case."""
        return (
            "Top picks for real-time analytics: "
            "Apache Kafka + ClickHouse for high throughput; "
            "Apache Flink for complex stateful stream processing; "
            "DuckDB for embedded OLAP on moderate volumes (<100K events/sec)."
        )

    llm = ChatOpenAI(model="gpt-4o-mini", temperature=0)
    tools = [recommend_stack]

    template = """Answer the following question as best you can. You have access to the following tools:

{tools}

Use the following format:

Question: the input question you must answer
Thought: you should always think about what to do
Action: the action to take, should be one of [{tool_names}]
Action Input: the input to the action
Observation: the result of the action
... (this Thought/Action/Action Input/Observation can repeat N times)
Thought: I now know the final answer
Final Answer: the final answer to the original input question

Begin!

Question: {input}
Thought:{agent_scratchpad}"""

    prompt = PromptTemplate.from_template(template)
    agent = create_react_agent(llm, tools, prompt)
    executor = AgentExecutor(agent=agent, tools=tools, verbose=True)

    result = executor.invoke(
        {"input": "What technology stack do you recommend for real-time analytics at scale?"},
        config={"callbacks": [handler]},
    )
    return result["output"]


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    live = "--live" in sys.argv

    if live and not os.environ.get("OPENAI_API_KEY"):
        print(
            "Error: OPENAI_API_KEY is not set.\n"
            "Set the env var and retry, or run without --live for the pre-scripted demo.",
            file=sys.stderr,
        )
        sys.exit(1)

    print("=== LangChain + Akashi Integration Example ===\n")

    # --- Connect to Akashi and provision an agent ---
    admin = AkashiSyncClient(base_url=URL, agent_id="admin", api_key=ADMIN_KEY)
    health = admin.health()
    print(f"==> Connected to Akashi {health.version}")

    try:
        admin.create_agent(CreateAgentRequest(
            agent_id=AGENT_ID,
            name="LangChain Example Agent",
            role="agent",
            api_key=AGENT_KEY,
        ))
        print(f"==> Created agent '{AGENT_ID}'")
    except ConflictError:
        print(f"==> Agent '{AGENT_ID}' already exists")

    client = AkashiSyncClient(
        base_url=URL, agent_id=AGENT_ID, api_key=AGENT_KEY,
    )

    # --- Create the callback handler (one line of wiring) ---
    handler = AkashiCallbackHandler(
        client,
        decision_type=DECISION_TYPE,
        confidence=0.85,
    )

    # --- Run the agent ---
    print("\n==> Running agent...\n")
    if live:
        output = _run_live(handler)
    else:
        output = _run_stub(handler)

    # --- Show the output ---
    print("\n" + "=" * 60)
    print("AGENT OUTPUT")
    print("=" * 60)
    print(output)

    # --- Query Akashi to show decisions were auto-recorded ---
    print("\n==> Decisions auto-recorded in Akashi (no explicit trace() calls):")
    decisions = client.recent(decision_type=DECISION_TYPE, limit=10)
    for i, d in enumerate(decisions, 1):
        preview = d.outcome[:100] + "..." if len(d.outcome) > 100 else d.outcome
        print(f"    {i}. id={d.id}")
        print(f"       [{d.decision_type}] {preview}")
        print(f"       confidence={d.confidence:.2f}")

    print(f"\n==> {len(decisions)} decision(s) traced automatically. View at {URL}")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\nInterrupted.")
        sys.exit(130)
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
