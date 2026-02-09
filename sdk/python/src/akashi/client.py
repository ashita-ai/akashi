"""Akashi Python client — async and sync HTTP client for the decision-tracing API."""

from __future__ import annotations

from datetime import datetime
from typing import Any
from uuid import UUID

import httpx

from akashi.auth import TokenManager
from akashi.exceptions import (
    AuthenticationError,
    AuthorizationError,
    ConflictError,
    AkashiError,
    NotFoundError,
    RateLimitError,
    ServerError,
    ValidationError,
)
from akashi.types import (
    Agent,
    AgentRun,
    CheckResponse,
    CompleteRunRequest,
    CreateAgentRequest,
    CreateGrantRequest,
    Decision,
    DecisionConflict,
    EventInput,
    Grant,
    HealthResponse,
    QueryFilters,
    QueryResponse,
    SearchResponse,
    TraceRequest,
    TraceResponse,
    UsageResponse,
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


def _build_create_run_body(agent_id: str, req: CreateRunRequest | None) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/runs."""
    body: dict[str, Any] = {"agent_id": agent_id}
    if req is not None:
        if req.trace_id is not None:
            body["trace_id"] = req.trace_id
        if req.parent_run_id is not None:
            body["parent_run_id"] = str(req.parent_run_id)
        if req.metadata:
            body["metadata"] = req.metadata
    return body


def _build_append_events_body(events: list[EventInput]) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/runs/{run_id}/events."""
    serialized: list[dict[str, Any]] = []
    for ev in events:
        d: dict[str, Any] = {"event_type": ev.event_type, "payload": ev.payload}
        if ev.occurred_at is not None:
            d["occurred_at"] = ev.occurred_at.isoformat()
        serialized.append(d)
    return {"events": serialized}


def _build_complete_run_body(req: CompleteRunRequest) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/runs/{run_id}/complete."""
    body: dict[str, Any] = {"status": req.status}
    if req.metadata:
        body["metadata"] = req.metadata
    return body


def _build_temporal_query_body(
    as_of: datetime,
    filters: QueryFilters | None,
) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/query/temporal."""
    return {
        "as_of": as_of.isoformat(),
        "filters": filters.model_dump(exclude_none=True) if filters else {},
    }


def _build_create_agent_body(req: CreateAgentRequest) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/agents."""
    body: dict[str, Any] = {
        "agent_id": req.agent_id,
        "name": req.name,
        "role": req.role,
        "api_key": req.api_key,
    }
    if req.metadata:
        body["metadata"] = req.metadata
    return body


def _build_create_grant_body(req: CreateGrantRequest) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/grants."""
    body: dict[str, Any] = {
        "grantee_agent_id": req.grantee_agent_id,
        "resource_type": req.resource_type,
        "permission": req.permission,
    }
    if req.resource_id is not None:
        body["resource_id"] = req.resource_id
    if req.expires_at is not None:
        body["expires_at"] = req.expires_at
    return body


def _build_conflicts_params(
    decision_type: str | None,
    limit: int,
    offset: int,
) -> dict[str, str]:
    """Build query params for GET /v1/conflicts."""
    params: dict[str, str] = {"limit": str(limit), "offset": str(offset)}
    if decision_type is not None:
        params["decision_type"] = decision_type
    return params


def _build_agent_history_params(limit: int) -> dict[str, str]:
    """Build query params for GET /v1/agents/{agent_id}/history."""
    return {"limit": str(limit)}


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
    if resp.status_code == 429:
        raise RateLimitError(_extract_error_message(resp, "Rate limit exceeded"))
    if resp.status_code >= 500:
        raise ServerError(_extract_error_message(resp, f"Server error: {resp.status_code}"))
    if resp.status_code >= 400:
        raise AkashiError(_extract_error_message(resp, f"Unexpected error: {resp.status_code}"))
    body = resp.json()
    return body.get("data", body)


def _handle_no_content(resp: httpx.Response) -> None:
    """Handle responses that should be 204 No Content."""
    if resp.status_code == 204:
        return
    # Delegate error handling to the standard handler; the return value is ignored.
    _handle_response(resp)


# ---------------------------------------------------------------------------
# Async client
# ---------------------------------------------------------------------------


class AkashiClient:
    """Async HTTP client for the Akashi decision-tracing API.

    Usage::

        async with AkashiClient(base_url="http://localhost:8080",
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

    async def __aenter__(self) -> AkashiClient:
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

    # --- Runs ---

    async def create_run(
        self,
        *,
        trace_id: str | None = None,
        parent_run_id: UUID | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> AgentRun:
        """Create a new agent run."""
        from akashi.types import CreateRunRequest

        req = CreateRunRequest(
            trace_id=trace_id,
            parent_run_id=parent_run_id,
            metadata=metadata or {},
        )
        data = await self._post("/v1/runs", _build_create_run_body(self.agent_id, req))
        return AgentRun.model_validate(data)

    async def append_events(self, run_id: UUID, events: list[EventInput]) -> None:
        """Append events to an existing run."""
        await self._post(f"/v1/runs/{run_id}/events", _build_append_events_body(events))

    async def complete_run(
        self,
        run_id: UUID,
        status: str,
        *,
        metadata: dict[str, Any] | None = None,
    ) -> AgentRun:
        """Mark a run as completed or failed."""
        req = CompleteRunRequest(status=status, metadata=metadata or {})
        data = await self._post(f"/v1/runs/{run_id}/complete", _build_complete_run_body(req))
        return AgentRun.model_validate(data)

    async def get_run(self, run_id: UUID) -> dict[str, Any]:
        """Get a run with its events and decisions."""
        return await self._get(f"/v1/runs/{run_id}")

    # --- Agents (admin-only) ---

    async def create_agent(self, req: CreateAgentRequest) -> Agent:
        """Create a new agent (admin-only)."""
        data = await self._post("/v1/agents", _build_create_agent_body(req))
        return Agent.model_validate(data)

    async def list_agents(self) -> list[Agent]:
        """List all agents in the organization (admin-only)."""
        data = await self._get("/v1/agents")
        # Server returns an array directly (wrapped by writeJSON in data envelope).
        if isinstance(data, list):
            return [Agent.model_validate(a) for a in data]
        return [Agent.model_validate(a) for a in data.get("agents", data)]

    async def delete_agent(self, agent_id: str) -> None:
        """Delete an agent and all associated data (admin-only)."""
        await self._delete(f"/v1/agents/{agent_id}")

    # --- Temporal query ---

    async def temporal_query(
        self,
        as_of: datetime,
        filters: QueryFilters | None = None,
    ) -> list[Decision]:
        """Query decisions as they existed at a specific point in time."""
        data = await self._post("/v1/query/temporal", _build_temporal_query_body(as_of, filters))
        return [Decision.model_validate(d) for d in data.get("decisions", [])]

    # --- Agent history ---

    async def agent_history(self, agent_id: str, *, limit: int = 50) -> list[Decision]:
        """Get the decision history for a specific agent."""
        data = await self._get(
            f"/v1/agents/{agent_id}/history",
            params=_build_agent_history_params(limit),
        )
        return [Decision.model_validate(d) for d in data.get("decisions", [])]

    # --- Grants ---

    async def create_grant(self, req: CreateGrantRequest) -> Grant:
        """Create an access grant between agents."""
        data = await self._post("/v1/grants", _build_create_grant_body(req))
        return Grant.model_validate(data)

    async def delete_grant(self, grant_id: UUID) -> None:
        """Revoke an access grant."""
        await self._delete(f"/v1/grants/{grant_id}")

    # --- Conflicts ---

    async def list_conflicts(
        self,
        *,
        decision_type: str | None = None,
        limit: int = 25,
        offset: int = 0,
    ) -> list[DecisionConflict]:
        """List detected decision conflicts."""
        data = await self._get(
            "/v1/conflicts",
            params=_build_conflicts_params(decision_type, limit, offset),
        )
        return [DecisionConflict.model_validate(c) for c in data.get("conflicts", [])]

    # --- Usage ---

    async def get_usage(self) -> UsageResponse:
        """Get the current billing period's usage statistics."""
        data = await self._get("/v1/usage")
        return UsageResponse.model_validate(data)

    # --- Health (no auth) ---

    async def health(self) -> HealthResponse:
        """Check server health. Does not require authentication."""
        data = await self._get_no_auth("/health")
        return HealthResponse.model_validate(data)

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

    async def _delete(self, path: str) -> None:
        token = await self._token_mgr.get_token(self._client)
        resp = await self._client.delete(
            f"{self.base_url}{path}",
            headers={"Authorization": f"Bearer {token}"},
        )
        _handle_no_content(resp)

    async def _get_no_auth(self, path: str) -> dict[str, Any]:
        resp = await self._client.get(f"{self.base_url}{path}")
        return _handle_response(resp)


# ---------------------------------------------------------------------------
# Sync client — delegates body building and response handling to shared code.
# ---------------------------------------------------------------------------


class AkashiSyncClient:
    """Synchronous HTTP client for the Akashi decision-tracing API.

    Usage::

        with AkashiSyncClient(base_url="http://localhost:8080",
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

    def __enter__(self) -> AkashiSyncClient:
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

    # --- Runs ---

    def create_run(
        self,
        *,
        trace_id: str | None = None,
        parent_run_id: UUID | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> AgentRun:
        """Create a new agent run."""
        from akashi.types import CreateRunRequest

        req = CreateRunRequest(
            trace_id=trace_id,
            parent_run_id=parent_run_id,
            metadata=metadata or {},
        )
        data = self._post("/v1/runs", _build_create_run_body(self.agent_id, req))
        return AgentRun.model_validate(data)

    def append_events(self, run_id: UUID, events: list[EventInput]) -> None:
        """Append events to an existing run."""
        self._post(f"/v1/runs/{run_id}/events", _build_append_events_body(events))

    def complete_run(
        self,
        run_id: UUID,
        status: str,
        *,
        metadata: dict[str, Any] | None = None,
    ) -> AgentRun:
        """Mark a run as completed or failed."""
        req = CompleteRunRequest(status=status, metadata=metadata or {})
        data = self._post(f"/v1/runs/{run_id}/complete", _build_complete_run_body(req))
        return AgentRun.model_validate(data)

    def get_run(self, run_id: UUID) -> dict[str, Any]:
        """Get a run with its events and decisions."""
        return self._get(f"/v1/runs/{run_id}")

    # --- Agents (admin-only) ---

    def create_agent(self, req: CreateAgentRequest) -> Agent:
        """Create a new agent (admin-only)."""
        data = self._post("/v1/agents", _build_create_agent_body(req))
        return Agent.model_validate(data)

    def list_agents(self) -> list[Agent]:
        """List all agents in the organization (admin-only)."""
        data = self._get("/v1/agents")
        if isinstance(data, list):
            return [Agent.model_validate(a) for a in data]
        return [Agent.model_validate(a) for a in data.get("agents", data)]

    def delete_agent(self, agent_id: str) -> None:
        """Delete an agent and all associated data (admin-only)."""
        self._delete(f"/v1/agents/{agent_id}")

    # --- Temporal query ---

    def temporal_query(
        self,
        as_of: datetime,
        filters: QueryFilters | None = None,
    ) -> list[Decision]:
        """Query decisions as they existed at a specific point in time."""
        data = self._post("/v1/query/temporal", _build_temporal_query_body(as_of, filters))
        return [Decision.model_validate(d) for d in data.get("decisions", [])]

    # --- Agent history ---

    def agent_history(self, agent_id: str, *, limit: int = 50) -> list[Decision]:
        """Get the decision history for a specific agent."""
        data = self._get(
            f"/v1/agents/{agent_id}/history",
            params=_build_agent_history_params(limit),
        )
        return [Decision.model_validate(d) for d in data.get("decisions", [])]

    # --- Grants ---

    def create_grant(self, req: CreateGrantRequest) -> Grant:
        """Create an access grant between agents."""
        data = self._post("/v1/grants", _build_create_grant_body(req))
        return Grant.model_validate(data)

    def delete_grant(self, grant_id: UUID) -> None:
        """Revoke an access grant."""
        self._delete(f"/v1/grants/{grant_id}")

    # --- Conflicts ---

    def list_conflicts(
        self,
        *,
        decision_type: str | None = None,
        limit: int = 25,
        offset: int = 0,
    ) -> list[DecisionConflict]:
        """List detected decision conflicts."""
        data = self._get(
            "/v1/conflicts",
            params=_build_conflicts_params(decision_type, limit, offset),
        )
        return [DecisionConflict.model_validate(c) for c in data.get("conflicts", [])]

    # --- Usage ---

    def get_usage(self) -> UsageResponse:
        """Get the current billing period's usage statistics."""
        data = self._get("/v1/usage")
        return UsageResponse.model_validate(data)

    # --- Health (no auth) ---

    def health(self) -> HealthResponse:
        """Check server health. Does not require authentication."""
        data = self._get_no_auth("/health")
        return HealthResponse.model_validate(data)

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

    def _delete(self, path: str) -> None:
        token = self._token_mgr.get_token_sync(self._client)
        resp = self._client.delete(
            f"{self.base_url}{path}",
            headers={"Authorization": f"Bearer {token}"},
        )
        _handle_no_content(resp)

    def _get_no_auth(self, path: str) -> dict[str, Any]:
        resp = self._client.get(f"{self.base_url}{path}")
        return _handle_response(resp)
