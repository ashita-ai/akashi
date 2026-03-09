# akashi-crewai

CrewAI integration that traces task decisions to [Akashi](../../../README.md) -- version control for AI decisions.

All tracing is **fire-and-forget**: Akashi errors are logged at `DEBUG` level and never interrupt your crew execution.

**Requirements:** Python 3.10+, `crewai>=0.70,<2`, `akashi>=0.1.0`

## Install

```bash
pip install akashi-crewai
# or from source:
pip install -e sdk/integrations/crewai
```

## Quick start

### `AkashiCrew` — recommended

Wrap an existing `Crew` with per-task callbacks and crew-level check/trace in one object:

```python
from crewai import Crew, Agent, Task
from akashi import AkashiSyncClient
from akashi_crewai import AkashiCrew

client = AkashiSyncClient(
    base_url="http://localhost:8080",
    agent_id="my-crew",
    api_key="my-api-key",
)

crew = Crew(agents=[researcher, writer], tasks=[research_task, write_task])
traced = AkashiCrew(crew, client, decision_type="research")
result = traced.kickoff(inputs={"topic": "AI trends"})
```

`AkashiCrew` does three things:

1. Installs `task_callback` and `step_callback` on the crew, composing with any existing callbacks you already set.
2. Calls `check()` before `kickoff()` to surface precedents.
3. Calls `trace()` after `kickoff()` to record the crew's output.

All other crew methods and attributes pass through unchanged.

**Callback composition:** If the crew already has `task_callback` or `step_callback` set, `AkashiCrew` chains them: your callback fires first, then the Akashi callback. Your callback errors propagate normally; Akashi callback errors are swallowed.

## Lower-level patterns

`AkashiCrew` covers the common case. For finer control, use these primitives directly:

### `AkashiCrewCallbacks` — per-task and per-step hooks

The most granular option. Wire Akashi into every task completion and agent step:

```python
from akashi_crewai import AkashiCrewCallbacks

hooks = AkashiCrewCallbacks(client, decision_type="research_task")

crew = Crew(
    agents=[researcher, writer],
    tasks=[research_task, write_task],
    task_callback=hooks.on_task_complete,
    step_callback=hooks.on_step,
)

result = crew.kickoff(inputs={"topic": "AI trends"})
```

### `make_hooks` — concise Crew kwargs

A convenience wrapper that returns `{"task_callback": ..., "step_callback": ...}` ready to unpack directly into a `Crew` constructor:

```python
from akashi_crewai import make_hooks

crew = Crew(
    agents=[researcher, writer],
    tasks=[research_task, write_task],
    **make_hooks(client, decision_type="research_task"),
)
```

Both callbacks share the same `AkashiCrewCallbacks` instance, so they use the same `decision_type` and `confidence` settings.

### `run_with_akashi` — crew-level check/trace

Wraps an entire crew run with a single `check()` before `kickoff()` and a single `trace()` after it completes. Use this to record the crew's overall output as one decision, independent of per-task tracing:

```python
from akashi_crewai import run_with_akashi

# crew was already constructed (with or without make_hooks)
result = run_with_akashi(
    crew,
    client,
    inputs={"topic": "AI trends"},
    decision_type="crew_run",
)
```

### Combining lower-level patterns

Use `make_hooks` for per-task tracing and `run_with_akashi` for the crew-level trace simultaneously:

```python
crew = Crew(
    agents=[researcher, writer],
    tasks=[research_task, write_task],
    **make_hooks(client, decision_type="research_task"),
)

result = run_with_akashi(crew, client, inputs={"topic": "AI trends"})
```

This produces: one crew-level trace (from `run_with_akashi`) plus one trace per completed task (from `make_hooks`).

## How it works

| Entry point | Akashi call | What it records |
|-------------|-------------|-----------------|
| `AkashiCrew.kickoff()` | `check()` + `trace()` | Combines crew-level tracing with per-task/step callbacks in one object |
| `step_callback` (AgentAction) | `check()` | Surfaces precedents when an agent selects a tool |
| `step_callback` (AgentFinish) | nothing | AgentFinish has no tool selection; skipped |
| `task_callback` | `trace()` | Records the task's raw output, the completing agent, and the task description as reasoning |
| `run_with_akashi` before kickoff | `check()` | Surfaces precedents before the whole crew starts |
| `run_with_akashi` after kickoff | `trace()` | Records the crew's overall output |

