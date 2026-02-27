"""Comprehensive tests for AkashiCallbackHandler and AsyncAkashiCallbackHandler."""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock, call
from uuid import uuid4

import pytest

from akashi import AkashiClient, AkashiSyncClient
from akashi.types import TraceRequest
from akashi_langchain import AkashiCallbackHandler, AsyncAkashiCallbackHandler


# ---------------------------------------------------------------------------
# Factories
# ---------------------------------------------------------------------------


def sync_client() -> MagicMock:
    client = MagicMock(spec=AkashiSyncClient)
    client.trace.return_value = MagicMock()
    client.check.return_value = MagicMock()
    return client


def async_client() -> AsyncMock:
    return AsyncMock(spec=AkashiClient)


def action(tool: str = "calculator", tool_input: str = "2+2", log: str = "Using calc") -> MagicMock:
    a = MagicMock()
    a.tool = tool
    a.tool_input = tool_input
    a.log = log
    return a


def finish(output: str = "42", log: str = "I found it") -> MagicMock:
    f = MagicMock()
    f.return_values = {"output": output}
    f.log = log
    return f


def _trace_req(client: MagicMock) -> TraceRequest:
    """Extract the first TraceRequest passed to client.trace."""
    return client.trace.call_args[0][0]


# ===========================================================================
# Sync handler — happy path
# ===========================================================================


