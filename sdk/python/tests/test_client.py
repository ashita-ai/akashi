"""Tests for the Akashi Python SDK client."""

from __future__ import annotations

import json
import uuid
from datetime import datetime, timezone

import httpx
import pytest
import respx

from akashi.client import AkashiClient, AkashiSyncClient, _handle_response, _USER_AGENT
from akashi.exceptions import (
    AuthenticationError,
    AuthorizationError,
    NotFoundError,
    RateLimitError,
    ServerError,
    ValidationError,
)
from akashi.middleware import AkashiSyncMiddleware
from akashi.types import (
    Agent,
    AgentRun,
    CheckResponse,
    CreateAgentRequest,
    CreateGrantRequest,
    DecisionConflict,
    EventInput,
    Grant,
    HealthResponse,
    QueryFilters,
    QueryResponse,
    SearchResponse,
    TraceRequest,
    TraceResponse,
)


BASE_URL = "https://akashi.test"
NOW = datetime.now(tz=timezone.utc).isoformat()
DECISION_ID = str(uuid.uuid4())
RUN_ID = str(uuid.uuid4())
ORG_ID = str(uuid.uuid4())
AGENT_UUID = str(uuid.uuid4())
GRANT_ID = str(uuid.uuid4())


def _mock_auth(router: respx.Router) -> None:
    """Register the auth endpoint on the respx router."""
    router.post(f"{BASE_URL}/auth/token").respond(
        200,
        json={
            "data": {
                "token": "test-token",
                "expires_at": "2099-01-01T00:00:00Z",
            }
        },
    )


def _make_client() -> AkashiSyncClient:
    return AkashiSyncClient(
        base_url=BASE_URL,
        agent_id="test-agent",
        api_key="test-key",
        timeout=5.0,
    )


def _make_async_client() -> AkashiClient:
    return AkashiClient(
        base_url=BASE_URL,
        agent_id="test-agent",
        api_key="test-key",
        timeout=5.0,
    )


def _decision_json(decision_id: str | None = None, run_id: str | None = None) -> dict:
    """Return a minimal valid Decision JSON payload."""
    return {
        "id": decision_id or DECISION_ID,
        "run_id": run_id or RUN_ID,
        "agent_id": "test-agent",
        "org_id": ORG_ID,
        "decision_type": "architecture",
        "outcome": "chose event sourcing",
        "confidence": 0.9,
        "metadata": {},
        "valid_from": NOW,
        "transaction_time": NOW,
        "created_at": NOW,
    }


def _run_json(run_id: str | None = None) -> dict:
    """Return a minimal valid AgentRun JSON payload."""
    return {
        "id": run_id or RUN_ID,
        "agent_id": "test-agent",
        "org_id": ORG_ID,
        "status": "running",
        "metadata": {},
        "started_at": NOW,
        "created_at": NOW,
    }


def _agent_json(agent_id: str = "coder") -> dict:
    """Return a minimal valid Agent JSON payload."""
    return {
        "id": AGENT_UUID,
        "agent_id": agent_id,
        "org_id": ORG_ID,
        "name": "Coder Agent",
        "role": "agent",
        "metadata": {},
        "created_at": NOW,
        "updated_at": NOW,
    }


def _grant_json() -> dict:
    """Return a minimal valid Grant JSON payload."""
    return {
        "id": GRANT_ID,
        "grantor_id": AGENT_UUID,
        "grantee_id": str(uuid.uuid4()),
        "resource_type": "agent_traces",
        "permission": "read",
        "granted_at": NOW,
    }


def _conflict_json() -> dict:
    """Return a minimal valid DecisionConflict JSON payload."""
    return {
        "conflict_kind": "cross_agent",
        "decision_a_id": str(uuid.uuid4()),
        "decision_b_id": str(uuid.uuid4()),
        "org_id": ORG_ID,
        "agent_a": "planner",
        "agent_b": "coder",
        "run_a": str(uuid.uuid4()),
        "run_b": str(uuid.uuid4()),
        "decision_type": "architecture",
        "outcome_a": "monolith",
        "outcome_b": "microservices",
        "confidence_a": 0.8,
        "confidence_b": 0.9,
        "decided_at_a": NOW,
        "decided_at_b": NOW,
        "detected_at": NOW,
    }


