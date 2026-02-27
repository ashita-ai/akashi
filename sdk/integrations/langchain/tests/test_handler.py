"""Tests for AkashiCallbackHandler and AsyncAkashiCallbackHandler."""

from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, MagicMock, call
from uuid import uuid4

import pytest

from akashi import AkashiClient, AkashiSyncClient
from akashi.types import TraceRequest
from akashi_langchain import AkashiCallbackHandler, AsyncAkashiCallbackHandler


def _make_sync_client() -> MagicMock:
    client = MagicMock(spec=AkashiSyncClient)
    client.trace.return_value = MagicMock()
    client.check.return_value = MagicMock()
    return client


def _make_async_client() -> AsyncMock:
    client = AsyncMock(spec=AkashiClient)
    return client


def _agent_action(tool: str = "calculator", tool_input: str = "2+2", log: str = "Using calc") -> MagicMock:
    action = MagicMock()
    action.tool = tool
    action.tool_input = tool_input
    action.log = log
    return action


def _agent_finish(output: str = "The answer is 42", log: str = "I found it") -> MagicMock:
    finish = MagicMock()
    finish.return_values = {"output": output}
    finish.log = log
    return finish


# ---------------------------------------------------------------------------
# Sync handler
# ---------------------------------------------------------------------------


class TestAkashiCallbackHandler:
    def test_traces_final_answer(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client, decision_type="test_agent")

        handler.on_agent_finish(_agent_finish("Paris"), run_id=uuid4())

        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert "Paris" in req.outcome
        assert req.decision_type == "test_agent"
        assert req.confidence == 0.7

    def test_traces_reasoning_from_finish_log(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client)

        handler.on_agent_finish(_agent_finish(log="My reasoning"), run_id=uuid4())

        req: TraceRequest = client.trace.call_args[0][0]
        assert req.reasoning == "My reasoning"

    def test_traces_tool_use(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client)

        agent_run_id = uuid4()
        tool_run_id = uuid4()
        action = _agent_action("search", "AI news", "Searching for news")

        handler.on_agent_action(action, run_id=agent_run_id)
        handler.on_tool_end("Results: ...", run_id=tool_run_id, parent_run_id=agent_run_id)

        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert "search" in req.outcome
        assert req.reasoning == "Searching for news"
        assert req.metadata["tool"] == "search"

    def test_checks_before_agent_action(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client, check_before_action=True)

        action = _agent_action("calculator", "1+1")
        handler.on_agent_action(action, run_id=uuid4())

        client.check.assert_called_once()
        check_args = client.check.call_args
        assert "calculator" in check_args[0][1]  # query contains tool name

    def test_no_check_when_disabled(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client, check_before_action=False)

        handler.on_agent_action(_agent_action(), run_id=uuid4())

        client.check.assert_not_called()

    def test_no_tool_trace_when_disabled(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client, trace_tool_use=False)

        agent_run_id = uuid4()
        handler.on_agent_action(_agent_action(), run_id=agent_run_id)
        handler.on_tool_end("result", run_id=uuid4(), parent_run_id=agent_run_id)

        client.trace.assert_not_called()

    def test_no_final_trace_when_disabled(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client, trace_final_answer=False)

        handler.on_agent_finish(_agent_finish(), run_id=uuid4())

        client.trace.assert_not_called()

    def test_cleans_up_pending_on_tool_error(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client)

        agent_run_id = uuid4()
        handler.on_agent_action(_agent_action(), run_id=agent_run_id)
        assert agent_run_id in handler._pending

        handler.on_tool_error(RuntimeError("boom"), run_id=uuid4(), parent_run_id=agent_run_id)

        assert agent_run_id not in handler._pending

    def test_akashi_error_does_not_propagate_on_trace(self) -> None:
        client = _make_sync_client()
        client.trace.side_effect = ConnectionError("akashi is down")
        handler = AkashiCallbackHandler(client)

        # Must not raise.
        handler.on_agent_finish(_agent_finish(), run_id=uuid4())

    def test_akashi_error_does_not_propagate_on_check(self) -> None:
        client = _make_sync_client()
        client.check.side_effect = ConnectionError("akashi is down")
        handler = AkashiCallbackHandler(client)

        # Must not raise.
        handler.on_agent_action(_agent_action(), run_id=uuid4())

    def test_tool_end_without_matching_action_is_noop(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client)

        # No on_agent_action call before on_tool_end.
        handler.on_tool_end("result", run_id=uuid4(), parent_run_id=uuid4())

        client.trace.assert_not_called()

    def test_outcome_truncated_to_500_chars(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client)

        long_output = "x" * 1000
        handler.on_agent_finish(_agent_finish(output=long_output), run_id=uuid4())

        req: TraceRequest = client.trace.call_args[0][0]
        assert len(req.outcome) == 500

    def test_custom_confidence(self) -> None:
        client = _make_sync_client()
        handler = AkashiCallbackHandler(client, confidence=0.9)

        handler.on_agent_finish(_agent_finish(), run_id=uuid4())

        req: TraceRequest = client.trace.call_args[0][0]
        assert req.confidence == 0.9


# ---------------------------------------------------------------------------
# Async handler
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
class TestAsyncAkashiCallbackHandler:
    async def test_traces_final_answer(self) -> None:
        client = _make_async_client()
        handler = AsyncAkashiCallbackHandler(client, decision_type="async_agent")

        await handler.on_agent_finish(_agent_finish("Berlin"), run_id=uuid4())

        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert "Berlin" in req.outcome

    async def test_checks_before_agent_action(self) -> None:
        client = _make_async_client()
        handler = AsyncAkashiCallbackHandler(client)

        await handler.on_agent_action(_agent_action("web_search"), run_id=uuid4())

        client.check.assert_called_once()

    async def test_traces_tool_use(self) -> None:
        client = _make_async_client()
        handler = AsyncAkashiCallbackHandler(client)

        agent_run_id = uuid4()
        action = _agent_action("summarize", "article text")

        await handler.on_agent_action(action, run_id=agent_run_id)
        await handler.on_tool_end("Summary: ...", run_id=uuid4(), parent_run_id=agent_run_id)

        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert "summarize" in req.outcome

    async def test_error_does_not_propagate(self) -> None:
        client = _make_async_client()
        client.trace.side_effect = RuntimeError("network error")
        handler = AsyncAkashiCallbackHandler(client)

        # Must not raise.
        await handler.on_agent_finish(_agent_finish(), run_id=uuid4())