class TestSyncHandlerHappyPath:
    def test_traces_agent_finish(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish("Paris"), run_id=uuid4())
        client.trace.assert_called_once()

    def test_trace_outcome_contains_output(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish("The Eiffel Tower"), run_id=uuid4())
        assert "Eiffel Tower" in _trace_req(client).outcome

    def test_trace_decision_type_from_constructor(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client, decision_type="my_agent")
        h.on_agent_finish(finish(), run_id=uuid4())
        assert _trace_req(client).decision_type == "my_agent"

    def test_trace_default_decision_type(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish(), run_id=uuid4())
        assert _trace_req(client).decision_type == "agent_decision"

    def test_trace_reasoning_from_finish_log(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish(log="My step-by-step reasoning"), run_id=uuid4())
        assert _trace_req(client).reasoning == "My step-by-step reasoning"

    def test_trace_reasoning_none_when_log_is_none(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        f = finish()
        f.log = None
        h.on_agent_finish(f, run_id=uuid4())
        assert _trace_req(client).reasoning is None

    def test_trace_reasoning_none_when_log_missing(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        f = MagicMock()
        f.return_values = {"output": "ok"}
        del f.log
        h.on_agent_finish(f, run_id=uuid4())
        assert _trace_req(client).reasoning is None

    def test_trace_custom_confidence(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client, confidence=0.9)
        h.on_agent_finish(finish(), run_id=uuid4())
        assert _trace_req(client).confidence == 0.9

    def test_trace_default_confidence(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish(), run_id=uuid4())
        assert _trace_req(client).confidence == 0.7

    def test_check_called_before_action(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_action(action("search"), run_id=uuid4())
        client.check.assert_called_once()

    def test_check_query_contains_tool_name(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_action(action("web_search", "AI news"), run_id=uuid4())
        query = client.check.call_args[0][1]
        assert "web_search" in query

    def test_check_query_contains_tool_input(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_action(action("calc", "1 + 1"), run_id=uuid4())
        query = client.check.call_args[0][1]
        assert "1 + 1" in query

    def test_check_decision_type_matches_constructor(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client, decision_type="dt_check")
        h.on_agent_action(action(), run_id=uuid4())
        assert client.check.call_args[0][0] == "dt_check"

    def test_tool_end_traces_tool_decision(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action("search"), run_id=agent_id)
        h.on_tool_end("Result: AI news", run_id=uuid4(), parent_run_id=agent_id)
        client.trace.assert_called_once()

    def test_tool_end_outcome_contains_tool_name(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action("calculator"), run_id=agent_id)
        h.on_tool_end("4", run_id=uuid4(), parent_run_id=agent_id)
        assert "calculator" in _trace_req(client).outcome

    def test_tool_end_outcome_contains_result(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action("calculator"), run_id=agent_id)
        h.on_tool_end("the result is 99", run_id=uuid4(), parent_run_id=agent_id)
        assert "99" in _trace_req(client).outcome

    def test_tool_end_reasoning_from_action_log(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action("t", "i", log="I'll use this tool"), run_id=agent_id)
        h.on_tool_end("out", run_id=uuid4(), parent_run_id=agent_id)
        assert _trace_req(client).reasoning == "I'll use this tool"

    def test_tool_end_metadata_has_tool_name(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action("mytool"), run_id=agent_id)
        h.on_tool_end("out", run_id=uuid4(), parent_run_id=agent_id)
        assert _trace_req(client).metadata["tool"] == "mytool"

    def test_tool_end_metadata_has_tool_input(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action("t", "my input"), run_id=agent_id)
        h.on_tool_end("out", run_id=uuid4(), parent_run_id=agent_id)
        assert "my input" in str(_trace_req(client).metadata["tool_input"])


# ===========================================================================
# Sync handler — multiple calls / state management
# ===========================================================================


class TestSyncHandlerStateManagement:
    def test_two_sequential_tool_uses(self) -> None:
        """Each action+tool_end pair should produce exactly one trace."""
        client = sync_client()
        h = AkashiCallbackHandler(client)

        id1, id2 = uuid4(), uuid4()
        h.on_agent_action(action("tool1"), run_id=id1)
        h.on_tool_end("r1", run_id=uuid4(), parent_run_id=id1)
        h.on_agent_action(action("tool2"), run_id=id2)
        h.on_tool_end("r2", run_id=uuid4(), parent_run_id=id2)

        assert client.trace.call_count == 2

    def test_two_parallel_run_ids_tracked_independently(self) -> None:
        """Concurrent tool calls from different run_ids don't interfere."""
        client = sync_client()
        h = AkashiCallbackHandler(client)

        id_a, id_b = uuid4(), uuid4()
        h.on_agent_action(action("tool_a"), run_id=id_a)
        h.on_agent_action(action("tool_b"), run_id=id_b)

        # End them out of order.
        h.on_tool_end("result_b", run_id=uuid4(), parent_run_id=id_b)
        h.on_tool_end("result_a", run_id=uuid4(), parent_run_id=id_a)

        assert client.trace.call_count == 2
        outcomes = [c[0][0].outcome for c in client.trace.call_args_list]
        assert any("tool_b" in o for o in outcomes)
        assert any("tool_a" in o for o in outcomes)

    def test_pending_action_cleaned_up_after_tool_end(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action(), run_id=agent_id)
        assert agent_id in h._pending
        h.on_tool_end("r", run_id=uuid4(), parent_run_id=agent_id)
        assert agent_id not in h._pending

    def test_handler_reusable_across_multiple_independent_runs(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)

        for _ in range(3):
            h.on_agent_finish(finish(), run_id=uuid4())

        assert client.trace.call_count == 3

    def test_check_and_trace_both_called_for_tool_use(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action(), run_id=agent_id)
        h.on_tool_end("out", run_id=uuid4(), parent_run_id=agent_id)

        client.check.assert_called_once()
        client.trace.assert_called_once()


# ===========================================================================
# Sync handler — disabled flags
# ===========================================================================


class TestSyncHandlerDisabledFlags:
    def test_no_check_when_check_before_action_false(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client, check_before_action=False)
        h.on_agent_action(action(), run_id=uuid4())
        client.check.assert_not_called()

    def test_no_tool_trace_when_trace_tool_use_false(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client, trace_tool_use=False)
        agent_id = uuid4()
        h.on_agent_action(action(), run_id=agent_id)
        h.on_tool_end("r", run_id=uuid4(), parent_run_id=agent_id)
        client.trace.assert_not_called()

    def test_no_final_trace_when_trace_final_answer_false(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client, trace_final_answer=False)
        h.on_agent_finish(finish(), run_id=uuid4())
        client.trace.assert_not_called()

    def test_check_still_called_when_trace_final_answer_false(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client, trace_final_answer=False)
        h.on_agent_action(action(), run_id=uuid4())
        client.check.assert_called_once()


# ===========================================================================
# Sync handler — truncation
# ===========================================================================


class TestSyncHandlerTruncation:
    def test_outcome_truncated_at_500(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish(output="x" * 1000), run_id=uuid4())
        assert len(_trace_req(client).outcome) == 500

    def test_outcome_not_truncated_when_short(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish(output="short"), run_id=uuid4())
        assert "short" in _trace_req(client).outcome

    def test_reasoning_truncated_at_500(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish(log="y" * 1000), run_id=uuid4())
        assert len(_trace_req(client).reasoning) == 500

    def test_check_query_truncated_at_200(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_agent_action(action("t", "z" * 500), run_id=uuid4())
        query = client.check.call_args[0][1]
        # query is "tool=t input=<200-char truncated input>"
        assert len(query) < 230  # tool= + tool_name + " input=" + 200

    def test_tool_end_outcome_truncated(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action("t"), run_id=agent_id)
        h.on_tool_end("q" * 1000, run_id=uuid4(), parent_run_id=agent_id)
        # outcome = "used tool 't': <truncated output>"
        assert len(_trace_req(client).outcome) <= 520  # "used tool 't': " + 500


# ===========================================================================
# Sync handler — edge cases / no-ops
# ===========================================================================


class TestSyncHandlerEdgeCases:
    def test_tool_end_without_parent_run_id_is_noop(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_tool_end("result", run_id=uuid4(), parent_run_id=None)
        client.trace.assert_not_called()

    def test_tool_end_with_unknown_parent_run_id_is_noop(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        h.on_tool_end("result", run_id=uuid4(), parent_run_id=uuid4())
        client.trace.assert_not_called()

    def test_tool_error_cleans_up_pending_action(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action(), run_id=agent_id)
        h.on_tool_error(RuntimeError("boom"), run_id=uuid4(), parent_run_id=agent_id)
        assert agent_id not in h._pending

    def test_tool_error_with_no_parent_run_id_is_safe(self) -> None:
        client = sync_client()
        h = AkashiCallbackHandler(client)
        # Should not raise.
        h.on_tool_error(RuntimeError("boom"), run_id=uuid4(), parent_run_id=None)

    def test_agent_finish_return_values_non_output_key(self) -> None:
        """return_values without 'output' key falls back to str(return_values)."""
        client = sync_client()
        h = AkashiCallbackHandler(client)
        f = MagicMock()
        f.return_values = {"result": "something"}
        f.log = None
        h.on_agent_finish(f, run_id=uuid4())
        assert "something" in _trace_req(client).outcome


# ===========================================================================
# Sync handler — error isolation
# ===========================================================================


class TestSyncHandlerErrorIsolation:
    def test_trace_error_does_not_propagate_on_finish(self) -> None:
        client = sync_client()
        client.trace.side_effect = RuntimeError("akashi down")
        h = AkashiCallbackHandler(client)
        h.on_agent_finish(finish(), run_id=uuid4())  # must not raise

    def test_trace_error_does_not_propagate_on_tool_end(self) -> None:
        client = sync_client()
        client.trace.side_effect = ConnectionError("timeout")
        h = AkashiCallbackHandler(client)
        agent_id = uuid4()
        h.on_agent_action(action(), run_id=agent_id)
        h.on_tool_end("r", run_id=uuid4(), parent_run_id=agent_id)  # must not raise

    def test_check_error_does_not_propagate_on_action(self) -> None:
        client = sync_client()
        client.check.side_effect = RuntimeError("akashi down")
        h = AkashiCallbackHandler(client)
        h.on_agent_action(action(), run_id=uuid4())  # must not raise

    def test_chain_continues_normally_after_akashi_error(self) -> None:
        """After a failed trace, the handler still processes subsequent calls."""
        client = sync_client()
        client.trace.side_effect = [RuntimeError("first fails"), MagicMock()]
        h = AkashiCallbackHandler(client)

        h.on_agent_finish(finish("first"), run_id=uuid4())
        h.on_agent_finish(finish("second"), run_id=uuid4())

        # Two calls were made despite the first one erroring.
        assert client.trace.call_count == 2


# ===========================================================================
# Async handler
# ===========================================================================


class TestAsyncHandler:
    async def test_traces_final_answer(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        await h.on_agent_finish(finish("Berlin"), run_id=uuid4())
        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert "Berlin" in req.outcome

    async def test_decision_type_from_constructor(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client, decision_type="async_agent")
        await h.on_agent_finish(finish(), run_id=uuid4())
        assert client.trace.call_args[0][0].decision_type == "async_agent"

    async def test_checks_before_action(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        await h.on_agent_action(action("web_search"), run_id=uuid4())
        client.check.assert_called_once()

    async def test_check_query_contains_tool(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        await h.on_agent_action(action("my_tool", "input val"), run_id=uuid4())
        query = client.check.call_args[0][1]
        assert "my_tool" in query
        assert "input val" in query

    async def test_traces_tool_use(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        agent_id = uuid4()
        await h.on_agent_action(action("summarize"), run_id=agent_id)
        await h.on_tool_end("Summary: done", run_id=uuid4(), parent_run_id=agent_id)
        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert "summarize" in req.outcome

    async def test_tool_end_reasoning(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        agent_id = uuid4()
        await h.on_agent_action(action("t", "i", log="async reason"), run_id=agent_id)
        await h.on_tool_end("out", run_id=uuid4(), parent_run_id=agent_id)
        assert client.trace.call_args[0][0].reasoning == "async reason"

    async def test_cleans_up_pending_on_tool_error(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        agent_id = uuid4()
        await h.on_agent_action(action(), run_id=agent_id)
        assert agent_id in h._pending
        await h.on_tool_error(RuntimeError("err"), run_id=uuid4(), parent_run_id=agent_id)
        assert agent_id not in h._pending

    async def test_two_sequential_tool_uses(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        id1, id2 = uuid4(), uuid4()
        await h.on_agent_action(action("t1"), run_id=id1)
        await h.on_tool_end("r1", run_id=uuid4(), parent_run_id=id1)
        await h.on_agent_action(action("t2"), run_id=id2)
        await h.on_tool_end("r2", run_id=uuid4(), parent_run_id=id2)
        assert client.trace.call_count == 2

    async def test_outcome_truncated(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        await h.on_agent_finish(finish(output="z" * 1000), run_id=uuid4())
        assert len(client.trace.call_args[0][0].outcome) == 500

    async def test_reasoning_truncated(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        await h.on_agent_finish(finish(log="w" * 1000), run_id=uuid4())
        assert len(client.trace.call_args[0][0].reasoning) == 500

    async def test_error_does_not_propagate_on_finish(self) -> None:
        client = async_client()
        client.trace.side_effect = RuntimeError("down")
        h = AsyncAkashiCallbackHandler(client)
        await h.on_agent_finish(finish(), run_id=uuid4())  # must not raise

    async def test_error_does_not_propagate_on_check(self) -> None:
        client = async_client()
        client.check.side_effect = RuntimeError("down")
        h = AsyncAkashiCallbackHandler(client)
        await h.on_agent_action(action(), run_id=uuid4())  # must not raise

    async def test_no_check_when_disabled(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client, check_before_action=False)
        await h.on_agent_action(action(), run_id=uuid4())
        client.check.assert_not_called()

    async def test_no_trace_when_trace_final_answer_disabled(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client, trace_final_answer=False)
        await h.on_agent_finish(finish(), run_id=uuid4())
        client.trace.assert_not_called()

    async def test_no_tool_trace_when_disabled(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client, trace_tool_use=False)
        agent_id = uuid4()
        await h.on_agent_action(action(), run_id=agent_id)
        await h.on_tool_end("r", run_id=uuid4(), parent_run_id=agent_id)
        client.trace.assert_not_called()

    async def test_tool_end_without_parent_run_id_is_noop(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client)
        await h.on_tool_end("r", run_id=uuid4(), parent_run_id=None)
        client.trace.assert_not_called()

    async def test_custom_confidence_in_trace(self) -> None:
        client = async_client()
        h = AsyncAkashiCallbackHandler(client, confidence=0.95)
        await h.on_agent_finish(finish(), run_id=uuid4())
        assert client.trace.call_args[0][0].confidence == 0.95