class TestCheck:
    @respx.mock
    def test_check_returns_check_response(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/check").respond(
            200,
            json={
                "data": {
                    "has_precedent": True,
                    "decisions": [
                        {
                            "id": DECISION_ID,
                            "run_id": RUN_ID,
                            "agent_id": "other-agent",
                            "org_id": ORG_ID,
                            "decision_type": "deployment",
                            "outcome": "approved",
                            "confidence": 0.95,
                            "metadata": {},
                            "valid_from": NOW,
                            "transaction_time": NOW,
                            "created_at": NOW,
                        }
                    ],
                    "conflicts": [],
                }
            },
        )

        with _make_client() as client:
            resp = client.check("deployment")

        assert isinstance(resp, CheckResponse)
        assert resp.has_precedent is True
        assert len(resp.decisions) == 1
        assert resp.decisions[0].outcome == "approved"
        assert str(resp.decisions[0].id) == DECISION_ID


class TestTrace:
    @respx.mock
    def test_trace_returns_trace_response(self) -> None:
        _mock_auth(respx)

        def _check_trace_body(request: httpx.Request) -> httpx.Response:
            body = request.content.decode()
            data = json.loads(body)
            assert data["agent_id"] == "test-agent"
            assert data["decision"]["decision_type"] == "model_selection"
            assert data["decision"]["outcome"] == "chose GPT-4"
            return httpx.Response(
                201,
                json={
                    "data": {
                        "run_id": RUN_ID,
                        "decision_id": DECISION_ID,
                        "event_count": 3,
                    }
                },
            )

        respx.post(f"{BASE_URL}/v1/trace").mock(side_effect=_check_trace_body)

        with _make_client() as client:
            resp = client.trace(
                TraceRequest(
                    decision_type="model_selection",
                    outcome="chose GPT-4",
                    confidence=0.9,
                    reasoning="high confidence from prior data",
                )
            )

        assert isinstance(resp, TraceResponse)
        assert str(resp.run_id) == RUN_ID
        assert str(resp.decision_id) == DECISION_ID
        assert resp.event_count == 3

    @respx.mock
    def test_trace_sends_user_agent_and_session_headers(self) -> None:
        _mock_auth(respx)

        captured_headers: dict[str, str] = {}

        def _capture_headers(request: httpx.Request) -> httpx.Response:
            captured_headers["user-agent"] = request.headers.get("user-agent", "")
            captured_headers["x-akashi-session"] = request.headers.get("x-akashi-session", "")
            return httpx.Response(
                201,
                json={
                    "data": {
                        "run_id": RUN_ID,
                        "decision_id": DECISION_ID,
                        "event_count": 1,
                    }
                },
            )

        respx.post(f"{BASE_URL}/v1/trace").mock(side_effect=_capture_headers)

        with _make_client() as client:
            client.trace(
                TraceRequest(
                    decision_type="test",
                    outcome="pass",
                    confidence=1.0,
                )
            )

        assert captured_headers["user-agent"] == _USER_AGENT
        # Session ID should be a valid UUID.
        session_str = captured_headers["x-akashi-session"]
        assert session_str != ""
        uuid.UUID(session_str)  # raises if invalid

    @respx.mock
    def test_trace_with_context(self) -> None:
        _mock_auth(respx)

        captured_body: dict = {}

        def _capture_body(request: httpx.Request) -> httpx.Response:
            captured_body.update(json.loads(request.content.decode()))
            return httpx.Response(
                201,
                json={
                    "data": {
                        "run_id": RUN_ID,
                        "decision_id": DECISION_ID,
                        "event_count": 1,
                    }
                },
            )

        respx.post(f"{BASE_URL}/v1/trace").mock(side_effect=_capture_body)

        with _make_client() as client:
            client.trace(
                TraceRequest(
                    decision_type="model_selection",
                    outcome="gpt-4o",
                    confidence=0.9,
                    context={"model": "gpt-4o", "task": "summarization", "repo": "example/repo"},
                )
            )

        assert "context" in captured_body
        assert captured_body["context"]["model"] == "gpt-4o"
        assert captured_body["context"]["task"] == "summarization"
        assert captured_body["context"]["repo"] == "example/repo"

    @respx.mock
    def test_session_id_override(self) -> None:
        _mock_auth(respx)

        fixed_session = uuid.UUID("11111111-1111-1111-1111-111111111111")
        captured_session: str = ""

        def _capture_session(request: httpx.Request) -> httpx.Response:
            nonlocal captured_session
            captured_session = request.headers.get("x-akashi-session", "")
            return httpx.Response(
                201,
                json={
                    "data": {
                        "run_id": RUN_ID,
                        "decision_id": DECISION_ID,
                        "event_count": 1,
                    }
                },
            )

        respx.post(f"{BASE_URL}/v1/trace").mock(side_effect=_capture_session)

        with AkashiSyncClient(
            base_url=BASE_URL,
            agent_id="test-agent",
            api_key="test-key",
            session_id=fixed_session,
        ) as client:
            client.trace(
                TraceRequest(
                    decision_type="test",
                    outcome="pass",
                    confidence=1.0,
                )
            )

        assert captured_session == str(fixed_session)

    @respx.mock
    def test_session_id_consistent_across_traces(self) -> None:
        _mock_auth(respx)

        captured_sessions: list[str] = []

        def _capture_session(request: httpx.Request) -> httpx.Response:
            captured_sessions.append(request.headers.get("x-akashi-session", ""))
            return httpx.Response(
                201,
                json={
                    "data": {
                        "run_id": RUN_ID,
                        "decision_id": DECISION_ID,
                        "event_count": 1,
                    }
                },
            )

        respx.post(f"{BASE_URL}/v1/trace").mock(side_effect=_capture_session)

        with _make_client() as client:
            for _ in range(3):
                client.trace(
                    TraceRequest(
                        decision_type="test",
                        outcome="pass",
                        confidence=1.0,
                    )
                )

        assert len(captured_sessions) == 3
        assert captured_sessions[0] == captured_sessions[1] == captured_sessions[2]
        uuid.UUID(captured_sessions[0])  # valid UUID


