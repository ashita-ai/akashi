# akashi-langchain

LangChain callback handler that traces agent decisions to [Akashi](../../../README.md) -- the black box recorder for AI decisions.

Hooks into the LangChain agent lifecycle to automatically call `check()` before each tool use and `trace()` after each tool call and final answer, with zero changes to your agent or chain code.

**Requirements:** Python 3.10+, `langchain-core>=0.3`, `akashi>=0.1.0`

## Install

```bash
pip install akashi-langchain
# or from source:
pip install -e sdk/integrations/langchain
```

## Quick start

### Synchronous agent

```python
from akashi import AkashiSyncClient
from akashi_langchain import AkashiCallbackHandler

client = AkashiSyncClient(
    base_url="http://localhost:8080",
    agent_id="my-agent",
    api_key="my-api-key",
)

handler = AkashiCallbackHandler(client, decision_type="research_agent")

# Pass the handler in the LangChain config — no other changes needed.
result = agent.invoke(
    {"input": "What is the capital of France?"},
    config={"callbacks": [handler]},
)
```

### Async agent

```python
from akashi import AkashiClient
from akashi_langchain import AsyncAkashiCallbackHandler

client = AkashiClient(
    base_url="http://localhost:8080",
    agent_id="my-agent",
    api_key="my-api-key",
)

handler = AsyncAkashiCallbackHandler(client, decision_type="research_agent")

result = await chain.ainvoke(
    {"input": "Summarize the latest AI news"},
    config={"callbacks": [handler]},
)
```

## How it works

The handler maps three LangChain lifecycle events to Akashi calls:

| LangChain event | Akashi call | What it records |
|-----------------|-------------|-----------------|
| `on_agent_action` | `check()` | Surfaces precedents before the agent selects a tool |
| `on_tool_end` | `trace()` | Records the tool used, its output, and the agent's reasoning |
| `on_agent_finish` | `trace()` | Records the final answer and the agent's log |

Each `check()` query is `tool=<name> input=<input[:200]>`, giving the Akashi server enough context to find relevant prior decisions.

The `trace()` call for tool use sets `metadata={"tool": <name>, "tool_input": <input[:200]>}` so you can filter decisions by tool in the Akashi UI.

**Concurrent tool calls:** The handler tracks pending agent actions by `run_id` in an internal `dict`. When multiple tool calls are in flight simultaneously (parallel agent runs), each is matched to its own `on_tool_end` event via `parent_run_id`. No cross-contamination.

## API

### `AkashiCallbackHandler(client, **options)`

Synchronous handler. Pair with `AkashiSyncClient`.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `client` | `AkashiSyncClient` | required | Configured Akashi client |
| `decision_type` | `str` | `"agent_decision"` | Label applied to every trace and check |
| `confidence` | `float` | `0.7` | Default confidence score for traces (0–1) |
| `check_before_action` | `bool` | `True` | Call `check()` on each `on_agent_action` event |
| `trace_tool_use` | `bool` | `True` | Call `trace()` on each `on_tool_end` event |
| `trace_final_answer` | `bool` | `True` | Call `trace()` on each `on_agent_finish` event |

### `AsyncAkashiCallbackHandler(client, **options)`

Async handler. Identical contract and options as above, but uses `AkashiClient` (async) and `await`s all Akashi calls. Use with `chain.ainvoke(...)` or `agent.arun(...)`.

## Configuring per-invocation

Create multiple handlers with different `decision_type` labels to distinguish agents running in the same process:

```python
planner_handler = AkashiCallbackHandler(client, decision_type="planner_agent")
executor_handler = AkashiCallbackHandler(client, decision_type="executor_agent")

planner_result = planner.invoke({"input": "..."}, config={"callbacks": [planner_handler]})
executor_result = executor.invoke({"input": "..."}, config={"callbacks": [executor_handler]})
```

## Selective tracing

Disable specific hooks to reduce trace volume:

```python
# Only trace final answers, skip per-tool traces.
handler = AkashiCallbackHandler(
    client,
    check_before_action=False,
    trace_tool_use=False,
    trace_final_answer=True,
)

# Only check before tool use; never trace.
handler = AkashiCallbackHandler(
    client,
    check_before_action=True,
    trace_tool_use=False,
    trace_final_answer=False,
)
```

## Error handling

All Akashi calls are **fire-and-forget**. If the Akashi server is unreachable or returns an error, the exception is logged at `DEBUG` level and the LangChain execution continues normally. The handler will never raise an exception into your agent or chain.

```python
import logging
logging.getLogger("akashi_langchain").setLevel(logging.DEBUG)
# DEBUG output: "akashi check failed (non-fatal): Connection refused"
```

## Truncation

Long values are truncated before being sent to Akashi to prevent oversized payloads:

| Field | Limit |
|-------|-------|
| `outcome` (tool output, final answer) | 500 characters |
| `reasoning` (agent log) | 500 characters |
| Check query (tool input) | 200 characters |