The `on_step` callback duck-types the presence of a `.tool` attribute to distinguish `AgentAction` (tool selection, worth checking) from `AgentFinish` (done with the step, nothing to check). This means the integration has no hard dependency on CrewAI's internal types and works across CrewAI versions.

## API

### `AkashiCrew(crew, client, **options)`

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `crew` | `crewai.Crew` | required | Configured Crew instance |
| `client` | `AkashiSyncClient` | required | Configured Akashi client |
| `decision_type` | `str` | `"crew_task"` | Label applied to every trace and check |
| `confidence` | `float` | `0.7` | Default confidence score for traces (0--1) |
| `check_before_step` | `bool` | `True` | Call `check()` on tool-selection steps |
| `trace_task_output` | `bool` | `True` | Call `trace()` when a task completes |

**Methods:**

- `kickoff(inputs=None)` -- Runs the crew with Akashi check-before / trace-after. Returns the `CrewOutput` unchanged.
- All other methods and attributes delegate to the underlying crew.

### `AkashiCrewCallbacks(client, **options)`

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `client` | `AkashiSyncClient` | required | Configured Akashi client |
| `decision_type` | `str` | `"crew_task"` | Label applied to every trace and check |
| `confidence` | `float` | `0.7` | Default confidence score for traces (0--1) |
| `check_before_step` | `bool` | `True` | Call `check()` on tool-selection steps |
| `trace_task_output` | `bool` | `True` | Call `trace()` when a task completes |

**Methods:**

- `on_task_complete(task_output)` -- Pass as `task_callback=` to `Crew(...)`. Receives a `TaskOutput`-like object; reads `.raw`, `.agent`, and `.description` via `getattr` so it works across CrewAI versions.
- `on_step(agent_output)` -- Pass as `step_callback=` to `Crew(...)`. Receives an `AgentAction` or `AgentFinish`; duck-types `.tool` to decide whether to call `check()`.

### `make_hooks(client, **options) -> dict`

Returns `{"task_callback": ..., "step_callback": ...}`. Accepts the same options as `AkashiCrewCallbacks`. Both callbacks are bound to the same `AkashiCrewCallbacks` instance.

### `run_with_akashi(crew, client, inputs=None, *, decision_type="crew_run", confidence=0.7) -> CrewOutput`

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `crew` | `crewai.Crew` | required | Configured Crew instance |
| `client` | `AkashiSyncClient` | required | Configured Akashi client |
| `inputs` | `dict \| None` | `None` | Forwarded to `crew.kickoff(inputs=...)` |
| `decision_type` | `str` | `"crew_run"` | Decision type for the crew-level trace |
| `confidence` | `float` | `0.7` | Confidence for the crew-level trace |

Returns the `CrewOutput` from `crew.kickoff()` unchanged. If the crew raises an exception, it propagates normally -- `run_with_akashi` does not swallow crew errors, only Akashi errors.

## Trace content

**Per-task trace (`trace_task_output=True`):**
- `outcome`: `task_output.raw` (truncated to 500 characters)
- `reasoning`: `task_output.description` if non-empty, else `None`
- `metadata`: `{"agent": task_output.agent}`

**Crew-level trace (`AkashiCrew.kickoff` or `run_with_akashi`):**
- `outcome`: `str(crew_output)` (truncated to 500 characters)
- `metadata`: `{"inputs": str(inputs)[:200]}`

## Error handling

All Akashi calls are wrapped in `try/except`. Errors are logged at `DEBUG` level and execution continues. The crew itself is never affected by Akashi failures.

```python
import logging
logging.getLogger("akashi_crewai").setLevel(logging.DEBUG)
# DEBUG output: "akashi trace failed (non-fatal): Connection refused"
```

## Truncation

| Field | Limit |
|-------|-------|
| Task output / crew output (`outcome`) | 500 characters |
| Task description (`reasoning`) | 500 characters |
| Tool input in check query | 200 characters |
| Inputs in crew-level metadata | 200 characters |
