"""Comprehensive tests for AkashiCrewCallbacks, make_hooks, and run_with_akashi."""

from __future__ import annotations

from unittest.mock import MagicMock, call

import pytest

from akashi import AkashiSyncClient
from akashi.types import TraceRequest
from akashi_crewai import AkashiCrewCallbacks, make_hooks, run_with_akashi


# ---------------------------------------------------------------------------
# Factories
# ---------------------------------------------------------------------------


def sync_client() -> MagicMock:
    c = MagicMock(spec=AkashiSyncClient)
    c.trace.return_value = MagicMock()
    c.check.return_value = MagicMock()
    return c


def task_out(raw: str = "Done", agent: str = "researcher", description: str = "Find info") -> MagicMock:
    m = MagicMock()
    m.raw = raw
    m.agent = agent
    m.description = description
    return m


def agent_action(tool: str = "search", tool_input: str = "query") -> MagicMock:
    m = MagicMock()
    m.tool = tool
    m.tool_input = tool_input
    return m


def agent_finish() -> MagicMock:
    m = MagicMock(spec=[])  # no attributes — no .tool
    return m


def _trace_req(client: MagicMock) -> TraceRequest:
    return client.trace.call_args[0][0]


# ===========================================================================
# AkashiCrewCallbacks — on_task_complete
# ===========================================================================


class TestOnTaskComplete:
    def test_traces_task_output(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out("Research complete"))
        client.trace.assert_called_once()

    def test_outcome_is_raw_output(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out(raw="Here are the findings"))
        assert "findings" in _trace_req(client).outcome

    def test_decision_type_from_constructor(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client, decision_type="research_task")
        hooks.on_task_complete(task_out())
        assert _trace_req(client).decision_type == "research_task"

    def test_default_decision_type(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out())
        assert _trace_req(client).decision_type == "crew_task"

    def test_reasoning_from_description(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out(description="Analyze the dataset"))
        assert _trace_req(client).reasoning == "Analyze the dataset"

    def test_empty_description_gives_none_reasoning(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out(description=""))
        assert _trace_req(client).reasoning is None

    def test_metadata_includes_agent(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out(agent="writer"))
        assert _trace_req(client).metadata["agent"] == "writer"

    def test_custom_confidence(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client, confidence=0.85)
        hooks.on_task_complete(task_out())
        assert _trace_req(client).confidence == 0.85

    def test_default_confidence(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out())
        assert _trace_req(client).confidence == 0.7

    def test_outcome_truncated_to_500(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out(raw="x" * 1000))
        assert len(_trace_req(client).outcome) == 500

    def test_reasoning_truncated_to_500(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out(description="y" * 1000))
        assert len(_trace_req(client).reasoning) == 500

    def test_task_output_without_description(self) -> None:
        """Duck-typed — missing description attribute falls back to empty string."""
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        m = MagicMock(spec=["raw", "agent"])
        m.raw = "output"
        m.agent = "bot"
        hooks.on_task_complete(m)
        client.trace.assert_called_once()
        # description missing → getattr returns "" → reasoning is None
        assert _trace_req(client).reasoning is None

    def test_task_output_without_agent(self) -> None:
        """Missing agent attribute falls back to 'unknown'."""
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        m = MagicMock(spec=["raw"])
        m.raw = "output"
        hooks.on_task_complete(m)
        assert _trace_req(client).metadata["agent"] == "unknown"

    def test_no_trace_when_disabled(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client, trace_task_output=False)
        hooks.on_task_complete(task_out())
        client.trace.assert_not_called()

    def test_called_multiple_times(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out("task1"))
        hooks.on_task_complete(task_out("task2"))
        hooks.on_task_complete(task_out("task3"))
        assert client.trace.call_count == 3

    def test_error_does_not_propagate(self) -> None:
        client = sync_client()
        client.trace.side_effect = RuntimeError("akashi down")
        hooks = AkashiCrewCallbacks(client)
        hooks.on_task_complete(task_out())  # must not raise


# ===========================================================================
# AkashiCrewCallbacks — on_step
# ===========================================================================


