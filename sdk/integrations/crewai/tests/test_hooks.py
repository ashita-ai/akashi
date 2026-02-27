"""Tests for AkashiCrewCallbacks, make_hooks, and run_with_akashi."""

from __future__ import annotations

from unittest.mock import MagicMock

import pytest

from akashi import AkashiSyncClient
from akashi.types import TraceRequest
from akashi_crewai import AkashiCrewCallbacks, make_hooks, run_with_akashi


def _make_client() -> MagicMock:
    client = MagicMock(spec=AkashiSyncClient)
    client.trace.return_value = MagicMock()
    client.check.return_value = MagicMock()
    return client


def _task_output(raw: str = "Task completed successfully", agent: str = "researcher", description: str = "Find info") -> MagicMock:
    out = MagicMock()
    out.raw = raw
    out.agent = agent
    out.description = description
    return out


def _agent_action(tool: str = "search", tool_input: str = "query") -> MagicMock:
    action = MagicMock()
    action.tool = tool
    action.tool_input = tool_input
    action.log = "Using search"
    return action


def _agent_finish(output: str = "Final answer") -> MagicMock:
    finish = MagicMock()
    # AgentFinish has no .tool attribute
    del finish.tool  # ensure it's not present
    finish.return_values = {"output": output}
    finish.log = "Done"
    return finish


# ---------------------------------------------------------------------------
# AkashiCrewCallbacks
# ---------------------------------------------------------------------------


class TestAkashiCrewCallbacks:
    def test_traces_task_output(self) -> None:
        client = _make_client()
        hooks = AkashiCrewCallbacks(client, decision_type="test_task")

        hooks.on_task_complete(_task_output("Research findings: AI is growing"))

        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert "Research findings" in req.outcome
        assert req.decision_type == "test_task"
        assert req.metadata["agent"] == "researcher"

    def test_reasoning_from_task_description(self) -> None:
        client = _make_client()
        hooks = AkashiCrewCallbacks(client)

        hooks.on_task_complete(_task_output(description="Summarize the article"))

        req: TraceRequest = client.trace.call_args[0][0]
        assert req.reasoning == "Summarize the article"

    def test_checks_on_agent_action_step(self) -> None:
        client = _make_client()
        hooks = AkashiCrewCallbacks(client, check_before_step=True)

        hooks.on_step(_agent_action("web_search", "AI news 2026"))

        client.check.assert_called_once()
        query = client.check.call_args[0][1]
        assert "web_search" in query

    def test_no_check_on_agent_finish_step(self) -> None:
        client = _make_client()
        hooks = AkashiCrewCallbacks(client)

        # AgentFinish has no .tool — should not trigger check.
        finish = MagicMock(spec=[])  # no attributes
        hooks.on_step(finish)

        client.check.assert_not_called()

    def test_no_check_when_disabled(self) -> None:
        client = _make_client()
        hooks = AkashiCrewCallbacks(client, check_before_step=False)

        hooks.on_step(_agent_action())

        client.check.assert_not_called()

    def test_no_trace_when_disabled(self) -> None:
        client = _make_client()
        hooks = AkashiCrewCallbacks(client, trace_task_output=False)

        hooks.on_task_complete(_task_output())

        client.trace.assert_not_called()

    def test_error_on_trace_does_not_propagate(self) -> None:
        client = _make_client()
        client.trace.side_effect = ConnectionError("akashi is down")
        hooks = AkashiCrewCallbacks(client)

        # Must not raise.
        hooks.on_task_complete(_task_output())

    def test_error_on_check_does_not_propagate(self) -> None:
        client = _make_client()
        client.check.side_effect = ConnectionError("akashi is down")
        hooks = AkashiCrewCallbacks(client)

        # Must not raise.
        hooks.on_step(_agent_action())

    def test_outcome_truncated_to_500(self) -> None:
        client = _make_client()
        hooks = AkashiCrewCallbacks(client)

        hooks.on_task_complete(_task_output(raw="x" * 1000))

        req: TraceRequest = client.trace.call_args[0][0]
        assert len(req.outcome) == 500

    def test_custom_confidence(self) -> None:
        client = _make_client()
        hooks = AkashiCrewCallbacks(client, confidence=0.85)

        hooks.on_task_complete(_task_output())

        req: TraceRequest = client.trace.call_args[0][0]
        assert req.confidence == 0.85


# ---------------------------------------------------------------------------
# make_hooks
# ---------------------------------------------------------------------------


class TestMakeHooks:
    def test_returns_task_and_step_callback_keys(self) -> None:
        client = _make_client()
        kwargs = make_hooks(client)

        assert "task_callback" in kwargs
        assert "step_callback" in kwargs
        assert callable(kwargs["task_callback"])
        assert callable(kwargs["step_callback"])

    def test_callbacks_are_bound_to_same_hooks_instance(self) -> None:
        client = _make_client()
        kwargs = make_hooks(client, decision_type="bound_test")

        kwargs["task_callback"](_task_output("output"))

        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert req.decision_type == "bound_test"


# ---------------------------------------------------------------------------
# run_with_akashi
# ---------------------------------------------------------------------------


class TestRunWithAkashi:
    def test_calls_check_before_kickoff(self) -> None:
        client = _make_client()
        crew = MagicMock()
        crew.kickoff.return_value = "Crew output"

        run_with_akashi(crew, client, inputs={"topic": "AI"})

        client.check.assert_called_once()
        crew.kickoff.assert_called_once_with(inputs={"topic": "AI"})

    def test_traces_crew_output(self) -> None:
        client = _make_client()
        crew = MagicMock()
        crew.kickoff.return_value = "Final crew output"

        run_with_akashi(crew, client, decision_type="crew_run")

        client.trace.assert_called_once()
        req: TraceRequest = client.trace.call_args[0][0]
        assert "Final crew output" in req.outcome
        assert req.decision_type == "crew_run"

    def test_returns_crew_result(self) -> None:
        client = _make_client()
        crew = MagicMock()
        crew.kickoff.return_value = "expected"

        result = run_with_akashi(crew, client)

        assert result == "expected"

    def test_check_error_does_not_prevent_kickoff(self) -> None:
        client = _make_client()
        client.check.side_effect = RuntimeError("check failed")
        crew = MagicMock()
        crew.kickoff.return_value = "ok"

        result = run_with_akashi(crew, client)

        crew.kickoff.assert_called_once()
        assert result == "ok"

    def test_trace_error_does_not_swallow_result(self) -> None:
        client = _make_client()
        client.trace.side_effect = RuntimeError("trace failed")
        crew = MagicMock()
        crew.kickoff.return_value = "ok"

        result = run_with_akashi(crew, client)

        assert result == "ok"
