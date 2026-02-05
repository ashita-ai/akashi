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
    decision_type: str
    outcome: str
    confidence: float = Field(ge=0.0, le=1.0)
    reasoning: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
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
    agent_a: str
    agent_b: str
    run_a: UUID
    run_b: UUID
    decision_type: str
    outcome_a: str
    outcome_b: str
    confidence_a: float
    confidence_b: float
    decided_at_a: datetime
    decided_at_b: datetime
    detected_at: datetime


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