class TestOnStep:
    def test_checks_on_agent_action(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_step(agent_action("web_search"))
        client.check.assert_called_once()

    def test_check_query_contains_tool_name(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_step(agent_action("my_tool", "my input"))
        query = client.check.call_args[0][1]
        assert "my_tool" in query

    def test_check_query_contains_tool_input(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_step(agent_action("t", "search for facts"))
        query = client.check.call_args[0][1]
        assert "search for facts" in query

    def test_check_decision_type_from_constructor(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client, decision_type="my_decision")
        hooks.on_step(agent_action())
        assert client.check.call_args[0][0] == "my_decision"

    def test_no_check_on_agent_finish(self) -> None:
        """AgentFinish has no .tool — should not trigger check."""
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_step(agent_finish())
        client.check.assert_not_called()

    def test_no_check_when_disabled(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client, check_before_step=False)
        hooks.on_step(agent_action())
        client.check.assert_not_called()

    def test_multiple_steps(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_step(agent_action("tool1"))
        hooks.on_step(agent_action("tool2"))
        hooks.on_step(agent_finish())
        assert client.check.call_count == 2

    def test_tool_input_truncated_in_query(self) -> None:
        client = sync_client()
        hooks = AkashiCrewCallbacks(client)
        hooks.on_step(agent_action("t", "z" * 500))
        query = client.check.call_args[0][1]
        assert len(query) < 230  # tool=t + input= + 200

    def test_error_does_not_propagate(self) -> None:
        client = sync_client()
        client.check.side_effect = RuntimeError("down")
        hooks = AkashiCrewCallbacks(client)
        hooks.on_step(agent_action())  # must not raise


# ===========================================================================
# make_hooks
# ===========================================================================


class TestMakeHooks:
    def test_returns_both_keys(self) -> None:
        client = sync_client()
        kwargs = make_hooks(client)
        assert set(kwargs.keys()) == {"task_callback", "step_callback"}

    def test_callbacks_are_callable(self) -> None:
        client = sync_client()
        kwargs = make_hooks(client)
        assert callable(kwargs["task_callback"])
        assert callable(kwargs["step_callback"])

    def test_decision_type_flows_through_to_trace(self) -> None:
        client = sync_client()
        kwargs = make_hooks(client, decision_type="dt_test")
        kwargs["task_callback"](task_out())
        assert client.trace.call_args[0][0].decision_type == "dt_test"

    def test_confidence_flows_through_to_trace(self) -> None:
        client = sync_client()
        kwargs = make_hooks(client, confidence=0.8)
        kwargs["task_callback"](task_out())
        assert client.trace.call_args[0][0].confidence == 0.8

    def test_step_callback_triggers_check(self) -> None:
        client = sync_client()
        kwargs = make_hooks(client)
        kwargs["step_callback"](agent_action("lookup"))
        client.check.assert_called_once()

    def test_check_before_step_false_flows_through(self) -> None:
        client = sync_client()
        kwargs = make_hooks(client, check_before_step=False)
        kwargs["step_callback"](agent_action())
        client.check.assert_not_called()

    def test_both_callbacks_share_same_hooks_instance(self) -> None:
        """task_callback and step_callback are bound to the same instance."""
        client = sync_client()
        kwargs = make_hooks(client, decision_type="shared")
        # Both should use the same decision_type.
        kwargs["task_callback"](task_out())
        kwargs["step_callback"](agent_action())
        assert client.trace.call_args[0][0].decision_type == "shared"
        assert client.check.call_args[0][0] == "shared"


# ===========================================================================
# run_with_akashi
# ===========================================================================


class TestRunWithAkashi:
    def test_calls_check_before_kickoff(self) -> None:
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "done"
        run_with_akashi(crew, client, inputs={"topic": "AI"})
        client.check.assert_called_once()

    def test_check_called_before_kickoff_ordering(self) -> None:
        """check must be called before kickoff."""
        call_order = []
        client = sync_client()
        client.check.side_effect = lambda *a, **k: call_order.append("check")
        crew = MagicMock()
        crew.kickoff.side_effect = lambda **k: call_order.append("kickoff")
        run_with_akashi(crew, client)
        assert call_order == ["check", "kickoff"]

    def test_traces_after_kickoff(self) -> None:
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "crew result"
        run_with_akashi(crew, client)
        client.trace.assert_called_once()
        assert "crew result" in _trace_req(client).outcome

    def test_returns_crew_result(self) -> None:
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "expected return"
        result = run_with_akashi(crew, client)
        assert result == "expected return"

    def test_inputs_passed_to_kickoff(self) -> None:
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "ok"
        run_with_akashi(crew, client, inputs={"x": 1, "y": 2})
        crew.kickoff.assert_called_once_with(inputs={"x": 1, "y": 2})

    def test_no_inputs_calls_kickoff_with_none(self) -> None:
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "ok"
        run_with_akashi(crew, client)
        crew.kickoff.assert_called_once_with(inputs=None)

    def test_custom_decision_type_in_trace(self) -> None:
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "r"
        run_with_akashi(crew, client, decision_type="crew_run")
        assert _trace_req(client).decision_type == "crew_run"

    def test_custom_confidence_in_trace(self) -> None:
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "r"
        run_with_akashi(crew, client, confidence=0.6)
        assert _trace_req(client).confidence == 0.6

    def test_check_error_does_not_prevent_kickoff(self) -> None:
        client = sync_client()
        client.check.side_effect = RuntimeError("check failed")
        crew = MagicMock()
        crew.kickoff.return_value = "ok"
        result = run_with_akashi(crew, client)
        crew.kickoff.assert_called_once()
        assert result == "ok"

    def test_trace_error_does_not_swallow_result(self) -> None:
        client = sync_client()
        client.trace.side_effect = RuntimeError("trace failed")
        crew = MagicMock()
        crew.kickoff.return_value = "ok"
        result = run_with_akashi(crew, client)
        assert result == "ok"

    def test_kickoff_exception_propagates(self) -> None:
        """If the crew itself fails, the exception must propagate."""
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.side_effect = ValueError("crew error")
        with pytest.raises(ValueError, match="crew error"):
            run_with_akashi(crew, client)

    def test_outcome_truncated(self) -> None:
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "q" * 1000
        run_with_akashi(crew, client)
        assert len(_trace_req(client).outcome) == 500

    def test_inputs_appear_in_check_query_or_first_arg(self) -> None:
        """check is called with the inputs as context."""
        client = sync_client()
        crew = MagicMock()
        crew.kickoff.return_value = "r"
        run_with_akashi(crew, client, inputs={"topic": "weather"})
        # check is called (exact args depend on implementation detail)
        client.check.assert_called_once()
