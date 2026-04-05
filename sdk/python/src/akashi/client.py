"""Akashi Python client — async and sync HTTP client for the decision-tracing API."""

from __future__ import annotations

import asyncio
import json as _json
import time as _time
from collections.abc import AsyncIterator, Iterator
from datetime import datetime
from typing import Any
from uuid import UUID, uuid4

import httpx

_USER_AGENT = "akashi-python/0.2.0"
_MAX_RESPONSE_BYTES = 10 * 1024 * 1024  # 10 MiB

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
from akashi.retry import (
    DEFAULT_MAX_RETRIES,
    DEFAULT_RETRY_BASE_DELAY,
    is_retryable_status,
    retry_delay,
    parse_retry_after,
)
from akashi.types import (
    AdjudicateConflictRequest,
    Agent,
    AgentEvent,
    AgentRun,
    AgentStatsResponse,
    APIKey,
    APIKeyWithRawKey,
    AssessRequest,
    AssessResponse,
    CheckResponse,
    CompleteRunRequest,
    ConfigResponse,
    ConflictAnalyticsResponse,
    ConflictDetail,
    ConflictEvalResponse,
    ConflictGroup,
    ConflictLabelRecord,
    ConflictStatusUpdate,
    CreateAgentRequest,
    CreateGrantRequest,
    CreateHoldRequest,
    CreateKeyRequest,
    CreateProjectLinkRequest,
    Decision,
    DecisionConflict,
    EraseDecisionResponse,
    EventInput,
    FacetsResponse,
    GetRunResponse,
    Grant,
    HealthResponse,
    IntegrityViolationsResponse,
    LineageResponse,
    ListConflictLabelsResponse,
    OrgSettingsData,
    ProjectLink,
    PurgeRequest,
    PurgeResponse,
    QueryFilters,
    QueryResponse,
    ResolveConflictGroupRequest,
    ResolveConflictGroupResponse,
    RetentionHold,
    RetentionPolicy,
    RevisionsResponse,
    RotateKeyResponse,
    ScopedTokenRequest,
    ScopedTokenResponse,
    ScorerEvalResponse,
    SearchResponse,
    SearchResult,
    SessionViewResponse,
    SetRetentionRequest,
    SignupRequest,
    SignupResponse,
    SubscriptionEvent,
    TimelineResponse,
    TraceHealthResponse,
    TraceRequest,
    TraceResponse,
    UpdateAgentRequest,
    UpsertConflictLabelRequest,
    UsageResponse,
    ValidatePairRequest,
    ValidatePairResponse,
    VerifyResponse,
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
    project: str | None = None,
    format: str | None = None,
) -> dict[str, Any]:
    body: dict[str, Any] = {"decision_type": decision_type, "limit": limit}
    if query is not None:
        body["query"] = query
    if agent_id is not None:
        body["agent_id"] = agent_id
    if project is not None:
        body["project"] = project
    if format is not None:
        body["format"] = format
    return body


def _infer_project_from_git() -> str:
    """Resolve the canonical project name from git remote origin.

    Runs ``git remote get-url origin``, strips ``.git``, and returns the
    basename. Cached for the process lifetime. Returns "" on any failure.
    """
    cached = getattr(_infer_project_from_git, "_cached", None)
    if cached is not None:
        return cached

    import subprocess  # noqa: PLC0415 — deferred to avoid import cost when not needed

    try:
        result = subprocess.run(
            ["git", "remote", "get-url", "origin"],
            capture_output=True,
            text=True,
            timeout=2,
            check=False,
        )
        if result.returncode != 0 or not result.stdout.strip():
            _infer_project_from_git._cached = ""  # type: ignore[attr-defined]
            return ""
        import posixpath

        remote = result.stdout.strip().removesuffix(".git")
        name = posixpath.basename(remote)
        _infer_project_from_git._cached = name  # type: ignore[attr-defined]
        return name
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        _infer_project_from_git._cached = ""  # type: ignore[attr-defined]
        return ""


def _build_trace_body(agent_id: str, request: TraceRequest) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/trace.

    The server expects ``{agent_id, decision: {decision_type, outcome, ...}}``.
    Rather than popping keys one-by-one and hoping we didn't miss any,
    we build the ``decision`` dict directly from the model's fields.

    decision_type is normalized to lowercase with surrounding whitespace stripped
    so that callers using "Architecture" vs "architecture" are treated identically.

    When the caller has not set ``context["project"]``, auto-detects the project
    from ``git remote get-url origin`` to prevent workspace names from leaking
    as project identifiers.
    """
    decision: dict[str, Any] = {
        "decision_type": request.decision_type.strip().lower(),
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
    if request.precedent_ref is not None:
        body["precedent_ref"] = str(request.precedent_ref)
    if request.precedent_reason is not None:
        body["precedent_reason"] = request.precedent_reason
    if request.supersedes_id is not None:
        body["supersedes_id"] = str(request.supersedes_id)
    if request.trace_id is not None:
        body["trace_id"] = request.trace_id
    if request.metadata:
        body["metadata"] = request.metadata

    ctx = dict(request.context) if request.context else {}
    if not ctx.get("project"):
        detected = _infer_project_from_git()
        if detected:
            ctx["project"] = detected
    if ctx:
        body["context"] = ctx
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


def _build_search_body(query: str, limit: int, semantic: bool = False) -> dict[str, Any]:
    return {"query": query, "limit": limit, "semantic": semantic}


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
    agent_id: str | None,
    conflict_kind: str | None,
    limit: int,
    offset: int,
) -> dict[str, str]:
    """Build query params for GET /v1/conflicts."""
    params: dict[str, str] = {"limit": str(limit), "offset": str(offset)}
    if decision_type is not None:
        params["decision_type"] = decision_type
    if agent_id is not None:
        params["agent_id"] = agent_id
    if conflict_kind is not None:
        params["conflict_kind"] = conflict_kind
    return params


def _build_agent_history_params(limit: int) -> dict[str, str]:
    """Build query params for GET /v1/agents/{agent_id}/history."""
    return {"limit": str(limit)}


def _build_assess_body(req: AssessRequest) -> dict[str, Any]:
    """Build the wire-format body for POST /v1/decisions/{id}/assess."""
    body: dict[str, Any] = {"outcome": req.outcome.value}
    if req.notes is not None:
        body["notes"] = req.notes
    return body


# ---------------------------------------------------------------------------
# Shared response handling
# ---------------------------------------------------------------------------


def _extract_error_message(resp: httpx.Response, fallback: str) -> str:
    """Best-effort extraction of the server's error message."""
    try:
        return resp.json().get("error", {}).get("message", fallback)
    except Exception:
        return fallback


def _raise_for_status(resp: httpx.Response) -> None:
    """Raise an SDK exception for error status codes."""
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


def _handle_response(resp: httpx.Response) -> dict[str, Any]:
    """Map HTTP status codes to SDK exceptions and unwrap the API envelope."""
    _raise_for_status(resp)
    body = resp.json()
    return body.get("data", body)


def _handle_list_body(resp: httpx.Response) -> tuple[list[Any], dict[str, Any]]:
    """Parse a list envelope response. Returns (items, pagination_meta)."""
    _raise_for_status(resp)
    body = resp.json()
    items = body.get("data", [])
    if not isinstance(items, list):
        items = []
    meta: dict[str, Any] = {
        "total": body.get("total"),
        "has_more": body.get("has_more", False),
        "limit": body.get("limit", 0),
        "offset": body.get("offset", 0),
    }
    return items, meta


