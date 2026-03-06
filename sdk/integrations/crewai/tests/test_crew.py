"""Comprehensive tests for the AkashiCrew proxy."""

from __future__ import annotations

from unittest.mock import MagicMock

import pytest

from akashi import AkashiSyncClient
from akashi.types import TraceRequest
from akashi_crewai import AkashiCrew
from akashi_crewai._crew import _chain_callback


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


def mock_crew(
    kickoff_return: str = "crew result",
    task_callback: object = None,
    step_callback: object = None,
) -> MagicMock:
    crew = MagicMock()
    crew.kickoff.return_value = kickoff_return
    crew.task_callback = task_callback
    crew.step_callback = step_callback
    return crew


def _trace_req(client: MagicMock) -> TraceRequest:
    return client.trace.call_args[0][0]


# ===========================================================================
# AkashiCrew — kickoff wrapping
# ===========================================================================


class TestAkashiCrewKickoff:
    def test_calls_check_before_kickoff(self) -> None:
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client)
        traced.kickoff(inputs={"topic": "AI"})
        client.check.assert_called_once()

    def test_check_called_before_kickoff_ordering(self) -> None:
        """check must be called before kickoff."""
        call_order: list[str] = []
        client = sync_client()
        client.check.side_effect = lambda *a, **k: call_order.append("check")
        crew = mock_crew()
        crew.kickoff.side_effect = lambda **k: call_order.append("kickoff")
        traced = AkashiCrew(crew, client)
        traced.kickoff()
        assert call_order == ["check", "kickoff"]

    def test_traces_after_kickoff(self) -> None:
        client = sync_client()
        crew = mock_crew(kickoff_return="crew result")
        traced = AkashiCrew(crew, client)
        traced.kickoff()
        client.trace.assert_called_once()
        assert "crew result" in _trace_req(client).outcome

    def test_returns_crew_result(self) -> None:
        client = sync_client()
        crew = mock_crew(kickoff_return="expected return")
        traced = AkashiCrew(crew, client)
        result = traced.kickoff()
        assert result == "expected return"

    def test_inputs_passed_to_kickoff(self) -> None:
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client)
        traced.kickoff(inputs={"x": 1, "y": 2})
        crew.kickoff.assert_called_once_with(inputs={"x": 1, "y": 2})

    def test_no_inputs_calls_kickoff_with_none(self) -> None:
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client)
        traced.kickoff()
        crew.kickoff.assert_called_once_with(inputs=None)

    def test_custom_decision_type(self) -> None:
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client, decision_type="my_type")
        traced.kickoff()
        assert _trace_req(client).decision_type == "my_type"

    def test_custom_confidence(self) -> None:
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client, confidence=0.9)
        traced.kickoff()
        assert _trace_req(client).confidence == 0.9

    def test_default_decision_type(self) -> None:
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client)
        traced.kickoff()
        assert _trace_req(client).decision_type == "crew_task"

    def test_default_confidence(self) -> None:
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client)
        traced.kickoff()
        assert _trace_req(client).confidence == 0.7

    def test_check_error_does_not_prevent_kickoff(self) -> None:
        client = sync_client()
        client.check.side_effect = RuntimeError("check failed")
        crew = mock_crew(kickoff_return="ok")
        traced = AkashiCrew(crew, client)
        result = traced.kickoff()
        crew.kickoff.assert_called_once()
        assert result == "ok"

    def test_trace_error_does_not_swallow_result(self) -> None:
        client = sync_client()
        client.trace.side_effect = RuntimeError("trace failed")
        crew = mock_crew(kickoff_return="ok")
        traced = AkashiCrew(crew, client)
        result = traced.kickoff()
        assert result == "ok"

    def test_kickoff_exception_propagates(self) -> None:
        """If the crew itself fails, the exception must propagate."""
        client = sync_client()
        crew = mock_crew()
        crew.kickoff.side_effect = ValueError("crew error")
        traced = AkashiCrew(crew, client)
        with pytest.raises(ValueError, match="crew error"):
            traced.kickoff()

    def test_outcome_truncated(self) -> None:
        client = sync_client()
        crew = mock_crew(kickoff_return="q" * 1000)
        traced = AkashiCrew(crew, client)
        traced.kickoff()
        assert len(_trace_req(client).outcome) == 500

    def test_inputs_in_check_query(self) -> None:
        """check is called with the inputs as context."""
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client)
        traced.kickoff(inputs={"topic": "weather"})
        query = client.check.call_args[0][1]
        assert "weather" in query

    def test_inputs_in_trace_metadata(self) -> None:
        client = sync_client()
        crew = mock_crew()
        traced = AkashiCrew(crew, client)
        traced.kickoff(inputs={"topic": "weather"})
        metadata = _trace_req(client).metadata
        assert "weather" in metadata["inputs"]


