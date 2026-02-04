"""Kyoyu Python client — async and sync HTTP client for the decision-tracing API."""

from __future__ import annotations

from typing import Any

import httpx

from kyoyu.auth import TokenManager
from kyoyu.exceptions import (
    AuthenticationError,
    AuthorizationError,
    ConflictError,
    KyoyuError,
    NotFoundError,
    ServerError,
    ValidationError,
)
from kyoyu.types import (
    CheckResponse,
    Decision,
    QueryFilters,
    QueryResponse,
    SearchResponse,
    TraceRequest,
    TraceResponse,
)


# ---------------------------------------------------------------------------
# Shared body builders — single source of truth for request shapes.
# These are pure functions (no I/O) that translate SDK types into the
# wire format the server expects. Both async and sync clients call them.
# ---------------------------------------------------------------------------


def _build_check_body(
    decision_type: str,
    query: str | None,
    agent_id: str | None,
    limit: int,
) -> dict[str, Any]:
    body: dict[str, Any] = {"decision_type": decision_type, "limit": limit}
    if query is not None:
        body["query"] = query
    if agent_id is not None:
        body["agent_id"] = agent_id
    return body


def _build_trace_body(agent_id: str, request: TraceRequest) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/trace.

    The server expects ``{agent_id, decision: {decision_type, outcome, ...}}``.
    Rather than popping keys one-by-one and hoping we didn't miss any,
    we build the ``decision`` dict directly from the model's fields.
    """
    decision: dict[str, Any] = {
        "decision_type": request.decision_type,
        "outcome": request.outcome,
        "confidence": request.confidence,
    }
    if request.reasoning is not None:
        decision["reasoning"] = request.reasoning
    if request.alternatives:
        decision["alternatives"] = [a.model_dump(exclude_none=True) for a in request.alternatives]
    if request.evidence:
        decision["evidence"] = [e.model_dump(exclude_none=True) for e in request.evidence]

    body: dict[str, Any] = {"agent_id": agent_id, "decision": decision}
    if request.metadata:
        body["metadata"] = request.metadata
    return body


def _build_query_body(
    filters: QueryFilters | None,
    limit: int,
    offset: int,
    order_by: str,
    order_dir: str,
) -> dict[str, Any]:
    return {
        "filters": filters.model_dump(exclude_none=True) if filters else {},
        "limit": limit,
        "offset": offset,
        "order_by": order_by,
        "order_dir": order_dir,
    }


def _build_search_body(query: str, limit: int) -> dict[str, Any]:
    return {"query": query, "limit": limit}


def _build_recent_params(
    limit: int,
    agent_id: str | None,
    decision_type: str | None,
) -> dict[str, str]:
    params: dict[str, str] = {"limit": str(limit)}
    if agent_id is not None:
        params["agent_id"] = agent_id
    if decision_type is not None:
        params["decision_type"] = decision_type
    return params


# ---------------------------------------------------------------------------
# Shared response handling
# ---------------------------------------------------------------------------


def _extract_error_message(resp: httpx.Response, fallback: str) -> str:
    """Best-effort extraction of the server's error message."""
    try:
        return resp.json().get("error", {}).get("message", fallback)
    except Exception:
        return fallback


def _handle_response(resp: httpx.Response) -> dict[str, Any]:
    """Map HTTP status codes to SDK exceptions and unwrap the API envelope."""
    if resp.status_code == 400:
        raise ValidationError(_extract_error_message(resp, "Bad request"))
    if resp.status_code == 401:
        raise AuthenticationError(_extract_error_message(resp, "Authentication failed"))
    if resp.status_code == 403:
        raise AuthorizationError(_extract_error_message(resp, "Insufficient permissions"))
    if resp.status_code == 404:
        raise NotFoundError(_extract_error_message(resp, "Resource not found"))
    if resp.status_code == 409:
        raise ConflictError(_extract_error_message(resp, "Conflict"))
    if resp.status_code >= 500:
        raise ServerError(_extract_error_message(resp, f"Server error: {resp.status_code}"))
    if resp.status_code >= 400:
        raise KyoyuError(_extract_error_message(resp, f"Unexpected error: {resp.status_code}"))
    body = resp.json()
    return body.get("data", body)


# ---------------------------------------------------------------------------
# Async client
# ---------------------------------------------------------------------------