def _handle_no_content(resp: httpx.Response) -> None:
    """Handle responses that should be 204 No Content."""
    if resp.status_code == 204:
        return
    # Delegate error handling to the standard handler; the return value is ignored.
    _handle_response(resp)


def _check_response_size(resp: httpx.Response) -> None:
    """Raise if the response body exceeds the safety limit."""
    if len(resp.content) > _MAX_RESPONSE_BYTES:
        raise AkashiError("Response body exceeds 10 MiB limit")


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
        max_retries: int = DEFAULT_MAX_RETRIES,
        retry_base_delay: float = DEFAULT_RETRY_BASE_DELAY,
        session_id: UUID | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.agent_id = agent_id
        self.session_id: UUID = session_id if session_id is not None else uuid4()
        self._max_retries = max_retries
        self._retry_base_delay = retry_base_delay
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
        project: str | None = None,
        limit: int = 5,
        format: str | None = None,
    ) -> CheckResponse:
        """Check for existing decisions before making a new one."""
        data = await self._post("/v1/check", _build_check_body(decision_type, query, agent_id, limit, project, format))
        return CheckResponse.model_validate(data)

    async def trace(self, request: TraceRequest, *, idempotency_key: str | None = None) -> TraceResponse:
        """Record a decision trace."""
        idem_key = idempotency_key or str(uuid4())
        data = await self._post(
            "/v1/trace",
            _build_trace_body(self.agent_id, request),
            extra_headers={"X-Idempotency-Key": idem_key},
        )
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
        items, meta = await self._post_list("/v1/query", _build_query_body(filters, limit, offset, order_by, order_dir))
        return QueryResponse(
            decisions=[Decision.model_validate(d) for d in items],
            total=meta.get("total") or 0,
            has_more=meta.get("has_more", False),
            limit=meta.get("limit", 0),
            offset=meta.get("offset", 0),
        )

    async def search(self, query: str, *, limit: int = 5, semantic: bool = False) -> SearchResponse:
        """Search decision history by semantic similarity."""
        items, meta = await self._post_list("/v1/search", _build_search_body(query, limit, semantic))
        return SearchResponse(
            results=[SearchResult.model_validate(r) for r in items],
            total=meta.get("total") or len(items),
        )

    async def recent(
        self,
        *,
        limit: int = 10,
        agent_id: str | None = None,
        decision_type: str | None = None,
    ) -> list[Decision]:
        """Get the most recent decisions."""
        items, _ = await self._get_list("/v1/decisions/recent", params=_build_recent_params(limit, agent_id, decision_type))
        return [Decision.model_validate(d) for d in items]

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

    async def append_events(
        self, run_id: UUID, events: list[EventInput], *, idempotency_key: str | None = None,
    ) -> None:
        """Append events to an existing run."""
        idem_key = idempotency_key or str(uuid4())
        await self._post(
            f"/v1/runs/{run_id}/events",
            _build_append_events_body(events),
            extra_headers={"X-Idempotency-Key": idem_key},
        )

    async def complete_run(
        self,
        run_id: UUID,
        status: str,
        *,
        metadata: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> AgentRun:
        """Mark a run as completed or failed."""
        idem_key = idempotency_key or str(uuid4())
        req = CompleteRunRequest(status=status, metadata=metadata or {})
        data = await self._post(
            f"/v1/runs/{run_id}/complete",
            _build_complete_run_body(req),
            extra_headers={"X-Idempotency-Key": idem_key},
        )
        return AgentRun.model_validate(data)

    async def get_run(self, run_id: UUID) -> GetRunResponse:
        """Get a run with its events and decisions."""
        data = await self._get(f"/v1/runs/{run_id}")
        return GetRunResponse(
            run=AgentRun.model_validate(data["run"]),
            events=[AgentEvent.model_validate(e) for e in data.get("events", [])],
            decisions=[Decision.model_validate(d) for d in data.get("decisions", [])],
        )

    # --- Agents (admin-only) ---

    async def create_agent(self, req: CreateAgentRequest) -> Agent:
        """Create a new agent (admin-only)."""
        data = await self._post("/v1/agents", _build_create_agent_body(req))
        return Agent.model_validate(data)

    async def list_agents(self) -> list[Agent]:
        """List all agents in the organization (admin-only)."""
        data = await self._get("/v1/agents")
        return [Agent.model_validate(a) for a in (data if isinstance(data, list) else [])]

    async def delete_agent(self, agent_id: str) -> None:
        """Delete an agent and all associated data (admin-only)."""
        await self._delete(f"/v1/agents/{agent_id}")

    async def update_agent_tags(self, agent_id: str, tags: list[str]) -> Agent:
        """Replace an agent's tags (admin-only)."""
        data = await self._patch(f"/v1/agents/{agent_id}/tags", {"tags": tags})
        return Agent.model_validate(data)

    # --- Integrity ---

    async def verify_decision(self, decision_id: UUID) -> VerifyResponse:
        """Verify the integrity of a decision by recomputing its content hash."""
        data = await self._get(f"/v1/verify/{decision_id}")
        return VerifyResponse.model_validate(data)

    async def get_decision_revisions(self, decision_id: UUID) -> RevisionsResponse:
        """Get the full revision chain for a decision."""
        data = await self._get(f"/v1/decisions/{decision_id}/revisions")
        return RevisionsResponse.model_validate(data)

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
        items, _ = await self._get_list(
            f"/v1/agents/{agent_id}/history",
            params=_build_agent_history_params(limit),
        )
        return [Decision.model_validate(d) for d in items]

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
        agent_id: str | None = None,
        conflict_kind: str | None = None,
        limit: int = 25,
        offset: int = 0,
    ) -> list[DecisionConflict]:
        """List detected decision conflicts."""
        items, _ = await self._get_list(
            "/v1/conflicts",
            params=_build_conflicts_params(
                decision_type, agent_id, conflict_kind, limit, offset
            ),
        )
        return [DecisionConflict.model_validate(c) for c in items]

    # --- Assessments (spec 29) ---

    async def assess(self, decision_id: UUID, req: AssessRequest) -> AssessResponse:
        """Record an outcome assessment for a prior decision.

        Assessments are append-only — each call creates a new row. An assessor
        changing their verdict over time is itself an auditable event; prior
        assessments are never overwritten.
        """
        data = await self._post(f"/v1/decisions/{decision_id}/assess", _build_assess_body(req))
        return AssessResponse.model_validate(data)

    async def list_assessments(self, decision_id: UUID) -> list[AssessResponse]:
        """Return the full assessment history for a decision, newest first."""
        items, _ = await self._get_list(f"/v1/decisions/{decision_id}/assessments")
        return [AssessResponse.model_validate(a) for a in items]

    # --- Health (no auth) ---

    async def health(self) -> HealthResponse:
        """Check server health. Does not require authentication."""
        data = await self._get_no_auth("/health")
        return HealthResponse.model_validate(data)

    # --- Phase 2: Decision details ---

    async def get_decision(self, decision_id: UUID) -> Decision:
        """Get a single decision by ID."""
        data = await self._get(f"/v1/decisions/{decision_id}")
        return Decision.model_validate(data)

    async def get_decision_conflicts(
        self,
        decision_id: UUID,
        *,
        status: str | None = None,
        limit: int = 50,
        offset: int = 0,
    ) -> list[DecisionConflict]:
        """List conflicts for a specific decision."""
        params: dict[str, str] = {"limit": str(limit), "offset": str(offset)}
        if status:
            params["status"] = status
        items, _ = await self._get_list(f"/v1/decisions/{decision_id}/conflicts", params=params)
        return [DecisionConflict.model_validate(c) for c in items]

    async def get_decision_lineage(self, decision_id: UUID) -> LineageResponse:
        """Get the precedent lineage for a decision."""
        data = await self._get(f"/v1/decisions/{decision_id}/lineage")
        return LineageResponse.model_validate(data)

    async def get_decision_timeline(
        self,
        *,
        granularity: str = "day",
        from_time: datetime | None = None,
        to_time: datetime | None = None,
        agent_id: str | None = None,
        project: str | None = None,
    ) -> TimelineResponse:
        """Get a bucketed timeline of decisions."""
        params: dict[str, str] = {"granularity": granularity}
        if from_time:
            params["from"] = from_time.isoformat()
        if to_time:
            params["to"] = to_time.isoformat()
        if agent_id:
            params["agent_id"] = agent_id
        if project:
            params["project"] = project
        data = await self._get("/v1/decisions/timeline", params=params)
        return TimelineResponse.model_validate(data)

    async def get_decision_facets(self) -> FacetsResponse:
        """Get available decision type and project facets."""
        data = await self._get("/v1/decisions/facets")
        return FacetsResponse.model_validate(data)

    async def retract_decision(self, decision_id: UUID, reason: str = "") -> Decision:
        """Retract (soft-delete) a decision."""
        data = await self._delete_with_body(
            f"/v1/decisions/{decision_id}",
            {"reason": reason} if reason else {},
        )
        return Decision.model_validate(data)

    async def patch_decision(self, decision_id: UUID, *, project: str | None = None) -> Decision:
        """Update mutable metadata on a decision (e.g. project)."""
        body: dict[str, Any] = {}
        if project is not None:
            body["project"] = project
        data = await self._patch(f"/v1/decisions/{decision_id}", body)
        return Decision.model_validate(data)

    async def erase_decision(self, decision_id: UUID, reason: str = "") -> EraseDecisionResponse:
        """GDPR-erase a decision (irreversible)."""
        data = await self._post(
            f"/v1/decisions/{decision_id}/erase",
            {"reason": reason} if reason else {},
        )
        return EraseDecisionResponse.model_validate(data)

    # --- Phase 2: Conflict management ---

    async def get_conflict(self, conflict_id: UUID) -> ConflictDetail:
        """Get a single conflict with recommendation."""
        data = await self._get(f"/v1/conflicts/{conflict_id}")
        return ConflictDetail.model_validate(data)

    async def adjudicate_conflict(self, conflict_id: UUID, req: AdjudicateConflictRequest) -> ConflictDetail:
        """Adjudicate a conflict by recording a resolution decision."""
        data = await self._post(
            f"/v1/conflicts/{conflict_id}/adjudicate",
            req.model_dump(exclude_none=True),
        )
        return ConflictDetail.model_validate(data)

    async def patch_conflict(self, conflict_id: UUID, req: ConflictStatusUpdate) -> DecisionConflict:
        """Update a conflict's status (resolve or mark false positive)."""
        data = await self._patch(
            f"/v1/conflicts/{conflict_id}",
            req.model_dump(exclude_none=True),
        )
        return DecisionConflict.model_validate(data)

    async def list_conflict_groups(
        self,
        *,
        decision_type: str | None = None,
        agent_id: str | None = None,
        conflict_kind: str | None = None,
        status: str | None = None,
        limit: int = 25,
        offset: int = 0,
    ) -> list[ConflictGroup]:
        """List conflict groups."""
        params: dict[str, str] = {"limit": str(limit), "offset": str(offset)}
        if decision_type:
            params["decision_type"] = decision_type
        if agent_id:
            params["agent_id"] = agent_id
        if conflict_kind:
            params["conflict_kind"] = conflict_kind
        if status:
            params["status"] = status
        items, _ = await self._get_list("/v1/conflict-groups", params=params)
        return [ConflictGroup.model_validate(g) for g in items]

    async def resolve_conflict_group(
        self,
        group_id: UUID,
        req: ResolveConflictGroupRequest,
    ) -> ResolveConflictGroupResponse:
        """Resolve all open conflicts in a group."""
        data = await self._patch(
            f"/v1/conflict-groups/{group_id}/resolve",
            req.model_dump(exclude_none=True),
        )
        return ResolveConflictGroupResponse.model_validate(data)

    async def get_conflict_analytics(
        self,
        *,
        period: str | None = None,
        from_time: datetime | None = None,
        to_time: datetime | None = None,
        agent_id: str | None = None,
        decision_type: str | None = None,
        conflict_kind: str | None = None,
    ) -> ConflictAnalyticsResponse:
        """Get conflict analytics summary."""
        params: dict[str, str] = {}
        if period:
            params["period"] = period
        if from_time:
            params["from"] = from_time.isoformat()
        if to_time:
            params["to"] = to_time.isoformat()
        if agent_id:
            params["agent_id"] = agent_id
        if decision_type:
            params["decision_type"] = decision_type
        if conflict_kind:
            params["conflict_kind"] = conflict_kind
        data = await self._get("/v1/conflicts/analytics", params=params if params else None)
        return ConflictAnalyticsResponse.model_validate(data)

    # --- Phase 3: API key management ---

    async def create_key(self, req: CreateKeyRequest) -> APIKeyWithRawKey:
        """Create a new API key."""
        data = await self._post("/v1/keys", req.model_dump(exclude_none=True))
        return APIKeyWithRawKey.model_validate(data)

    async def list_keys(self, *, limit: int = 50, offset: int = 0) -> list[APIKey]:
        """List API keys for the organization."""
        items, _ = await self._get_list("/v1/keys", params={"limit": str(limit), "offset": str(offset)})
        return [APIKey.model_validate(k) for k in items]

    async def revoke_key(self, key_id: UUID) -> None:
        """Revoke an API key."""
        await self._delete(f"/v1/keys/{key_id}")

    async def rotate_key(self, key_id: UUID) -> RotateKeyResponse:
        """Rotate an API key (revoke old, create new)."""
        data = await self._post(f"/v1/keys/{key_id}/rotate", {})
        return RotateKeyResponse.model_validate(data)

    # --- Phase 3: Org settings ---

    async def get_org_settings(self) -> OrgSettingsData:
        """Get organization settings."""
        data = await self._get("/v1/org/settings")
        return OrgSettingsData.model_validate(data)

    async def set_org_settings(self, req: OrgSettingsData) -> OrgSettingsData:
        """Update organization settings."""
        data = await self._put("/v1/org/settings", req.model_dump())
        return OrgSettingsData.model_validate(data)

    # --- Phase 3: Retention ---

    async def get_retention(self) -> RetentionPolicy:
        """Get the retention policy."""
        data = await self._get("/v1/retention")
        return RetentionPolicy.model_validate(data)

    async def set_retention(self, req: SetRetentionRequest) -> RetentionPolicy:
        """Update the retention policy."""
        data = await self._put("/v1/retention", req.model_dump())
        return RetentionPolicy.model_validate(data)

    async def purge_decisions(self, req: PurgeRequest) -> PurgeResponse:
        """Purge decisions matching criteria (supports dry_run)."""
        data = await self._post("/v1/retention/purge", req.model_dump(exclude_none=True))
        return PurgeResponse.model_validate(data)

    async def create_hold(self, req: CreateHoldRequest) -> RetentionHold:
        """Create a retention hold to prevent purging."""
        data = await self._post(
            "/v1/retention/hold",
            req.model_dump(by_alias=True, exclude_none=True),
        )
        return RetentionHold.model_validate(data)

    async def release_hold(self, hold_id: UUID) -> None:
        """Release a retention hold."""
        await self._delete(f"/v1/retention/hold/{hold_id}")

    # --- Phase 3: Project links ---

    async def create_project_link(self, req: CreateProjectLinkRequest) -> ProjectLink:
        """Create a link between two projects."""
        data = await self._post("/v1/project-links", req.model_dump())
        return ProjectLink.model_validate(data)

    async def list_project_links(self, *, limit: int = 50, offset: int = 0) -> list[ProjectLink]:
        """List project links."""
        items, _ = await self._get_list(
            "/v1/project-links",
            params={"limit": str(limit), "offset": str(offset)},
        )
        return [ProjectLink.model_validate(p) for p in items]

    async def delete_project_link(self, link_id: UUID) -> None:
        """Delete a project link."""
        await self._delete(f"/v1/project-links/{link_id}")

    async def grant_all_project_links(self, link_type: str = "") -> dict[str, Any]:
        """Grant cross-project access for all linked projects."""
        return await self._post(
            "/v1/project-links/grant-all",
            {"link_type": link_type} if link_type else {},
        )

    # --- Phase 3: Integrity, trace health, usage ---

    async def list_integrity_violations(self, *, limit: int = 50) -> IntegrityViolationsResponse:
        """List integrity violations."""
        data = await self._get("/v1/integrity/violations", params={"limit": str(limit)})
        return IntegrityViolationsResponse.model_validate(data)

    async def get_trace_health(
        self,
        *,
        from_time: datetime | None = None,
        to_time: datetime | None = None,
    ) -> TraceHealthResponse:
        """Get trace health metrics."""
        params: dict[str, str] = {}
        if from_time:
            params["from"] = from_time.isoformat()
        if to_time:
            params["to"] = to_time.isoformat()
        data = await self._get("/v1/trace-health", params=params if params else None)
        return TraceHealthResponse.model_validate(data)

    async def get_usage(self, *, period: str = "") -> UsageResponse:
        """Get usage statistics."""
        params = {"period": period} if period else None
        data = await self._get("/v1/usage", params=params)
        return UsageResponse.model_validate(data)

    # --- Phase 3: Auth ---

    async def scoped_token(self, req: ScopedTokenRequest) -> ScopedTokenResponse:
        """Create a scoped token for delegated access."""
        data = await self._post("/v1/auth/scoped-token", req.model_dump())
        return ScopedTokenResponse.model_validate(data)

    async def signup(self, req: SignupRequest) -> SignupResponse:
        """Sign up a new organization (no auth required)."""
        data = await self._post_no_auth("/auth/signup", req.model_dump())
        return SignupResponse.model_validate(data)

    async def get_config(self) -> ConfigResponse:
        """Get server configuration (no auth required)."""
        data = await self._get_no_auth("/config")
        return ConfigResponse.model_validate(data)

    # --- Phase 4: Agent management ---

    async def get_agent(self, agent_id: str) -> Agent:
        """Get a single agent by ID."""
        data = await self._get(f"/v1/agents/{agent_id}")
        return Agent.model_validate(data)

    async def update_agent(self, agent_id: str, req: UpdateAgentRequest) -> Agent:
        """Update an agent's name or metadata."""
        data = await self._patch(f"/v1/agents/{agent_id}", req.model_dump(exclude_none=True))
        return Agent.model_validate(data)

    async def get_agent_stats(self, agent_id: str) -> AgentStatsResponse:
        """Get statistics for a specific agent."""
        data = await self._get(f"/v1/agents/{agent_id}/stats")
        return AgentStatsResponse.model_validate(data)

    # --- Phase 4: Grants ---

    async def list_grants(self, *, limit: int = 50, offset: int = 0) -> list[Grant]:
        """List all grants in the organization."""
        items, _ = await self._get_list(
            "/v1/grants",
            params={"limit": str(limit), "offset": str(offset)},
        )
        return [Grant.model_validate(g) for g in items]

    # --- Phase 4: Sessions ---

    async def get_session_view(self, session_id: UUID) -> SessionViewResponse:
        """Get a session with its decisions and summary."""
        data = await self._get(f"/v1/sessions/{session_id}")
        return SessionViewResponse.model_validate(data)

    # --- Admin: conflict validation, evaluation, and labels ---

    async def validate_pair(self, req: ValidatePairRequest) -> ValidatePairResponse:
        """Validate the relationship between two decision outcomes (admin-only)."""
        data = await self._post("/v1/admin/conflicts/validate-pair", req.model_dump(exclude_none=True))
        return ValidatePairResponse.model_validate(data)

    async def conflict_eval(self) -> ConflictEvalResponse:
        """Run the conflict evaluation suite against labeled conflicts (admin-only)."""
        data = await self._post("/v1/admin/conflicts/eval", {})
        return ConflictEvalResponse.model_validate(data)

    async def upsert_conflict_label(self, conflict_id: UUID, req: UpsertConflictLabelRequest) -> ConflictLabelRecord:
        """Create or update a human label on a scored conflict (admin-only)."""
        data = await self._put(f"/v1/admin/conflicts/{conflict_id}/label", req.model_dump(exclude_none=True))
        return ConflictLabelRecord.model_validate(data)

    async def get_conflict_label(self, conflict_id: UUID) -> ConflictLabelRecord:
        """Get the human label for a scored conflict (admin-only)."""
        data = await self._get(f"/v1/admin/conflicts/{conflict_id}/label")
        return ConflictLabelRecord.model_validate(data)

    async def delete_conflict_label(self, conflict_id: UUID) -> None:
        """Delete the human label from a scored conflict (admin-only)."""
        await self._delete(f"/v1/admin/conflicts/{conflict_id}/label")

    async def list_conflict_labels(self) -> ListConflictLabelsResponse:
        """List all conflict labels with aggregate counts (admin-only)."""
        data = await self._get("/v1/admin/conflict-labels")
        return ListConflictLabelsResponse.model_validate(data)

    async def scorer_eval(self) -> ScorerEvalResponse:
        """Evaluate the conflict scorer's precision using human labels (admin-only)."""
        data = await self._post("/v1/admin/scorer-eval", {})
        return ScorerEvalResponse.model_validate(data)

    async def export_decisions(
        self,
        *,
        agent_id: str | None = None,
        decision_type: str | None = None,
        from_time: datetime | None = None,
        to_time: datetime | None = None,
    ) -> AsyncIterator[Decision]:
        """Stream decisions as NDJSON (admin-only). Yields Decision objects."""
        params: dict[str, str] = {}
        if agent_id:
            params["agent_id"] = agent_id
        if decision_type:
            params["decision_type"] = decision_type
        if from_time:
            params["from"] = from_time.isoformat()
        if to_time:
            params["to"] = to_time.isoformat()

        token = await self._token_mgr.get_token(self._client)
        headers = {
            "Authorization": f"Bearer {token}",
            "User-Agent": _USER_AGENT,
            "X-Akashi-Session": str(self.session_id),
        }
        async with self._client.stream(
            "GET",
            f"{self.base_url}/v1/export/decisions",
            params=params if params else None,
            headers=headers,
        ) as resp:
            if resp.status_code >= 400:
                await resp.aread()
                _check_response_size(resp)
                _handle_response(resp)  # raises
            async for line in resp.aiter_lines():
                if not line.strip():
                    continue
                data = _json.loads(line)
                if data.get("__error"):
                    raise ServerError(data.get("message", "Export terminated due to internal error"))
                yield Decision.model_validate(data)

    async def subscribe(self) -> AsyncIterator[SubscriptionEvent]:
        """Open an SSE connection to ``GET /v1/subscribe`` and yield real-time events.

        Yields :class:`SubscriptionEvent` instances for decision and conflict
        notifications scoped to the caller's organization. Keepalive comments
        from the server are silently consumed.

        The connection stays open until the caller breaks out of the iterator
        or the server closes it.
        """
        token = await self._token_mgr.get_token(self._client)
        headers = {
            "Authorization": f"Bearer {token}",
            "User-Agent": _USER_AGENT,
            "X-Akashi-Session": str(self.session_id),
            "Accept": "text/event-stream",
        }
        async with self._client.stream(
            "GET",
            f"{self.base_url}/v1/subscribe",
            headers=headers,
            timeout=None,
        ) as resp:
            if resp.status_code >= 400:
                await resp.aread()
                _check_response_size(resp)
                _handle_response(resp)  # raises
            event_type = ""
            data_buf: list[str] = []
            async for raw_line in resp.aiter_lines():
                line = raw_line.rstrip("\n")
                # SSE comment (keepalive).
                if line.startswith(":"):
                    continue
                # Empty line = end of event.
                if line == "":
                    if event_type and data_buf:
                        payload = _json.loads("\n".join(data_buf))
                        yield SubscriptionEvent(event_type=event_type, data=payload)
                    event_type = ""
                    data_buf = []
                    continue
                if line.startswith("event: "):
                    event_type = line[7:]
                elif line.startswith("data: "):
                    data_buf.append(line[6:])

    # --- HTTP transport ---

    async def _post(self, path: str, body: dict[str, Any], extra_headers: dict[str, str] | None = None) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        if extra_headers:
            headers.update(extra_headers)
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = await self._token_mgr.get_token(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = await self._client.post(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    async def _post_list(self, path: str, body: dict[str, Any], extra_headers: dict[str, str] | None = None) -> tuple[list[Any], dict[str, Any]]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        if extra_headers:
            headers.update(extra_headers)
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = await self._token_mgr.get_token(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = await self._client.post(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_list_body(resp)
        raise last_err  # type: ignore[misc]

    async def _get(self, path: str, *, params: dict[str, str] | None = None) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = await self._token_mgr.get_token(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = await self._client.get(
                    f"{self.base_url}{path}",
                    params=params,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    async def _get_list(self, path: str, *, params: dict[str, str] | None = None) -> tuple[list[Any], dict[str, Any]]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = await self._token_mgr.get_token(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = await self._client.get(
                    f"{self.base_url}{path}",
                    params=params,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_list_body(resp)
        raise last_err  # type: ignore[misc]

    async def _patch(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = await self._token_mgr.get_token(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = await self._client.patch(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    async def _put(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = await self._token_mgr.get_token(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = await self._client.put(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    async def _delete(self, path: str) -> None:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = await self._token_mgr.get_token(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = await self._client.delete(
                    f"{self.base_url}{path}",
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            _handle_no_content(resp)
            return
        raise last_err  # type: ignore[misc]

    async def _delete_with_body(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = await self._token_mgr.get_token(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = await self._client.request(
                    "DELETE",
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    async def _get_no_auth(self, path: str) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            try:
                resp = await self._client.get(
                    f"{self.base_url}{path}",
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    async def _post_no_auth(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            try:
                resp = await self._client.post(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    await asyncio.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                await asyncio.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]


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
        max_retries: int = DEFAULT_MAX_RETRIES,
        retry_base_delay: float = DEFAULT_RETRY_BASE_DELAY,
        session_id: UUID | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.agent_id = agent_id
        self.session_id: UUID = session_id if session_id is not None else uuid4()
        self._max_retries = max_retries
        self._retry_base_delay = retry_base_delay
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
        project: str | None = None,
        limit: int = 5,
        format: str | None = None,
    ) -> CheckResponse:
        """Check for existing decisions before making a new one."""
        data = self._post("/v1/check", _build_check_body(decision_type, query, agent_id, limit, project, format))
        return CheckResponse.model_validate(data)

    def trace(self, request: TraceRequest, *, idempotency_key: str | None = None) -> TraceResponse:
        """Record a decision trace."""
        idem_key = idempotency_key or str(uuid4())
        data = self._post(
            "/v1/trace",
            _build_trace_body(self.agent_id, request),
            extra_headers={"X-Idempotency-Key": idem_key},
        )
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
        items, meta = self._post_list("/v1/query", _build_query_body(filters, limit, offset, order_by, order_dir))
        return QueryResponse(
            decisions=[Decision.model_validate(d) for d in items],
            total=meta.get("total") or 0,
            has_more=meta.get("has_more", False),
            limit=meta.get("limit", 0),
            offset=meta.get("offset", 0),
        )

    def search(self, query: str, *, limit: int = 5, semantic: bool = False) -> SearchResponse:
        """Search decision history by semantic similarity."""
        items, meta = self._post_list("/v1/search", _build_search_body(query, limit, semantic))
        return SearchResponse(
            results=[SearchResult.model_validate(r) for r in items],
            total=meta.get("total") or len(items),
        )

    def recent(
        self,
        *,
        limit: int = 10,
        agent_id: str | None = None,
        decision_type: str | None = None,
    ) -> list[Decision]:
        """Get the most recent decisions."""
        items, _ = self._get_list("/v1/decisions/recent", params=_build_recent_params(limit, agent_id, decision_type))
        return [Decision.model_validate(d) for d in items]

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

    def append_events(
        self, run_id: UUID, events: list[EventInput], *, idempotency_key: str | None = None,
    ) -> None:
        """Append events to an existing run."""
        idem_key = idempotency_key or str(uuid4())
        self._post(
            f"/v1/runs/{run_id}/events",
            _build_append_events_body(events),
            extra_headers={"X-Idempotency-Key": idem_key},
        )

    def complete_run(
        self,
        run_id: UUID,
        status: str,
        *,
        metadata: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> AgentRun:
        """Mark a run as completed or failed."""
        idem_key = idempotency_key or str(uuid4())
        req = CompleteRunRequest(status=status, metadata=metadata or {})
        data = self._post(
            f"/v1/runs/{run_id}/complete",
            _build_complete_run_body(req),
            extra_headers={"X-Idempotency-Key": idem_key},
        )
        return AgentRun.model_validate(data)

    def get_run(self, run_id: UUID) -> GetRunResponse:
        """Get a run with its events and decisions."""
        data = self._get(f"/v1/runs/{run_id}")
        return GetRunResponse(
            run=AgentRun.model_validate(data["run"]),
            events=[AgentEvent.model_validate(e) for e in data.get("events", [])],
            decisions=[Decision.model_validate(d) for d in data.get("decisions", [])],
        )

    # --- Agents (admin-only) ---

    def create_agent(self, req: CreateAgentRequest) -> Agent:
        """Create a new agent (admin-only)."""
        data = self._post("/v1/agents", _build_create_agent_body(req))
        return Agent.model_validate(data)

    def list_agents(self) -> list[Agent]:
        """List all agents in the organization (admin-only)."""
        data = self._get("/v1/agents")
        return [Agent.model_validate(a) for a in (data if isinstance(data, list) else [])]

    def delete_agent(self, agent_id: str) -> None:
        """Delete an agent and all associated data (admin-only)."""
        self._delete(f"/v1/agents/{agent_id}")

    def update_agent_tags(self, agent_id: str, tags: list[str]) -> Agent:
        """Replace an agent's tags (admin-only)."""
        data = self._patch(f"/v1/agents/{agent_id}/tags", {"tags": tags})
        return Agent.model_validate(data)

    # --- Integrity ---

    def verify_decision(self, decision_id: UUID) -> VerifyResponse:
        """Verify the integrity of a decision by recomputing its content hash."""
        data = self._get(f"/v1/verify/{decision_id}")
        return VerifyResponse.model_validate(data)

    def get_decision_revisions(self, decision_id: UUID) -> RevisionsResponse:
        """Get the full revision chain for a decision."""
        data = self._get(f"/v1/decisions/{decision_id}/revisions")
        return RevisionsResponse.model_validate(data)

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
        items, _ = self._get_list(
            f"/v1/agents/{agent_id}/history",
            params=_build_agent_history_params(limit),
        )
        return [Decision.model_validate(d) for d in items]

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
        agent_id: str | None = None,
        conflict_kind: str | None = None,
        limit: int = 25,
        offset: int = 0,
    ) -> list[DecisionConflict]:
        """List detected decision conflicts."""
        items, _ = self._get_list(
            "/v1/conflicts",
            params=_build_conflicts_params(
                decision_type, agent_id, conflict_kind, limit, offset
            ),
        )
        return [DecisionConflict.model_validate(c) for c in items]

    # --- Assessments (spec 29) ---

    def assess(self, decision_id: UUID, req: AssessRequest) -> AssessResponse:
        """Record an outcome assessment for a prior decision.

        Assessments are append-only — each call creates a new row. An assessor
        changing their verdict over time is itself an auditable event; prior
        assessments are never overwritten.
        """
        data = self._post(f"/v1/decisions/{decision_id}/assess", _build_assess_body(req))
        return AssessResponse.model_validate(data)

    def list_assessments(self, decision_id: UUID) -> list[AssessResponse]:
        """Return the full assessment history for a decision, newest first."""
        items, _ = self._get_list(f"/v1/decisions/{decision_id}/assessments")
        return [AssessResponse.model_validate(a) for a in items]

    # --- Health (no auth) ---

    def health(self) -> HealthResponse:
        """Check server health. Does not require authentication."""
        data = self._get_no_auth("/health")
        return HealthResponse.model_validate(data)

    # --- Phase 2: Decision details ---

    def get_decision(self, decision_id: UUID) -> Decision:
        """Get a single decision by ID."""
        data = self._get(f"/v1/decisions/{decision_id}")
        return Decision.model_validate(data)

    def get_decision_conflicts(
        self,
        decision_id: UUID,
        *,
        status: str | None = None,
        limit: int = 50,
        offset: int = 0,
    ) -> list[DecisionConflict]:
        """List conflicts for a specific decision."""
        params: dict[str, str] = {"limit": str(limit), "offset": str(offset)}
        if status:
            params["status"] = status
        items, _ = self._get_list(f"/v1/decisions/{decision_id}/conflicts", params=params)
        return [DecisionConflict.model_validate(c) for c in items]

    def get_decision_lineage(self, decision_id: UUID) -> LineageResponse:
        """Get the precedent lineage for a decision."""
        data = self._get(f"/v1/decisions/{decision_id}/lineage")
        return LineageResponse.model_validate(data)

    def get_decision_timeline(
        self,
        *,
        granularity: str = "day",
        from_time: datetime | None = None,
        to_time: datetime | None = None,
        agent_id: str | None = None,
        project: str | None = None,
    ) -> TimelineResponse:
        """Get a bucketed timeline of decisions."""
        params: dict[str, str] = {"granularity": granularity}
        if from_time:
            params["from"] = from_time.isoformat()
        if to_time:
            params["to"] = to_time.isoformat()
        if agent_id:
            params["agent_id"] = agent_id
        if project:
            params["project"] = project
        data = self._get("/v1/decisions/timeline", params=params)
        return TimelineResponse.model_validate(data)

    def get_decision_facets(self) -> FacetsResponse:
        """Get available decision type and project facets."""
        data = self._get("/v1/decisions/facets")
        return FacetsResponse.model_validate(data)

    def retract_decision(self, decision_id: UUID, reason: str = "") -> Decision:
        """Retract (soft-delete) a decision."""
        data = self._delete_with_body(
            f"/v1/decisions/{decision_id}",
            {"reason": reason} if reason else {},
        )
        return Decision.model_validate(data)

    def patch_decision(self, decision_id: UUID, *, project: str | None = None) -> Decision:
        """Update mutable metadata on a decision (e.g. project)."""
        body: dict[str, Any] = {}
        if project is not None:
            body["project"] = project
        data = self._patch(f"/v1/decisions/{decision_id}", body)
        return Decision.model_validate(data)

    def erase_decision(self, decision_id: UUID, reason: str = "") -> EraseDecisionResponse:
        """GDPR-erase a decision (irreversible)."""
        data = self._post(
            f"/v1/decisions/{decision_id}/erase",
            {"reason": reason} if reason else {},
        )
        return EraseDecisionResponse.model_validate(data)

    # --- Phase 2: Conflict management ---

    def get_conflict(self, conflict_id: UUID) -> ConflictDetail:
        """Get a single conflict with recommendation."""
        data = self._get(f"/v1/conflicts/{conflict_id}")
        return ConflictDetail.model_validate(data)

    def adjudicate_conflict(self, conflict_id: UUID, req: AdjudicateConflictRequest) -> ConflictDetail:
        """Adjudicate a conflict by recording a resolution decision."""
        data = self._post(
            f"/v1/conflicts/{conflict_id}/adjudicate",
            req.model_dump(exclude_none=True),
        )
        return ConflictDetail.model_validate(data)

    def patch_conflict(self, conflict_id: UUID, req: ConflictStatusUpdate) -> DecisionConflict:
        """Update a conflict's status (resolve or mark false positive)."""
        data = self._patch(
            f"/v1/conflicts/{conflict_id}",
            req.model_dump(exclude_none=True),
        )
        return DecisionConflict.model_validate(data)

    def list_conflict_groups(
        self,
        *,
        decision_type: str | None = None,
        agent_id: str | None = None,
        conflict_kind: str | None = None,
        status: str | None = None,
        limit: int = 25,
        offset: int = 0,
    ) -> list[ConflictGroup]:
        """List conflict groups."""
        params: dict[str, str] = {"limit": str(limit), "offset": str(offset)}
        if decision_type:
            params["decision_type"] = decision_type
        if agent_id:
            params["agent_id"] = agent_id
        if conflict_kind:
            params["conflict_kind"] = conflict_kind
        if status:
            params["status"] = status
        items, _ = self._get_list("/v1/conflict-groups", params=params)
        return [ConflictGroup.model_validate(g) for g in items]

    def resolve_conflict_group(
        self,
        group_id: UUID,
        req: ResolveConflictGroupRequest,
    ) -> ResolveConflictGroupResponse:
        """Resolve all open conflicts in a group."""
        data = self._patch(
            f"/v1/conflict-groups/{group_id}/resolve",
            req.model_dump(exclude_none=True),
        )
        return ResolveConflictGroupResponse.model_validate(data)

    def get_conflict_analytics(
        self,
        *,
        period: str | None = None,
        from_time: datetime | None = None,
        to_time: datetime | None = None,
        agent_id: str | None = None,
        decision_type: str | None = None,
        conflict_kind: str | None = None,
    ) -> ConflictAnalyticsResponse:
        """Get conflict analytics summary."""
        params: dict[str, str] = {}
        if period:
            params["period"] = period
        if from_time:
            params["from"] = from_time.isoformat()
        if to_time:
            params["to"] = to_time.isoformat()
        if agent_id:
            params["agent_id"] = agent_id
        if decision_type:
            params["decision_type"] = decision_type
        if conflict_kind:
            params["conflict_kind"] = conflict_kind
        data = self._get("/v1/conflicts/analytics", params=params if params else None)
        return ConflictAnalyticsResponse.model_validate(data)

    # --- Phase 3: API key management ---

    def create_key(self, req: CreateKeyRequest) -> APIKeyWithRawKey:
        """Create a new API key."""
        data = self._post("/v1/keys", req.model_dump(exclude_none=True))
        return APIKeyWithRawKey.model_validate(data)

    def list_keys(self, *, limit: int = 50, offset: int = 0) -> list[APIKey]:
        """List API keys for the organization."""
        items, _ = self._get_list("/v1/keys", params={"limit": str(limit), "offset": str(offset)})
        return [APIKey.model_validate(k) for k in items]

    def revoke_key(self, key_id: UUID) -> None:
        """Revoke an API key."""
        self._delete(f"/v1/keys/{key_id}")

    def rotate_key(self, key_id: UUID) -> RotateKeyResponse:
        """Rotate an API key (revoke old, create new)."""
        data = self._post(f"/v1/keys/{key_id}/rotate", {})
        return RotateKeyResponse.model_validate(data)

    # --- Phase 3: Org settings ---

    def get_org_settings(self) -> OrgSettingsData:
        """Get organization settings."""
        data = self._get("/v1/org/settings")
        return OrgSettingsData.model_validate(data)

    def set_org_settings(self, req: OrgSettingsData) -> OrgSettingsData:
        """Update organization settings."""
        data = self._put("/v1/org/settings", req.model_dump())
        return OrgSettingsData.model_validate(data)

    # --- Phase 3: Retention ---

    def get_retention(self) -> RetentionPolicy:
        """Get the retention policy."""
        data = self._get("/v1/retention")
        return RetentionPolicy.model_validate(data)

    def set_retention(self, req: SetRetentionRequest) -> RetentionPolicy:
        """Update the retention policy."""
        data = self._put("/v1/retention", req.model_dump())
        return RetentionPolicy.model_validate(data)

    def purge_decisions(self, req: PurgeRequest) -> PurgeResponse:
        """Purge decisions matching criteria (supports dry_run)."""
        data = self._post("/v1/retention/purge", req.model_dump(exclude_none=True))
        return PurgeResponse.model_validate(data)

    def create_hold(self, req: CreateHoldRequest) -> RetentionHold:
        """Create a retention hold to prevent purging."""
        data = self._post(
            "/v1/retention/hold",
            req.model_dump(by_alias=True, exclude_none=True),
        )
        return RetentionHold.model_validate(data)

    def release_hold(self, hold_id: UUID) -> None:
        """Release a retention hold."""
        self._delete(f"/v1/retention/hold/{hold_id}")

    # --- Phase 3: Project links ---

    def create_project_link(self, req: CreateProjectLinkRequest) -> ProjectLink:
        """Create a link between two projects."""
        data = self._post("/v1/project-links", req.model_dump())
        return ProjectLink.model_validate(data)

    def list_project_links(self, *, limit: int = 50, offset: int = 0) -> list[ProjectLink]:
        """List project links."""
        items, _ = self._get_list(
            "/v1/project-links",
            params={"limit": str(limit), "offset": str(offset)},
        )
        return [ProjectLink.model_validate(p) for p in items]

    def delete_project_link(self, link_id: UUID) -> None:
        """Delete a project link."""
        self._delete(f"/v1/project-links/{link_id}")

    def grant_all_project_links(self, link_type: str = "") -> dict[str, Any]:
        """Grant cross-project access for all linked projects."""
        return self._post(
            "/v1/project-links/grant-all",
            {"link_type": link_type} if link_type else {},
        )

    # --- Phase 3: Integrity, trace health, usage ---

    def list_integrity_violations(self, *, limit: int = 50) -> IntegrityViolationsResponse:
        """List integrity violations."""
        data = self._get("/v1/integrity/violations", params={"limit": str(limit)})
        return IntegrityViolationsResponse.model_validate(data)

    def get_trace_health(
        self,
        *,
        from_time: datetime | None = None,
        to_time: datetime | None = None,
    ) -> TraceHealthResponse:
        """Get trace health metrics."""
        params: dict[str, str] = {}
        if from_time:
            params["from"] = from_time.isoformat()
        if to_time:
            params["to"] = to_time.isoformat()
        data = self._get("/v1/trace-health", params=params if params else None)
        return TraceHealthResponse.model_validate(data)

    def get_usage(self, *, period: str = "") -> UsageResponse:
        """Get usage statistics."""
        params = {"period": period} if period else None
        data = self._get("/v1/usage", params=params)
        return UsageResponse.model_validate(data)

    # --- Phase 3: Auth ---

    def scoped_token(self, req: ScopedTokenRequest) -> ScopedTokenResponse:
        """Create a scoped token for delegated access."""
        data = self._post("/v1/auth/scoped-token", req.model_dump())
        return ScopedTokenResponse.model_validate(data)

    def signup(self, req: SignupRequest) -> SignupResponse:
        """Sign up a new organization (no auth required)."""
        data = self._post_no_auth("/auth/signup", req.model_dump())
        return SignupResponse.model_validate(data)

    def get_config(self) -> ConfigResponse:
        """Get server configuration (no auth required)."""
        data = self._get_no_auth("/config")
        return ConfigResponse.model_validate(data)

    # --- Phase 4: Agent management ---

    def get_agent(self, agent_id: str) -> Agent:
        """Get a single agent by ID."""
        data = self._get(f"/v1/agents/{agent_id}")
        return Agent.model_validate(data)

    def update_agent(self, agent_id: str, req: UpdateAgentRequest) -> Agent:
        """Update an agent's name or metadata."""
        data = self._patch(f"/v1/agents/{agent_id}", req.model_dump(exclude_none=True))
        return Agent.model_validate(data)

    def get_agent_stats(self, agent_id: str) -> AgentStatsResponse:
        """Get statistics for a specific agent."""
        data = self._get(f"/v1/agents/{agent_id}/stats")
        return AgentStatsResponse.model_validate(data)

    # --- Phase 4: Grants ---

    def list_grants(self, *, limit: int = 50, offset: int = 0) -> list[Grant]:
        """List all grants in the organization."""
        items, _ = self._get_list(
            "/v1/grants",
            params={"limit": str(limit), "offset": str(offset)},
        )
        return [Grant.model_validate(g) for g in items]

    # --- Phase 4: Sessions ---

    def get_session_view(self, session_id: UUID) -> SessionViewResponse:
        """Get a session with its decisions and summary."""
        data = self._get(f"/v1/sessions/{session_id}")
        return SessionViewResponse.model_validate(data)

    # --- Admin: conflict validation, evaluation, and labels ---

    def validate_pair(self, req: ValidatePairRequest) -> ValidatePairResponse:
        """Validate the relationship between two decision outcomes (admin-only)."""
        data = self._post("/v1/admin/conflicts/validate-pair", req.model_dump(exclude_none=True))
        return ValidatePairResponse.model_validate(data)

    def conflict_eval(self) -> ConflictEvalResponse:
        """Run the conflict evaluation suite against labeled conflicts (admin-only)."""
        data = self._post("/v1/admin/conflicts/eval", {})
        return ConflictEvalResponse.model_validate(data)

    def upsert_conflict_label(self, conflict_id: UUID, req: UpsertConflictLabelRequest) -> ConflictLabelRecord:
        """Create or update a human label on a scored conflict (admin-only)."""
        data = self._put(f"/v1/admin/conflicts/{conflict_id}/label", req.model_dump(exclude_none=True))
        return ConflictLabelRecord.model_validate(data)

    def get_conflict_label(self, conflict_id: UUID) -> ConflictLabelRecord:
        """Get the human label for a scored conflict (admin-only)."""
        data = self._get(f"/v1/admin/conflicts/{conflict_id}/label")
        return ConflictLabelRecord.model_validate(data)

    def delete_conflict_label(self, conflict_id: UUID) -> None:
        """Delete the human label from a scored conflict (admin-only)."""
        self._delete(f"/v1/admin/conflicts/{conflict_id}/label")

    def list_conflict_labels(self) -> ListConflictLabelsResponse:
        """List all conflict labels with aggregate counts (admin-only)."""
        data = self._get("/v1/admin/conflict-labels")
        return ListConflictLabelsResponse.model_validate(data)

    def scorer_eval(self) -> ScorerEvalResponse:
        """Evaluate the conflict scorer's precision using human labels (admin-only)."""
        data = self._post("/v1/admin/scorer-eval", {})
        return ScorerEvalResponse.model_validate(data)

    def export_decisions(
        self,
        *,
        agent_id: str | None = None,
        decision_type: str | None = None,
        from_time: datetime | None = None,
        to_time: datetime | None = None,
    ) -> Iterator[Decision]:
        """Stream decisions as NDJSON (admin-only). Yields Decision objects."""
        params: dict[str, str] = {}
        if agent_id:
            params["agent_id"] = agent_id
        if decision_type:
            params["decision_type"] = decision_type
        if from_time:
            params["from"] = from_time.isoformat()
        if to_time:
            params["to"] = to_time.isoformat()

        token = self._token_mgr.get_token_sync(self._client)
        headers = {
            "Authorization": f"Bearer {token}",
            "User-Agent": _USER_AGENT,
            "X-Akashi-Session": str(self.session_id),
        }
        with self._client.stream(
            "GET",
            f"{self.base_url}/v1/export/decisions",
            params=params if params else None,
            headers=headers,
        ) as resp:
            if resp.status_code >= 400:
                resp.read()
                _check_response_size(resp)
                _handle_response(resp)  # raises
            for line in resp.iter_lines():
                if not line.strip():
                    continue
                data = _json.loads(line)
                if data.get("__error"):
                    raise ServerError(data.get("message", "Export terminated due to internal error"))
                yield Decision.model_validate(data)

    def subscribe(self) -> Iterator[SubscriptionEvent]:
        """Open an SSE connection to ``GET /v1/subscribe`` and yield real-time events.

        Yields :class:`SubscriptionEvent` instances for decision and conflict
        notifications scoped to the caller's organization. Keepalive comments
        from the server are silently consumed.

        The connection stays open until the caller breaks out of the iterator
        or the server closes it.
        """
        token = self._token_mgr.get_token_sync(self._client)
        headers = {
            "Authorization": f"Bearer {token}",
            "User-Agent": _USER_AGENT,
            "X-Akashi-Session": str(self.session_id),
            "Accept": "text/event-stream",
        }
        with self._client.stream(
            "GET",
            f"{self.base_url}/v1/subscribe",
            headers=headers,
            timeout=None,
        ) as resp:
            if resp.status_code >= 400:
                resp.read()
                _check_response_size(resp)
                _handle_response(resp)  # raises
            event_type = ""
            data_buf: list[str] = []
            for raw_line in resp.iter_lines():
                line = raw_line.rstrip("\n")
                # SSE comment (keepalive).
                if line.startswith(":"):
                    continue
                # Empty line = end of event.
                if line == "":
                    if event_type and data_buf:
                        payload = _json.loads("\n".join(data_buf))
                        yield SubscriptionEvent(event_type=event_type, data=payload)
                    event_type = ""
                    data_buf = []
                    continue
                if line.startswith("event: "):
                    event_type = line[7:]
                elif line.startswith("data: "):
                    data_buf.append(line[6:])

    # --- HTTP transport ---

    def _post(self, path: str, body: dict[str, Any], extra_headers: dict[str, str] | None = None) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        if extra_headers:
            headers.update(extra_headers)
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = self._token_mgr.get_token_sync(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = self._client.post(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    def _post_list(self, path: str, body: dict[str, Any], extra_headers: dict[str, str] | None = None) -> tuple[list[Any], dict[str, Any]]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        if extra_headers:
            headers.update(extra_headers)
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = self._token_mgr.get_token_sync(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = self._client.post(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_list_body(resp)
        raise last_err  # type: ignore[misc]

    def _get(self, path: str, *, params: dict[str, str] | None = None) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = self._token_mgr.get_token_sync(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = self._client.get(
                    f"{self.base_url}{path}",
                    params=params,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    def _get_list(self, path: str, *, params: dict[str, str] | None = None) -> tuple[list[Any], dict[str, Any]]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = self._token_mgr.get_token_sync(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = self._client.get(
                    f"{self.base_url}{path}",
                    params=params,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_list_body(resp)
        raise last_err  # type: ignore[misc]

    def _patch(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = self._token_mgr.get_token_sync(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = self._client.patch(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    def _put(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = self._token_mgr.get_token_sync(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = self._client.put(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    def _delete(self, path: str) -> None:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = self._token_mgr.get_token_sync(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = self._client.delete(
                    f"{self.base_url}{path}",
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            _handle_no_content(resp)
            return
        raise last_err  # type: ignore[misc]

    def _delete_with_body(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            token = self._token_mgr.get_token_sync(self._client)
            headers["Authorization"] = f"Bearer {token}"
            try:
                resp = self._client.request(
                    "DELETE",
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    def _get_no_auth(self, path: str) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT, "X-Akashi-Session": str(self.session_id)}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            try:
                resp = self._client.get(
                    f"{self.base_url}{path}",
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]

    def _post_no_auth(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        headers = {"User-Agent": _USER_AGENT}
        last_err: Exception | None = None
        for attempt in range(self._max_retries + 1):
            try:
                resp = self._client.post(
                    f"{self.base_url}{path}",
                    json=body,
                    headers=headers,
                )
            except httpx.TransportError as exc:
                last_err = exc
                if attempt < self._max_retries:
                    _time.sleep(retry_delay(attempt, self._retry_base_delay))
                    continue
                raise
            if is_retryable_status(resp.status_code) and attempt < self._max_retries:
                ra = parse_retry_after(resp.headers.get("retry-after"))
                _time.sleep(retry_delay(attempt, self._retry_base_delay, ra))
                continue
            _check_response_size(resp)
            return _handle_response(resp)
        raise last_err  # type: ignore[misc]