# ===========================================================================
# AkashiCrew — callback chaining
# ===========================================================================


class TestAkashiCrewCallbackChaining:
    def test_installs_task_callback_when_none(self) -> None:
        client = sync_client()
        crew = mock_crew(task_callback=None)
        AkashiCrew(crew, client)
        assert crew.task_callback is not None
        # Trigger the callback to verify it traces
        crew.task_callback(task_out())
        client.trace.assert_called_once()

    def test_installs_step_callback_when_none(self) -> None:
        client = sync_client()
        crew = mock_crew(step_callback=None)
        AkashiCrew(crew, client)
        assert crew.step_callback is not None
        crew.step_callback(agent_action())
        client.check.assert_called_once()

    def test_chains_existing_task_callback(self) -> None:
        """User's task_callback is called, then Akashi traces."""
        user_cb = MagicMock()
        client = sync_client()
        crew = mock_crew(task_callback=user_cb)
        AkashiCrew(crew, client)
        out = task_out()
        crew.task_callback(out)
        user_cb.assert_called_once_with(out)
        client.trace.assert_called_once()

    def test_chains_existing_step_callback(self) -> None:
        """User's step_callback is called, then Akashi checks."""
        user_cb = MagicMock()
        client = sync_client()
        crew = mock_crew(step_callback=user_cb)
        AkashiCrew(crew, client)
        action = agent_action()
        crew.step_callback(action)
        user_cb.assert_called_once_with(action)
        client.check.assert_called_once()

    def test_user_task_callback_called_first(self) -> None:
        call_order: list[str] = []
        user_cb = MagicMock(side_effect=lambda _: call_order.append("user"))
        client = sync_client()
        client.trace.side_effect = lambda *a, **k: call_order.append("akashi")
        crew = mock_crew(task_callback=user_cb)
        AkashiCrew(crew, client)
        crew.task_callback(task_out())
        assert call_order == ["user", "akashi"]

    def test_user_step_callback_called_first(self) -> None:
        call_order: list[str] = []
        user_cb = MagicMock(side_effect=lambda _: call_order.append("user"))
        client = sync_client()
        client.check.side_effect = lambda *a, **k: call_order.append("akashi")
        crew = mock_crew(step_callback=user_cb)
        AkashiCrew(crew, client)
        crew.step_callback(agent_action())
        assert call_order == ["user", "akashi"]

    def test_user_task_callback_exception_propagates(self) -> None:
        """User's callback fails — exception propagates, Akashi callback skipped."""
        user_cb = MagicMock(side_effect=ValueError("user error"))
        client = sync_client()
        crew = mock_crew(task_callback=user_cb)
        AkashiCrew(crew, client)
        with pytest.raises(ValueError, match="user error"):
            crew.task_callback(task_out())
        # Akashi trace should NOT have been called
        client.trace.assert_not_called()

    def test_akashi_task_callback_error_does_not_propagate(self) -> None:
        """Akashi callback fails in a chained context — no exception visible."""
        user_cb = MagicMock()
        client = sync_client()
        client.trace.side_effect = RuntimeError("akashi down")
        crew = mock_crew(task_callback=user_cb)
        AkashiCrew(crew, client)
        crew.task_callback(task_out())  # must not raise
        user_cb.assert_called_once()

    def test_akashi_step_callback_error_does_not_propagate(self) -> None:
        user_cb = MagicMock()
        client = sync_client()
        client.check.side_effect = RuntimeError("akashi down")
        crew = mock_crew(step_callback=user_cb)
        AkashiCrew(crew, client)
        crew.step_callback(agent_action())  # must not raise
        user_cb.assert_called_once()

    def test_both_callbacks_fire_during_kickoff(self) -> None:
        """Wire a mock crew that invokes its callbacks during kickoff."""
        client = sync_client()
        crew = mock_crew(kickoff_return="done")

        def fake_kickoff(**kwargs: object) -> str:
            # Simulate CrewAI calling the callbacks during execution
            crew.task_callback(task_out())
            crew.step_callback(agent_action())
            return "done"

        crew.kickoff.side_effect = fake_kickoff
        traced = AkashiCrew(crew, client)
        traced.kickoff()
        # task_callback → trace (per-task) + trace (crew-level) = 2 traces
        assert client.trace.call_count == 2
        # step_callback → check (per-step) + check (crew-level) = 2 checks
        assert client.check.call_count == 2


