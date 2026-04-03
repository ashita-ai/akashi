"""Pydantic models mirroring the Akashi Go domain types."""

from __future__ import annotations

from datetime import datetime
from enum import Enum
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
    completeness_score: float = 0.0
    outcome_score: float | None = None
    precedent_ref: UUID | None = None
    precedent_reason: str | None = None
    supersedes_id: UUID | None = None
    content_hash: str = ""
    tags: list[str] = Field(default_factory=list)
    session_id: UUID | None = None
    agent_context: dict[str, Any] = Field(default_factory=dict)
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
    metrics: dict[str, float] | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class ConflictKind(str, Enum):
    cross_agent = "cross_agent"
    self_contradiction = "self_contradiction"


class DecisionConflict(BaseModel):
    """A detected conflict between two decisions."""

    conflict_kind: ConflictKind
    decision_a_id: UUID
    decision_b_id: UUID
    org_id: UUID
    agent_a: str
    agent_b: str
    run_a: UUID
    run_b: UUID
    decision_type: str
    decision_type_a: str | None = None
    decision_type_b: str | None = None
    outcome_a: str
    outcome_b: str
    confidence_a: float
    confidence_b: float
    reasoning_a: str | None = None
    reasoning_b: str | None = None
    decided_at_a: datetime
    decided_at_b: datetime
    detected_at: datetime
    topic_similarity: float | None = None
    outcome_divergence: float | None = None
    significance: float | None = None
    scoring_method: str = ""


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
    updated_at: datetime


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
    buffer_depth: int = 0
    buffer_status: str = ""
    sse_broker: str = ""
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
    precedent_ref: UUID | None = None
    precedent_reason: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    context: dict[str, Any] = Field(default_factory=dict)


class TraceAlternative(BaseModel):
    """An alternative in a trace request."""

    label: str
    rejection_reason: str | None = None


class TraceEvidence(BaseModel):
    """Evidence in a trace request."""

    source_type: str
    source_uri: str | None = None
    content: str = ""
    relevance_score: float | None = None
    metrics: dict[str, float] | None = None


class QueryFilters(BaseModel):
    """Structured filters for decision queries."""

    agent_id: list[str] | None = None
    decision_type: str | None = None
    confidence_min: float | None = Field(default=None, ge=0.0, le=1.0)
    outcome: str | None = None
    session_id: str | None = None
    tool: str | None = None
    model: str | None = None
    project: str | None = None


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
    conflicts_unavailable: bool = False


class QueryResponse(BaseModel):
    """Response from a structured query."""

    decisions: list[Decision] = Field(default_factory=list)
    total: int = 0
    has_more: bool = False
    limit: int = 0
    offset: int = 0


class SearchResult(BaseModel):
    """A decision with its similarity score."""

    decision: Decision
    similarity_score: float


class SearchResponse(BaseModel):
    """Response from a semantic search."""

    results: list[SearchResult]
    total: int


class GetRunResponse(BaseModel):
    """Response from GET /v1/runs/{run_id} — includes run, events, and decisions."""

    run: AgentRun
    events: list[AgentEvent] = Field(default_factory=list)
    decisions: list[Decision] = Field(default_factory=list)


class VerifyResponse(BaseModel):
    """Response from GET /v1/verify/{decision_id} — integrity verification."""

    decision_id: UUID
    valid: bool
    stored_hash: str
    computed_hash: str


class RevisionsResponse(BaseModel):
    """Response from GET /v1/decisions/{decision_id}/revisions — revision chain."""

    decision_id: UUID
    revisions: list[Decision]
    count: int


# --- Assessment types (spec 29) ---


class AssessOutcome(str, Enum):
    """The verdict an assessor records for a prior decision."""

    correct = "correct"
    incorrect = "incorrect"
    partially_correct = "partially_correct"


class AssessRequest(BaseModel):
    """Request body for recording an outcome assessment."""

    outcome: AssessOutcome
    notes: str | None = None


class AssessResponse(BaseModel):
    """Response from POST /v1/decisions/{id}/assess and a list element from GET /v1/decisions/{id}/assessments."""

    id: UUID
    decision_id: UUID
    org_id: UUID
    assessor_agent_id: str
    outcome: AssessOutcome
    notes: str | None = None
    created_at: datetime


# --- Phase 2: Decision & conflict management types ---


class ConflictRecommendation(BaseModel):
    suggested_winner: UUID
    reasons: list[str] = Field(default_factory=list)
    confidence: float


class ConflictDetail(BaseModel):
    decision_conflict: DecisionConflict
    recommendation: ConflictRecommendation | None = None


class LineageEntry(BaseModel):
    id: UUID
    decision_type: str
    outcome: str
    confidence: float
    agent_id: str
    valid_from: datetime
    precedent_reason: str | None = None