class TestQuery:
    @respx.mock
    def test_query_returns_query_response(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/query").respond(
            200,
            json={
                "data": {
                    "decisions": [_decision_json()],
                    "total": 1,
                    "limit": 50,
                    "offset": 0,
                }
            },
        )

        with _make_client() as client:
            resp = client.query()

        assert isinstance(resp, QueryResponse)
        assert resp.total == 1
        assert len(resp.decisions) == 1
        assert resp.decisions[0].decision_type == "architecture"

    @respx.mock
    def test_query_with_filters(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/query").respond(
            200,
            json={
                "data": {
                    "decisions": [],
                    "total": 0,
                    "limit": 10,
                    "offset": 0,
                }
            },
        )

        with _make_client() as client:
            resp = client.query(
                QueryFilters(decision_type="architecture"),
                limit=10,
                offset=0,
            )

        assert isinstance(resp, QueryResponse)
        assert resp.total == 0


class TestSearch:
    @respx.mock
    def test_search_returns_search_response(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/search").respond(
            200,
            json={
                "data": {
                    "results": [
                        {
                            "decision": _decision_json(),
                            "similarity_score": 0.92,
                        }
                    ],
                    "total": 1,
                }
            },
        )

        with _make_client() as client:
            resp = client.search("event sourcing")

        assert isinstance(resp, SearchResponse)
        assert resp.total == 1
        assert resp.results[0].similarity_score == 0.92


class TestRecent:
    @respx.mock
    def test_recent_returns_decisions(self) -> None:
        _mock_auth(respx)
        respx.get(f"{BASE_URL}/v1/decisions/recent").respond(
            200,
            json={
                "data": {
                    "decisions": [_decision_json()],
                    "total": 1,
                    "count": 1,
                    "limit": 10,
                }
            },
        )

        with _make_client() as client:
            decisions = client.recent(limit=10)

        assert len(decisions) == 1
        assert decisions[0].outcome == "chose event sourcing"


class TestCreateRun:
    @respx.mock
    def test_create_run_returns_agent_run(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/runs").respond(
            201, json={"data": _run_json()}
        )

        with _make_client() as client:
            run = client.create_run(trace_id="my-trace")

        assert isinstance(run, AgentRun)
        assert str(run.id) == RUN_ID
        assert run.status == "running"


