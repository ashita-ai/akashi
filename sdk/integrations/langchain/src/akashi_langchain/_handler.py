"""LangChain callback handler that traces agent decisions to Akashi."""

from __future__ import annotations

import logging
from typing import Any
from uuid import UUID

from langchain_core.callbacks import AsyncCallbackHandler, BaseCallbackHandler

from akashi import AkashiClient, AkashiSyncClient
from akashi.types import TraceRequest

logger = logging.getLogger(__name__)

_MAX_OUTCOME = 500
_MAX_REASONING = 500
_MAX_QUERY = 200


def _trunc(value: Any, max_len: int) -> str:
    s = str(value)
    return s[:max_len] if len(s) > max_len else s


class AkashiCallbackHandler(BaseCallbackHandler):
    """Synchronous LangChain callback handler that traces agent decisions to Akashi.

    Hooks into three lifecycle events:

    - ``on_agent_action``: the agent chose a tool. Calls ``check()`` to surface
      any relevant precedents before the action proceeds.
    - ``on_tool_end``: the tool completed. Records a ``trace()`` capturing which
      tool was used, the result, and the agent's reasoning.
    - ``on_agent_finish``: the agent produced its final answer. Records a
      ``trace()`` with the output and log.

    All Akashi calls are fire-and-forget: exceptions are logged at DEBUG level
    and never propagated into the calling chain.

    Usage::

        from akashi import AkashiSyncClient
        from akashi_langchain import AkashiCallbackHandler

        client = AkashiSyncClient(base_url="...", agent_id="...", api_key="...")
        handler = AkashiCallbackHandler(client, decision_type="my_agent")

        result = agent.invoke(
            {"input": "What is the capital of France?"},
            config={"callbacks": [handler]},
        )
    """

    raise_error: bool = False  # never bubble akashi errors into LangChain

    def __init__(
        self,
        client: AkashiSyncClient,
        *,
        decision_type: str = "agent_decision",
        confidence: float = 0.7,
        check_before_action: bool = True,
        trace_tool_use: bool = True,
        trace_final_answer: bool = True,
    ) -> None:
        """
        Args:
            client: A configured ``AkashiSyncClient``.
            decision_type: Label applied to every trace. Defaults to
                ``"agent_decision"``.
            confidence: Default confidence score for traced decisions (0–1).
            check_before_action: If True, call ``check()`` before each tool
                selection so the agent can surface relevant precedents.
            trace_tool_use: If True, record a trace after each tool call.
            trace_final_answer: If True, record a trace when the agent returns
                its final answer.
        """
        super().__init__()
        self.client = client
        self.decision_type = decision_type
        self.confidence = confidence
        self.check_before_action = check_before_action
        self.trace_tool_use = trace_tool_use
        self.trace_final_answer = trace_final_answer
        # Maps agent run_id → AgentAction for pending tool calls.
        self._pending: dict[UUID, Any] = {}

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
    # LangChain callbacks
    # ------------------------------------------------------------------

    def on_agent_action(
        self,
        action: Any,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        **kwargs: Any,
    ) -> None:
        """The agent chose a tool — check for precedents before executing it."""
        self._pending[run_id] = action
        if self.check_before_action:
            query = f"tool={action.tool} input={_trunc(action.tool_input, _MAX_QUERY)}"
            self._check(query)

    def on_tool_end(
        self,
        output: str,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        **kwargs: Any,
    ) -> None:
        """Tool completed — trace the tool-use decision."""
        if not self.trace_tool_use:
            return
        # parent_run_id is the agent's run_id, where we stored the action.
        action = self._pending.pop(parent_run_id, None) if parent_run_id else None
        if action is None:
            return
        self._trace(
            TraceRequest(
                decision_type=self.decision_type,
                outcome=f"used tool '{action.tool}': {_trunc(output, _MAX_OUTCOME)}",
                reasoning=_trunc(action.log, _MAX_REASONING) if action.log else None,
                confidence=self.confidence,
                metadata={
                    "tool": action.tool,
                    "tool_input": _trunc(action.tool_input, _MAX_QUERY),
                },
            )
        )

    def on_tool_error(
        self,
        error: BaseException,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        **kwargs: Any,
    ) -> None:
        """Clean up the pending action entry on tool failure."""
        if parent_run_id:
            self._pending.pop(parent_run_id, None)

    def on_agent_finish(
        self,
        finish: Any,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        **kwargs: Any,
    ) -> None:
        """The agent produced its final answer — trace the decision."""
        if not self.trace_final_answer:
            return
        output = finish.return_values.get("output", str(finish.return_values))
        log = getattr(finish, "log", None)
        self._trace(
            TraceRequest(
                decision_type=self.decision_type,
                outcome=_trunc(output, _MAX_OUTCOME),
                reasoning=_trunc(log, _MAX_REASONING) if log else None,
                confidence=self.confidence,
            )
        )