class LineageResponse(BaseModel):
    decision_id: UUID
    precedent_ref: UUID | None = None
    precedent: LineageEntry | None = None
    cites: list[LineageEntry] = Field(default_factory=list)


class TimelineDecisionSummary(BaseModel):
    id: UUID
    agent_id: str
    decision_type: str
    outcome: str
    confidence: float
    project: str = ""
    created_at: datetime


class TimelineBucket(BaseModel):
    bucket: str
    decision_count: int = 0
    avg_confidence: float = 0.0
    decision_types: dict[str, int] = Field(default_factory=dict)
    agents: dict[str, int] = Field(default_factory=dict)
    conflict_count: int = 0
    top_decisions: list[TimelineDecisionSummary] = Field(default_factory=list)


class TimelineResponse(BaseModel):
    granularity: str
    buckets: list[TimelineBucket] = Field(default_factory=list)
    projects: list[str] = Field(default_factory=list)


class FacetsResponse(BaseModel):
    types: list[str] = Field(default_factory=list)
    projects: list[str] = Field(default_factory=list)


class EraseDecisionResponse(BaseModel):
    decision_id: UUID
    erased_at: datetime
    original_hash: str
    erased_hash: str
    alternatives_erased: int = 0
    evidence_erased: int = 0
    claims_erased: int = 0


class ConflictStatusUpdate(BaseModel):
    status: str  # "resolved" or "false_positive"
    resolution_note: str | None = None
    winning_decision_id: UUID | None = None
    false_positive_label: str | None = None


class AdjudicateConflictRequest(BaseModel):
    outcome: str
    reasoning: str | None = None
    decision_type: str = "conflict_resolution"
    winning_decision_id: UUID | None = None


class ResolveConflictGroupRequest(BaseModel):
    status: str  # "resolved" or "false_positive"
    resolution_note: str | None = None
    winning_agent: str | None = None
    false_positive_label: str | None = None


class ResolveConflictGroupResponse(BaseModel):
    group_id: UUID
    status: str
    resolved: int


class ConflictGroup(BaseModel):
    id: UUID
    org_id: UUID
    agent_a: str
    agent_b: str
    conflict_kind: ConflictKind
    decision_type: str
    group_topic: str = ""
    first_detected_at: datetime
    last_detected_at: datetime
    conflict_count: int = 0
    open_count: int = 0
    times_reopened: int = 0
    representative: DecisionConflict | None = None
    open_conflicts: list[DecisionConflict] = Field(default_factory=list)


class ConflictAnalyticsSummary(BaseModel):
    total_conflicts: int = 0
    open: int = 0
    resolved: int = 0
    false_positives: int = 0
    avg_days_to_resolution: float = 0.0


class ConflictAgentPairStats(BaseModel):
    agent_a: str
    agent_b: str
    count: int = 0
    open: int = 0
    resolved: int = 0
    false_positives: int = 0


class ConflictTypeStats(BaseModel):
    decision_type: str
    count: int = 0
    open: int = 0


class ConflictSeverityStats(BaseModel):
    severity: str
    count: int = 0
    open: int = 0


class ConflictDailyTrend(BaseModel):
    date: str
    detected: int = 0
    resolved: int = 0


class ConflictAnalyticsResponse(BaseModel):
    period: str = ""
    from_time: datetime | None = Field(default=None, alias="from")
    to_time: datetime | None = Field(default=None, alias="to")
    summary: ConflictAnalyticsSummary = Field(default_factory=ConflictAnalyticsSummary)
    by_agent_pair: list[ConflictAgentPairStats] = Field(default_factory=list)
    by_decision_type: list[ConflictTypeStats] = Field(default_factory=list)
    by_severity: list[ConflictSeverityStats] = Field(default_factory=list)
    daily_trend: list[ConflictDailyTrend] = Field(default_factory=list)


# --- Phase 3: Admin & configuration types ---


class APIKey(BaseModel):
    id: UUID
    prefix: str
    agent_id: str
    org_id: UUID | None = None
    label: str = ""
    created_by: str = ""
    created_at: datetime
    expires_at: datetime | None = None
    revoked_at: datetime | None = None


class APIKeyWithRawKey(BaseModel):
    api_key: APIKey
    raw_key: str


class CreateKeyRequest(BaseModel):
    agent_id: str
    label: str = ""
    expires_at: str | None = None


class RotateKeyResponse(BaseModel):
    new_key: APIKeyWithRawKey
    revoked_key_id: UUID


class ConflictResolutionSettings(BaseModel):
    auto_resolve_threshold: float = 0.95
    enable_cascade_resolution: bool = True
    cascade_similarity_threshold: float = 0.8


