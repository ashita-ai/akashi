# LangChain + Akashi: Automatic Decision Tracing

This example shows how to wire `AkashiCallbackHandler` into a LangChain agent so that every tool selection and final answer is automatically traced to Akashi — with zero explicit `trace()` calls.

## How it works

The `AkashiCallbackHandler` hooks into three LangChain agent lifecycle events:

| LangChain event    | Akashi call | What gets recorded                              |
|--------------------|-------------|-------------------------------------------------|
| `on_agent_action`  | `check()`   | Surfaces relevant precedents before tool use    |
| `on_tool_end`      | `trace()`   | Records which tool was used and its output       |
| `on_agent_finish`  | `trace()`   | Records the agent's final answer and reasoning   |

All you need is one line of wiring — pass `callbacks=[handler]` when invoking the agent:

```python
from akashi import AkashiSyncClient
from akashi_langchain import AkashiCallbackHandler

client = AkashiSyncClient(base_url="http://localhost:8080", agent_id="my-agent", api_key="...")
handler = AkashiCallbackHandler(client, decision_type="tech_recommendation")

result = agent.invoke(
    {"input": "What stack do you recommend?"},
    config={"callbacks": [handler]},
)
```

No changes to your agent, chain, or tools. Every decision flows into the Akashi audit trail automatically.

## Prerequisites

1. Start the local stack:

```sh
docker compose -f docker-compose.complete.yml up -d
```

2. Install dependencies:

```sh
pip install -e sdk/python
pip install -e sdk/integrations/langchain
```

## Running

### Pre-scripted mode (no LLM needed)

```sh
python examples/langchain/main.py
```

Simulates an agent run by firing the callback lifecycle manually. Useful for verifying the integration works without an OpenAI key.

### Live mode (real LLM)

```sh
OPENAI_API_KEY=sk-... python examples/langchain/main.py --live
```

Runs a real LangChain ReAct agent with a tool, traced through the callback handler.

## Expected output

```
=== LangChain + Akashi Integration Example ===

==> Connected to Akashi v0.x.x
==> Agent 'langchain-example-agent' already exists

==> Running agent...

============================================================
AGENT OUTPUT
============================================================
For real-time analytics at scale, I recommend Apache Kafka for ingestion...

==> Decisions auto-recorded in Akashi (no explicit trace() calls):
    1. id=<uuid>
       [tech_recommendation] used tool 'web_search': Top picks: Apache Kafka + ClickHouse...
       confidence=0.85
    2. id=<uuid>
       [tech_recommendation] For real-time analytics at scale, I recommend Apache Kafka...
       confidence=0.85

==> 2 decision(s) traced automatically. View at http://localhost:8080
```

## Key takeaways

- **Zero-friction**: add one callback, every agent decision is captured
- **Fire-and-forget**: if Akashi is down, the agent runs normally — errors are swallowed
- **Precedent checks**: before each tool use, the handler calls `check()` to surface relevant prior decisions
- **No code changes**: your existing LangChain agent/chain/tools stay untouched