class TestAppendEvents:
    @respx.mock
    def test_append_events_succeeds(self) -> None:
        _mock_auth(respx)
        run_id = uuid.UUID(RUN_ID)
        respx.post(f"{BASE_URL}/v1/runs/{run_id}/events").respond(
            202,
            json={
                "data": {
                    "accepted": 1,
                    "event_ids": [str(uuid.uuid4())],
                }
            },
        )

        with _make_client() as client:
            # Should not raise.
            client.append_events(
                run_id,
                [EventInput(event_type="DecisionMade", payload={"key": "value"})],
            )


class TestCompleteRun:
    @respx.mock
    def test_complete_run_returns_updated_run(self) -> None:
        _mock_auth(respx)
        run_id = uuid.UUID(RUN_ID)
        completed_run = _run_json()
        completed_run["status"] = "completed"
        completed_run["completed_at"] = NOW
        respx.post(f"{BASE_URL}/v1/runs/{run_id}/complete").respond(
            200, json={"data": completed_run}
        )

        with _make_client() as client:
            run = client.complete_run(run_id, "completed")

        assert isinstance(run, AgentRun)
        assert run.status == "completed"


class TestGetRun:
    @respx.mock
    def test_get_run_returns_dict(self) -> None:
        _mock_auth(respx)
        run_id = uuid.UUID(RUN_ID)
        respx.get(f"{BASE_URL}/v1/runs/{run_id}").respond(
            200,
            json={
                "data": {
                    "run": _run_json(),
                    "events": [],
                    "decisions": [],
                }
            },
        )

        with _make_client() as client:
            data = client.get_run(run_id)

        assert "run" in data
        assert data["run"]["status"] == "running"


class TestCreateAgent:
    @respx.mock
    def test_create_agent_returns_agent(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/agents").respond(
            201, json={"data": _agent_json("new-agent")}
        )

        with _make_client() as client:
            agent = client.create_agent(
                CreateAgentRequest(
                    agent_id="new-agent",
                    name="New Agent",
                    role="agent",
                    api_key="secret-key",
                )
            )

        assert isinstance(agent, Agent)
        assert agent.agent_id == "new-agent"


class TestListAgents:
    @respx.mock
    def test_list_agents_returns_list(self) -> None:
        _mock_auth(respx)
        # Server wraps the array in data envelope.
        respx.get(f"{BASE_URL}/v1/agents").respond(
            200,
            json={
                "data": [_agent_json("agent-a"), _agent_json("agent-b")]
            },
        )

        with _make_client() as client:
            agents = client.list_agents()

        assert len(agents) == 2
        assert all(isinstance(a, Agent) for a in agents)


class TestDeleteAgent:
    @respx.mock
    def test_delete_agent_succeeds(self) -> None:
        _mock_auth(respx)
        respx.delete(f"{BASE_URL}/v1/agents/old-agent").respond(204)

        with _make_client() as client:
            # Should not raise.
            client.delete_agent("old-agent")


class TestTemporalQuery:
    @respx.mock
    def test_temporal_query_returns_decisions(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/query/temporal").respond(
            200,
            json={
                "data": {
                    "as_of": NOW,
                    "decisions": [_decision_json()],
                }
            },
        )

        with _make_client() as client:
            decisions = client.temporal_query(
                as_of=datetime.now(tz=timezone.utc),
            )

        assert len(decisions) == 1


class TestAgentHistory:
    @respx.mock
    def test_agent_history_returns_decisions(self) -> None:
        _mock_auth(respx)
        respx.get(f"{BASE_URL}/v1/agents/coder/history").respond(
            200,
            json={
                "data": {
                    "agent_id": "coder",
                    "decisions": [_decision_json()],
                    "total": 1,
                    "limit": 50,
                    "offset": 0,
                }
            },
        )

        with _make_client() as client:
            decisions = client.agent_history("coder")

        assert len(decisions) == 1


class TestCreateGrant:
    @respx.mock
    def test_create_grant_returns_grant(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/grants").respond(
            201, json={"data": _grant_json()}
        )

        with _make_client() as client:
            grant = client.create_grant(
                CreateGrantRequest(
                    grantee_agent_id="reader-agent",
                    resource_type="agent_traces",
                    permission="read",
                )
            )

        assert isinstance(grant, Grant)
        assert grant.permission == "read"