class OrgSettings(BaseModel):
    conflict_resolution: ConflictResolutionSettings = Field(default_factory=ConflictResolutionSettings)
    updated_at: datetime | None = None


class SetOrgSettingsRequest(BaseModel):
    conflict_resolution: ConflictResolutionSettings


class RetentionHold(BaseModel):
    id: UUID
    org_id: UUID
    reason: str
    hold_from: datetime
    hold_to: datetime
    decision_types: list[str] = Field(default_factory=list)
    agent_ids: list[str] = Field(default_factory=list)
    created_by: str = ""
    created_at: datetime
    released_at: datetime | None = None


class RetentionPolicy(BaseModel):
    retention_days: int = 0
    retention_exclude_types: list[str] = Field(default_factory=list)
    last_run: datetime | None = None
    last_run_deleted: int = 0
    next_run: datetime | None = None
    holds: list[RetentionHold] = Field(default_factory=list)


class SetRetentionRequest(BaseModel):
    retention_days: int
    retention_exclude_types: list[str] = Field(default_factory=list)


class PurgeCounts(BaseModel):
    decisions: int = 0
    alternatives: int = 0
    evidence: int = 0
    claims: int = 0
    events: int = 0


class PurgeRequest(BaseModel):
    before: datetime
    decision_type: str | None = None
    agent_id: str | None = None
    dry_run: bool = True


class PurgeResponse(BaseModel):
    dry_run: bool
    would_delete: PurgeCounts = Field(default_factory=PurgeCounts)
    deleted: PurgeCounts = Field(default_factory=PurgeCounts)


class CreateHoldRequest(BaseModel):
    reason: str
    from_time: datetime = Field(alias="from")
    to_time: datetime = Field(alias="to")
    decision_types: list[str] = Field(default_factory=list)
    agent_ids: list[str] = Field(default_factory=list)


class ProjectLink(BaseModel):
    id: UUID
    org_id: UUID
    project_a: str
    project_b: str
    link_type: str = ""
    created_by: str = ""
    created_at: datetime


class CreateProjectLinkRequest(BaseModel):
    project_a: str
    project_b: str
    link_type: str = "conflict_scope"


class IntegrityViolation(BaseModel):
    id: UUID
    decision_id: UUID
    org_id: UUID
    expected_hash: str
    actual_hash: str
    detected_at: datetime


class IntegrityViolationsResponse(BaseModel):
    violations: list[IntegrityViolation] = Field(default_factory=list)
    count: int = 0


class TraceHealthResponse(BaseModel):
    total_decisions: int = 0
    total_assessments: int = 0
    total_conflicts: int = 0
    avg_completeness: float = 0.0
    avg_confidence: float = 0.0
    assessment_rate: float = 0.0
    conflict_rate: float = 0.0
    compliance_score: float = 0.0


class UsageByKey(BaseModel):
    key_id: UUID
    prefix: str = ""
    label: str = ""
    agent_id: str = ""
    decisions: int = 0


class UsageByAgent(BaseModel):
    agent_id: str
    decisions: int = 0


class UsageResponse(BaseModel):
    org_id: UUID
    period: str = ""
    total_decisions: int = 0
    by_key: list[UsageByKey] = Field(default_factory=list)
    by_agent: list[UsageByAgent] = Field(default_factory=list)


class ScopedTokenRequest(BaseModel):
    as_agent_id: str
    expires_in: int = 300


class ScopedTokenResponse(BaseModel):
    token: str
    expires_at: datetime
    as_agent_id: str
    scoped_by: str


class SignupRequest(BaseModel):
    org_name: str
    agent_id: str
    email: str


class MCPConfigInfo(BaseModel):
    url: str = ""
    header: str = ""


class SignupResponse(BaseModel):
    org_id: UUID
    org_slug: str = ""
    agent_id: str
    api_key: str
    mcp_config: MCPConfigInfo | None = None


class ConfigResponse(BaseModel):
    search_enabled: bool


# --- Phase 4: Agent, grant, session types ---


class UpdateAgentRequest(BaseModel):
    name: str | None = None
    metadata: dict[str, Any] | None = None


class AgentStats(BaseModel):
    decision_count: int = 0
    last_decision_at: datetime | None = None
    avg_confidence: float = 0.0
    conflict_rate: float = 0.0


class AgentStatsResponse(BaseModel):
    agent_id: str
    stats: AgentStats


class SessionSummary(BaseModel):
    started_at: datetime | None = None
    ended_at: datetime | None = None
    duration_secs: float = 0.0
    decision_types: dict[str, int] = Field(default_factory=dict)
    avg_confidence: float = 0.0


class SessionViewResponse(BaseModel):
    session_id: UUID
    decisions: list[Decision] = Field(default_factory=list)
    decision_count: int = 0
    summary: SessionSummary = Field(default_factory=SessionSummary)
