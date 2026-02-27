"""CrewAI callbacks that trace task decisions to Akashi."""

from __future__ import annotations

import logging
from typing import Any

from akashi import AkashiSyncClient
from akashi.types import TraceRequest

logger = logging.getLogger(__name__)

_MAX_OUTCOME = 500
_MAX_REASONING = 500
_MAX_QUERY = 200


def _trunc(value: Any, max_len: int) -> str:
    s = str(value)
    return s[:max_len] if len(s) > max_len else s


class AkashiCrewCallbacks:
    """Stateful callbacks for a CrewAI ``Crew``.

    Provides two methods designed to be passed as ``task_callback`` and
    ``step_callback`` to ``Crew(...)``.

    - ``on_task_complete``: Called by CrewAI after each task finishes.  Records
      an Akashi trace with the task output and the agent that completed it.
    - ``on_step``: Called by CrewAI after each agent step (an individual tool use
      or chain-of-thought cycle within a task). Calls Akashi ``check()`` when the
      agent is choosing a tool.

    Usage::

        from akashi import AkashiSyncClient
        from akashi_crewai import AkashiCrewCallbacks

        client = AkashiSyncClient(base_url="...", agent_id="...", api_key="...")
        hooks = AkashiCrewCallbacks(client, decision_type="research_task")

        crew = Crew(
            agents=[researcher, writer],
            tasks=[research_task, write_task],
            task_callback=hooks.on_task_complete,
            step_callback=hooks.on_step,
        )

    Alternatively, use :func:`make_hooks` to get the ``Crew`` kwargs directly::

        crew = Crew(
            agents=[...],
            tasks=[...],
            **make_hooks(client, decision_type="research_task"),
        )

    Or wrap an existing crew with :func:`run_with_akashi` to add check/trace
    around the entire crew run::

        result = run_with_akashi(crew, client, inputs={"topic": "AI trends"})
    """

    def __init__(
        self,
        client: AkashiSyncClient,
        *,
        decision_type: str = "crew_task",
        confidence: float = 0.7,
        check_before_step: bool = True,
        trace_task_output: bool = True,
    ) -> None:
        """
        Args:
            client: A configured ``AkashiSyncClient``.
            decision_type: Label applied to every trace. Defaults to
                ``"crew_task"``.
            confidence: Default confidence score for traced decisions (0–1).
            check_before_step: If True, call ``check()`` when an agent step
                contains a tool selection (the ``AgentAction`` case).
            trace_task_output: If True, call ``trace()`` when a task completes.
        """
        self.client = client
        self.decision_type = decision_type
        self.confidence = confidence
        self.check_before_step = check_before_step
        self.trace_task_output = trace_task_output

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _check(self, query: str | None) -> None:
        try:
            self.client.check(self.decision_type, query)
        except Exception as exc:  # noqa: BLE001
            logger.debug("akashi check failed (non-fatal): %s", exc)

    def _trace(self, req: TraceRequest) -> None:
        try:
            self.client.trace(req)
        except Exception as exc:  # noqa: BLE001
            logger.debug("akashi trace failed (non-fatal): %s", exc)

    # ------------------------------------------------------------------
    # CrewAI callbacks
    # ------------------------------------------------------------------

    def on_task_complete(self, task_output: Any) -> None:
        """Called by CrewAI after each task finishes.

        ``task_output`` is a ``crewai.tasks.task_output.TaskOutput`` with at
        minimum ``.raw`` (str), ``.agent`` (str), and ``.description`` (str).
        The callback duck-types these attributes so it works across CrewAI
        versions without a hard import of CrewAI's internal classes.
        """
        if not self.trace_task_output:
            return

        raw = _trunc(getattr(task_output, "raw", str(task_output)), _MAX_OUTCOME)
        description = _trunc(getattr(task_output, "description", ""), _MAX_REASONING)
        agent = str(getattr(task_output, "agent", "unknown"))

        self._trace(
            TraceRequest(
                decision_type=self.decision_type,
                outcome=raw,
                reasoning=description if description else None,
                confidence=self.confidence,
                metadata={"agent": agent},
            )
        )

    def on_step(self, agent_output: Any) -> None:
        """Called by CrewAI after each agent step.

        ``agent_output`` may be a LangChain ``AgentAction`` (the agent chose a
        tool and produced a log) or an ``AgentFinish`` (the agent is done with
        its current task step). We check for precedents only on ``AgentAction``
        because that is the moment of tool selection.
        """
        if not self.check_before_step:
            return

        # Duck-type the presence of .tool to distinguish AgentAction from
        # AgentFinish without importing LangChain's model types.
        tool = getattr(agent_output, "tool", None)
        if tool is not None:
            tool_input = getattr(agent_output, "tool_input", "")
            query = f"tool={tool} input={_trunc(tool_input, _MAX_QUERY)}"
            self._check(query)


def make_hooks(
    client: AkashiSyncClient,
    *,
    decision_type: str = "crew_task",
    confidence: float = 0.7,
    check_before_step: bool = True,
    trace_task_output: bool = True,
) -> dict[str, Any]:
    """Return ``Crew`` kwargs that wire Akashi callbacks.

    Pass the returned dict as ``**kwargs`` when constructing a ``Crew``::

        crew = Crew(
            agents=[researcher, writer],
            tasks=[research_task, write_task],
            **make_hooks(client, decision_type="research"),
        )
    """
    hooks = AkashiCrewCallbacks(
        client,
        decision_type=decision_type,
        confidence=confidence,
        check_before_step=check_before_step,
        trace_task_output=trace_task_output,
    )
    return {
        "task_callback": hooks.on_task_complete,
        "step_callback": hooks.on_step,
    }


def run_with_akashi(
    crew: Any,
    client: AkashiSyncClient,
    inputs: dict[str, Any] | None = None,
    *,
    decision_type: str = "crew_run",
    confidence: float = 0.7,
) -> Any:
    """Run a ``Crew`` with Akashi check-before / trace-after semantics.

    Calls ``check()`` before the crew starts and ``trace()`` after it completes,
    capturing the crew's overall output as a single decision. Per-task tracing
    is separate and can be configured by wiring :func:`make_hooks` into the
    crew at construction time.

    Args:
        crew: A configured ``crewai.Crew`` instance.
        client: A configured ``AkashiSyncClient``.
        inputs: Optional keyword inputs forwarded to ``crew.kickoff(inputs=...)``.
        decision_type: Decision type label for the crew-level trace.
        confidence: Confidence score for the crew-level trace.

    Returns:
        The ``CrewOutput`` returned by ``crew.kickoff()``.
    """
    query = _trunc(str(inputs), _MAX_QUERY) if inputs else None
    try:
        client.check(decision_type, query)
    except Exception as exc:  # noqa: BLE001
        logger.debug("akashi check failed (non-fatal): %s", exc)

    result = crew.kickoff(inputs=inputs)

    try:
        client.trace(
            TraceRequest(
                decision_type=decision_type,
                outcome=_trunc(str(result), _MAX_OUTCOME),
                confidence=confidence,
                metadata={"inputs": str(inputs)[:200] if inputs else ""},
            )
        )
    except Exception as exc:  # noqa: BLE001
        logger.debug("akashi trace failed (non-fatal): %s", exc)

    return result