class AsyncAkashiCallbackHandler(AsyncCallbackHandler):
    """Async variant of :class:`AkashiCallbackHandler`.

    Identical contract but uses ``AkashiClient`` (async) instead of
    ``AkashiSyncClient``.  Use with async LangChain chains::

        client = AkashiClient(base_url="...", agent_id="...", api_key="...")
        handler = AsyncAkashiCallbackHandler(client)

        result = await chain.ainvoke(
            {"input": "..."},
            config={"callbacks": [handler]},
        )
    """

    raise_error: bool = False

    def __init__(
        self,
        client: AkashiClient,
        *,
        decision_type: str = "agent_decision",
        confidence: float = 0.7,
        check_before_action: bool = True,
        trace_tool_use: bool = True,
        trace_final_answer: bool = True,
    ) -> None:
        super().__init__()
        self.client = client
        self.decision_type = decision_type
        self.confidence = confidence
        self.check_before_action = check_before_action
        self.trace_tool_use = trace_tool_use
        self.trace_final_answer = trace_final_answer
        self._pending: dict[UUID, Any] = {}

    # ------------------------------------------------------------------
    # Async helpers
    # ------------------------------------------------------------------

    async def _check(self, query: str | None) -> None:
        try:
            await self.client.check(self.decision_type, query)
        except Exception as exc:  # noqa: BLE001
            logger.debug("akashi check failed (non-fatal): %s", exc)

    async def _trace(self, req: TraceRequest) -> None:
        try:
            await self.client.trace(req)
        except Exception as exc:  # noqa: BLE001
            logger.debug("akashi trace failed (non-fatal): %s", exc)

    # ------------------------------------------------------------------
    # LangChain async callbacks
    # ------------------------------------------------------------------

    async def on_agent_action(
        self,
        action: Any,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        **kwargs: Any,
    ) -> None:
        self._pending[run_id] = action
        if self.check_before_action:
            query = f"tool={action.tool} input={_trunc(action.tool_input, _MAX_QUERY)}"
            await self._check(query)

    async def on_tool_end(
        self,
        output: str,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        **kwargs: Any,
    ) -> None:
        if not self.trace_tool_use:
            return
        action = self._pending.pop(parent_run_id, None) if parent_run_id else None
        if action is None:
            return
        await self._trace(
            TraceRequest(
                decision_type=self.decision_type,
                outcome=f"used tool '{action.tool}': {_trunc(output, _MAX_OUTCOME)}",
                reasoning=_trunc(action.log, _MAX_REASONING) if action.log else None,
                confidence=self.confidence,
                metadata={
                    "tool": action.tool,
                    "tool_input": _trunc(action.tool_input, _MAX_QUERY),
                },
            )
        )

    async def on_tool_error(
        self,
        error: BaseException,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        **kwargs: Any,
    ) -> None:
        if parent_run_id:
            self._pending.pop(parent_run_id, None)

    async def on_agent_finish(
        self,
        finish: Any,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        **kwargs: Any,
    ) -> None:
        if not self.trace_final_answer:
            return
        output = finish.return_values.get("output", str(finish.return_values))
        log = getattr(finish, "log", None)
        await self._trace(
            TraceRequest(
                decision_type=self.decision_type,
                outcome=_trunc(output, _MAX_OUTCOME),
                reasoning=_trunc(log, _MAX_REASONING) if log else None,
                confidence=self.confidence,
            )
        )
