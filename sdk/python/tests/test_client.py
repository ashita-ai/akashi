"""Tests for the Akashi Python SDK client."""

from __future__ import annotations

import uuid
from datetime import datetime, timezone

import httpx
import pytest
import respx

from akashi.client import AkashiSyncClient, _handle_response
from akashi.exceptions import (
    AuthenticationError,
    AuthorizationError,
    NotFoundError,
    ServerError,
    ValidationError,
)
from akashi.middleware import AkashiSyncMiddleware
from akashi.types import CheckResponse, TraceRequest, TraceResponse


BASE_URL = "https://akashi.test"
NOW = datetime.now(tz=timezone.utc).isoformat()
DECISION_ID = str(uuid.uuid4())
RUN_ID = str(uuid.uuid4())


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
            import json

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