class TestDeleteGrant:
    @respx.mock
    def test_delete_grant_succeeds(self) -> None:
        _mock_auth(respx)
        grant_id = uuid.UUID(GRANT_ID)
        respx.delete(f"{BASE_URL}/v1/grants/{grant_id}").respond(204)

        with _make_client() as client:
            client.delete_grant(grant_id)


class TestListConflicts:
    @respx.mock
    def test_list_conflicts_returns_conflicts(self) -> None:
        _mock_auth(respx)
        respx.get(f"{BASE_URL}/v1/conflicts").respond(
            200,
            json={
                "data": {
                    "conflicts": [_conflict_json()],
                    "total": 1,
                    "limit": 25,
                    "offset": 0,
                }
            },
        )

        with _make_client() as client:
            conflicts = client.list_conflicts()

        assert len(conflicts) == 1
        assert isinstance(conflicts[0], DecisionConflict)
        assert conflicts[0].outcome_a == "monolith"


class TestHealth:
    @respx.mock
    def test_health_returns_health_response(self) -> None:
        # Health does NOT require auth — no _mock_auth call.
        respx.get(f"{BASE_URL}/health").respond(
            200,
            json={
                "data": {
                    "status": "healthy",
                    "version": "0.1.0",
                    "postgres": "connected",
                    "qdrant": "connected",
                    "uptime_seconds": 3600,
                }
            },
        )

        with _make_client() as client:
            health = client.health()

        assert isinstance(health, HealthResponse)
        assert health.status == "healthy"
        assert health.uptime_seconds == 3600

    @respx.mock
    def test_health_no_auth_header_sent(self) -> None:
        """Verify the health endpoint does not send an Authorization header."""

        def _check_no_auth(request: httpx.Request) -> httpx.Response:
            assert "Authorization" not in request.headers
            return httpx.Response(
                200,
                json={
                    "data": {
                        "status": "healthy",
                        "version": "0.1.0",
                        "postgres": "connected",
                        "uptime_seconds": 100,
                    }
                },
            )

        respx.get(f"{BASE_URL}/health").mock(side_effect=_check_no_auth)

        with _make_client() as client:
            client.health()


class TestUserAgent:
    @respx.mock
    def test_user_agent_on_all_requests(self) -> None:
        """Verify User-Agent is sent on check (POST) and recent (GET) requests."""
        _mock_auth(respx)

        captured_uas: list[str] = []

        def _capture_ua(request: httpx.Request) -> httpx.Response:
            captured_uas.append(request.headers.get("user-agent", ""))
            return httpx.Response(
                200,
                json={
                    "data": {
                        "has_precedent": False,
                        "decisions": [],
                        "conflicts": [],
                    }
                },
            )

        respx.post(f"{BASE_URL}/v1/check").mock(side_effect=_capture_ua)

        with _make_client() as client:
            client.check("test")

        assert len(captured_uas) == 1
        assert captured_uas[0] == _USER_AGENT


class TestDecisionSpec31Fields:
    @respx.mock
    def test_decision_deserializes_session_id_and_agent_context(self) -> None:
        _mock_auth(respx)
        session_id = str(uuid.uuid4())
        respx.post(f"{BASE_URL}/v1/query").respond(
            200,
            json={
                "data": {
                    "decisions": [
                        {
                            **_decision_json(),
                            "session_id": session_id,
                            "agent_context": {
                                "tool": "claude-code",
                                "tool_version": "akashi-python/0.2.0",
                                "model": "claude-opus-4-6",
                            },
                        }
                    ],
                    "total": 1,
                    "limit": 50,
                    "offset": 0,
                }
            },
        )

        with _make_client() as client:
            resp = client.query()

        assert len(resp.decisions) == 1
        d = resp.decisions[0]
        assert str(d.session_id) == session_id
        assert d.agent_context["tool"] == "claude-code"
        assert d.agent_context["model"] == "claude-opus-4-6"


