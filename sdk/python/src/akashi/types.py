"""Pydantic models mirroring the Akashi Go domain types."""

from __future__ import annotations

from datetime import datetime
from typing import Any
from uuid import UUID

from pydantic import BaseModel, Field


class Decision(BaseModel):
    """A recorded decision with bi-temporal modeling."""

    id: UUID
    run_id: UUID
    agent_id: str
    org_id: UUID
    decision_type: str
    outcome: str
    confidence: float = Field(ge=0.0, le=1.0)
    reasoning: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    quality_score: float = 0.0
    precedent_ref: UUID | None = None
    supersedes_id: UUID | None = None
    content_hash: str = ""
    tags: list[str] = Field(default_factory=list)
    valid_from: datetime
    valid_to: datetime | None = None
    transaction_time: datetime
    created_at: datetime
    alternatives: list[Alternative] = Field(default_factory=list)
    evidence: list[Evidence] = Field(default_factory=list)


class Alternative(BaseModel):
    """An option considered for a decision."""

    id: UUID
    decision_id: UUID
    label: str
    score: float | None = None
    selected: bool
    rejection_reason: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class Evidence(BaseModel):
    """Supporting information for a decision."""

    id: UUID
    decision_id: UUID
    source_type: str
    source_uri: str | None = None
    content: str
    relevance_score: float | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class DecisionConflict(BaseModel):
    """A detected conflict between two decisions."""

    decision_a_id: UUID
    decision_b_id: UUID
    org_id: UUID
    agent_a: str
    agent_b: str
    run_a: UUID
    run_b: UUID
    decision_type: str
    outcome_a: str
    outcome_b: str
    confidence_a: float
    confidence_b: float
    reasoning_a: str | None = None
    reasoning_b: str | None = None
    decided_at_a: datetime
    decided_at_b: datetime
    detected_at: datetime


class AgentRun(BaseModel):
    """An agent run — top-level execution context corresponding to an OTEL trace."""

    id: UUID
    agent_id: str
    org_id: UUID
    trace_id: str | None = None
    parent_run_id: UUID | None = None
    status: str
    metadata: dict[str, Any] = Field(default_factory=dict)
    started_at: datetime
    completed_at: datetime | None = None
    created_at: datetime


class AgentEvent(BaseModel):
    """An append-only event in the event log."""

    id: UUID
    run_id: UUID
    org_id: UUID
    event_type: str
    sequence_num: int
    occurred_at: datetime
    agent_id: str
    trace_id: str = ""
    span_id: str = ""
    payload: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class Agent(BaseModel):
    """An agent identity with role assignment."""

    id: UUID
    agent_id: str
    org_id: UUID
    name: str
    role: str
    tags: list[str] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class Grant(BaseModel):
    """A fine-grained access grant between agents."""

    id: UUID
    grantor_id: UUID
    grantee_id: UUID
    resource_type: str
    resource_id: str | None = None
    permission: str
    expires_at: datetime | None = None
    granted_at: datetime


class HealthResponse(BaseModel):
    """Response from GET /health."""

    status: str
    version: str
    postgres: str
    qdrant: str = ""
    uptime_seconds: int


# --- Request types ---


class TraceRequest(BaseModel):
    """Request body for recording a decision."""

    decision_type: str
    outcome: str
    confidence: float = Field(ge=0.0, le=1.0)
    reasoning: str | None = None
    alternatives: list[TraceAlternative] = Field(default_factory=list)
    evidence: list[TraceEvidence] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(default_factory=dict)


class TraceAlternative(BaseModel):
    """An alternative in a trace request."""

    label: str
    score: float | None = None
    selected: bool = False
    rejection_reason: str | None = None


class TraceEvidence(BaseModel):
    """Evidence in a trace request."""

    source_type: str
    source_uri: str | None = None
    content: str
    relevance_score: float | None = None


class QueryFilters(BaseModel):
    """Structured filters for decision queries."""

    agent_id: list[str] | None = None
    decision_type: str | None = None
    confidence_min: float | None = Field(default=None, ge=0.0, le=1.0)
    outcome: str | None = None


class CheckRequest(BaseModel):
    """Request for checking precedents before making a decision."""

    decision_type: str
    query: str | None = None
    agent_id: str | None = None
    limit: int = Field(default=5, ge=1, le=100)


class CreateRunRequest(BaseModel):
    """Request body for creating an agent run."""

    trace_id: str | None = None
    parent_run_id: UUID | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class EventInput(BaseModel):
    """A single event to append to a run."""

    event_type: str
    occurred_at: datetime | None = None
    payload: dict[str, Any] = Field(default_factory=dict)


class CompleteRunRequest(BaseModel):
    """Request body for completing an agent run."""

    status: str
    metadata: dict[str, Any] = Field(default_factory=dict)


class CreateAgentRequest(BaseModel):
    """Request body for creating an agent (admin-only)."""

    agent_id: str
    name: str
    role: str
    api_key: str
    metadata: dict[str, Any] = Field(default_factory=dict)


class CreateGrantRequest(BaseModel):
    """Request body for creating an access grant."""

    grantee_agent_id: str
    resource_type: str
    resource_id: str | None = None
    permission: str
    expires_at: str | None = None


class TemporalQueryRequest(BaseModel):
    """Request body for a temporal (point-in-time) query."""

    as_of: datetime
    filters: QueryFilters = Field(default_factory=QueryFilters)


# --- Response types ---


class TraceResponse(BaseModel):
    """Response from recording a decision."""

    run_id: UUID
    decision_id: UUID
    event_count: int = 0


class CheckResponse(BaseModel):
    """Response from a precedent check."""

    has_precedent: bool
    decisions: list[Decision]
    conflicts: list[DecisionConflict] = Field(default_factory=list)


class QueryResponse(BaseModel):
    """Response from a structured query."""

    decisions: list[Decision]
    total: int
    count: int = 0
    limit: int
    offset: int = 0


class SearchResult(BaseModel):
    """A decision with its similarity score."""

    decision: Decision
    similarity_score: float


class SearchResponse(BaseModel):
    """Response from a semantic search."""

    results: list[SearchResult]
    total: int