# ===========================================================================
# AkashiCrew — __getattr__ delegation
# ===========================================================================


class TestAkashiCrewDelegation:
    def test_delegates_agents_attribute(self) -> None:
        crew = mock_crew()
        crew.agents = ["agent1", "agent2"]
        traced = AkashiCrew(crew, sync_client())
        assert traced.agents == ["agent1", "agent2"]

    def test_delegates_tasks_attribute(self) -> None:
        crew = mock_crew()
        crew.tasks = ["task1"]
        traced = AkashiCrew(crew, sync_client())
        assert traced.tasks == ["task1"]

    def test_delegates_arbitrary_method(self) -> None:
        crew = mock_crew()
        crew.some_method.return_value = 42
        traced = AkashiCrew(crew, sync_client())
        assert traced.some_method() == 42

    def test_kickoff_is_not_delegated(self) -> None:
        """traced.kickoff should be AkashiCrew.kickoff, not crew.kickoff."""
        crew = mock_crew()
        traced = AkashiCrew(crew, sync_client())
        assert traced.kickoff.__func__ is AkashiCrew.kickoff

    def test_attribute_error_from_crew_propagates(self) -> None:
        crew = MagicMock(spec=[])  # no attributes
        crew.task_callback = None
        crew.step_callback = None
        traced = AkashiCrew(crew, sync_client())
        with pytest.raises(AttributeError):
            _ = traced.nonexistent_attr

    def test_kickoff_for_each_passes_through(self) -> None:
        """kickoff_for_each should delegate to crew unchanged (no check/trace wrapping)."""
        crew = mock_crew()
        crew.kickoff_for_each.return_value = ["r1", "r2"]
        traced = AkashiCrew(crew, sync_client())
        result = traced.kickoff_for_each(inputs=[{"a": 1}, {"b": 2}])
        crew.kickoff_for_each.assert_called_once_with(inputs=[{"a": 1}, {"b": 2}])
        assert result == ["r1", "r2"]


# ===========================================================================
# AkashiCrew — constructor options
# ===========================================================================


class TestAkashiCrewOptions:
    def test_check_before_step_false(self) -> None:
        client = sync_client()
        crew = mock_crew()
        AkashiCrew(crew, client, check_before_step=False)
        crew.step_callback(agent_action())
        client.check.assert_not_called()

    def test_trace_task_output_false(self) -> None:
        client = sync_client()
        crew = mock_crew()
        AkashiCrew(crew, client, trace_task_output=False)
        crew.task_callback(task_out())
        client.trace.assert_not_called()


# ===========================================================================
# _chain_callback — unit tests
# ===========================================================================


class TestChainCallback:
    def test_none_existing_returns_akashi_cb_directly(self) -> None:
        akashi_cb = MagicMock()
        result = _chain_callback(None, akashi_cb)
        assert result is akashi_cb

    def test_both_called_in_order(self) -> None:
        call_order: list[str] = []
        existing = MagicMock(side_effect=lambda _: call_order.append("existing"))
        akashi_cb = MagicMock(side_effect=lambda _: call_order.append("akashi"))
        chained = _chain_callback(existing, akashi_cb)
        chained("output")
        assert call_order == ["existing", "akashi"]

    def test_akashi_exception_swallowed(self) -> None:
        existing = MagicMock()
        akashi_cb = MagicMock(side_effect=RuntimeError("boom"))
        chained = _chain_callback(existing, akashi_cb)
        chained("output")  # must not raise
        existing.assert_called_once()

    def test_existing_exception_propagates(self) -> None:
        existing = MagicMock(side_effect=ValueError("user error"))
        akashi_cb = MagicMock()
        chained = _chain_callback(existing, akashi_cb)
        with pytest.raises(ValueError, match="user error"):
            chained("output")
        akashi_cb.assert_not_called()