class TestErrorMapping:
    @respx.mock
    def test_401_raises_authentication_error(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/check").respond(
            401,
            json={"error": {"code": "UNAUTHORIZED", "message": "bad token"}},
        )
        with _make_client() as client:
            with pytest.raises(AuthenticationError, match="bad token"):
                client.check("test")

    @respx.mock
    def test_403_raises_authorization_error(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/check").respond(
            403,
            json={"error": {"code": "FORBIDDEN", "message": "no access"}},
        )
        with _make_client() as client:
            with pytest.raises(AuthorizationError, match="no access"):
                client.check("test")

    @respx.mock
    def test_404_raises_not_found_error(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/check").respond(
            404,
            json={"error": {"code": "NOT_FOUND", "message": "not found"}},
        )
        with _make_client() as client:
            with pytest.raises(NotFoundError, match="not found"):
                client.check("test")

    @respx.mock
    def test_400_raises_validation_error(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/check").respond(
            400,
            json={
                "error": {
                    "code": "INVALID_INPUT",
                    "message": "decision_type required",
                }
            },
        )
        with _make_client() as client:
            with pytest.raises(ValidationError, match="decision_type required"):
                client.check("test")

    @respx.mock
    def test_429_raises_rate_limit_error(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/check").respond(
            429,
            json={
                "error": {
                    "code": "RATE_LIMITED",
                    "message": "too many requests",
                }
            },
        )
        with _make_client() as client:
            with pytest.raises(RateLimitError, match="too many requests"):
                client.check("test")

    @respx.mock
    def test_500_raises_server_error(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/check").respond(
            500,
            json={"error": {"code": "INTERNAL", "message": "kaboom"}},
        )
        with _make_client() as client:
            with pytest.raises(ServerError, match="kaboom"):
                client.check("test")


class TestMiddleware:
    @respx.mock
    def test_sync_middleware_check_then_trace(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/check").respond(
            200,
            json={
                "data": {
                    "has_precedent": False,
                    "decisions": [],
                    "conflicts": [],
                }
            },
        )
        respx.post(f"{BASE_URL}/v1/trace").respond(
            201,
            json={
                "data": {
                    "run_id": RUN_ID,
                    "decision_id": DECISION_ID,
                    "event_count": 1,
                }
            },
        )

        class MyResult:
            def to_trace(self) -> TraceRequest:
                return TraceRequest(
                    decision_type="test",
                    outcome="result",
                    confidence=0.7,
                )

        def decide(precedents: CheckResponse) -> MyResult:
            assert precedents.has_precedent is False
            return MyResult()

        with _make_client() as client:
            middleware = AkashiSyncMiddleware(client=client)
            result = middleware.wrap("test", decide)

        assert isinstance(result, MyResult)


class TestHandleResponse:
    def test_unwraps_data_envelope(self) -> None:
        resp = httpx.Response(200, json={"data": {"key": "value"}})
        result = _handle_response(resp)
        assert result == {"key": "value"}

    def test_falls_back_without_envelope(self) -> None:
        resp = httpx.Response(200, json={"decisions": []})
        result = _handle_response(resp)
        assert result == {"decisions": []}


class TestAsyncClient:
    """Verify that at least one async method works via the async client."""

    @respx.mock
    @pytest.mark.asyncio
    async def test_async_health(self) -> None:
        respx.get(f"{BASE_URL}/health").respond(
            200,
            json={
                "data": {
                    "status": "healthy",
                    "version": "0.1.0",
                    "postgres": "connected",
                    "uptime_seconds": 99,
                }
            },
        )

        async with _make_async_client() as client:
            health = await client.health()

        assert isinstance(health, HealthResponse)
        assert health.status == "healthy"

    @respx.mock
    @pytest.mark.asyncio
    async def test_async_create_run(self) -> None:
        _mock_auth(respx)
        respx.post(f"{BASE_URL}/v1/runs").respond(
            201, json={"data": _run_json()}
        )

        async with _make_async_client() as client:
            run = await client.create_run(trace_id="async-trace")

        assert isinstance(run, AgentRun)
        assert run.status == "running"

    @respx.mock
    @pytest.mark.asyncio
    async def test_async_delete_grant(self) -> None:
        _mock_auth(respx)
        grant_id = uuid.UUID(GRANT_ID)
        respx.delete(f"{BASE_URL}/v1/grants/{grant_id}").respond(204)

        async with _make_async_client() as client:
            await client.delete_grant(grant_id)