class KyoyuClient:
    """Async HTTP client for the Kyoyu decision-tracing API.

    Usage::

        async with KyoyuClient(base_url="http://localhost:8080",
                                agent_id="my-agent",
                                api_key="secret") as client:
            resp = await client.check("architecture")
            if not resp.has_precedent:
                await client.trace(TraceRequest(
                    decision_type="architecture",
                    outcome="chose event sourcing",
                    confidence=0.8,
                    reasoning="Auditability requirement",
                ))
    """

    def __init__(
        self,
        base_url: str,
        agent_id: str,
        api_key: str,
        *,
        timeout: float = 30.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.agent_id = agent_id
        self._token_mgr = TokenManager(
            base_url=self.base_url,
            agent_id=agent_id,
            api_key=api_key,
        )
        self._client = httpx.AsyncClient(timeout=timeout)

    async def __aenter__(self) -> KyoyuClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    async def close(self) -> None:
        """Close the underlying HTTP client."""
        await self._client.aclose()

    # --- Core API methods ---

    async def check(
        self,
        decision_type: str,
        query: str | None = None,
        *,
        agent_id: str | None = None,
        limit: int = 5,
    ) -> CheckResponse:
        """Check for existing decisions before making a new one."""
        data = await self._post("/v1/check", _build_check_body(decision_type, query, agent_id, limit))
        return CheckResponse.model_validate(data)

    async def trace(self, request: TraceRequest) -> TraceResponse:
        """Record a decision trace."""
        data = await self._post("/v1/trace", _build_trace_body(self.agent_id, request))
        return TraceResponse.model_validate(data)

    async def query(
        self,
        filters: QueryFilters | None = None,
        *,
        limit: int = 50,
        offset: int = 0,
        order_by: str = "valid_from",
        order_dir: str = "desc",
    ) -> QueryResponse:
        """Query past decisions with structured filters."""
        data = await self._post("/v1/query", _build_query_body(filters, limit, offset, order_by, order_dir))
        return QueryResponse.model_validate(data)

    async def search(self, query: str, *, limit: int = 5) -> SearchResponse:
        """Search decision history by semantic similarity."""
        data = await self._post("/v1/search", _build_search_body(query, limit))
        return SearchResponse.model_validate(data)

    async def recent(
        self,
        *,
        limit: int = 10,
        agent_id: str | None = None,
        decision_type: str | None = None,
    ) -> list[Decision]:
        """Get the most recent decisions."""
        data = await self._get("/v1/decisions/recent", params=_build_recent_params(limit, agent_id, decision_type))
        return [Decision.model_validate(d) for d in data.get("decisions", [])]

    # --- HTTP transport ---

    async def _post(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        token = await self._token_mgr.get_token(self._client)
        resp = await self._client.post(
            f"{self.base_url}{path}",
            json=body,
            headers={"Authorization": f"Bearer {token}"},
        )
        return _handle_response(resp)

    async def _get(self, path: str, *, params: dict[str, str] | None = None) -> dict[str, Any]:
        token = await self._token_mgr.get_token(self._client)
        resp = await self._client.get(
            f"{self.base_url}{path}",
            params=params,
            headers={"Authorization": f"Bearer {token}"},
        )
        return _handle_response(resp)


# ---------------------------------------------------------------------------
# Sync client — delegates body building and response handling to shared code.
# ---------------------------------------------------------------------------


class KyoyuSyncClient:
    """Synchronous HTTP client for the Kyoyu decision-tracing API.

    Usage::

        with KyoyuSyncClient(base_url="http://localhost:8080",
                              agent_id="my-agent",
                              api_key="secret") as client:
            resp = client.check("architecture")
    """

    def __init__(
        self,
        base_url: str,
        agent_id: str,
        api_key: str,
        *,
        timeout: float = 30.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.agent_id = agent_id
        self._token_mgr = TokenManager(
            base_url=self.base_url,
            agent_id=agent_id,
            api_key=api_key,
        )
        self._client = httpx.Client(timeout=timeout)

    def __enter__(self) -> KyoyuSyncClient:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def close(self) -> None:
        """Close the underlying HTTP client."""
        self._client.close()

    def check(
        self,
        decision_type: str,
        query: str | None = None,
        *,
        agent_id: str | None = None,
        limit: int = 5,
    ) -> CheckResponse:
        """Check for existing decisions before making a new one."""
        data = self._post("/v1/check", _build_check_body(decision_type, query, agent_id, limit))
        return CheckResponse.model_validate(data)

    def trace(self, request: TraceRequest) -> TraceResponse:
        """Record a decision trace."""
        data = self._post("/v1/trace", _build_trace_body(self.agent_id, request))
        return TraceResponse.model_validate(data)

    def query(
        self,
        filters: QueryFilters | None = None,
        *,
        limit: int = 50,
        offset: int = 0,
        order_by: str = "valid_from",
        order_dir: str = "desc",
    ) -> QueryResponse:
        """Query past decisions with structured filters."""
        data = self._post("/v1/query", _build_query_body(filters, limit, offset, order_by, order_dir))
        return QueryResponse.model_validate(data)

    def search(self, query: str, *, limit: int = 5) -> SearchResponse:
        """Search decision history by semantic similarity."""
        data = self._post("/v1/search", _build_search_body(query, limit))
        return SearchResponse.model_validate(data)

    def recent(
        self,
        *,
        limit: int = 10,
        agent_id: str | None = None,
        decision_type: str | None = None,
    ) -> list[Decision]:
        """Get the most recent decisions."""
        data = self._get("/v1/decisions/recent", params=_build_recent_params(limit, agent_id, decision_type))
        return [Decision.model_validate(d) for d in data.get("decisions", [])]

    # --- HTTP transport ---

    def _post(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        token = self._token_mgr.get_token_sync(self._client)
        resp = self._client.post(
            f"{self.base_url}{path}",
            json=body,
            headers={"Authorization": f"Bearer {token}"},
        )
        return _handle_response(resp)

    def _get(self, path: str, *, params: dict[str, str] | None = None) -> dict[str, Any]:
        token = self._token_mgr.get_token_sync(self._client)
        resp = self._client.get(
            f"{self.base_url}{path}",
            params=params,
            headers={"Authorization": f"Bearer {token}"},
        )
        return _handle_response(resp)
