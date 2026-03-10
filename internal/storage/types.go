package storage

// types.go holds type definitions, error sentinels, and constants that are
// referenced by the Store interface (store.go) and therefore must be available
// in both the full (PostgreSQL) build and the lite (SQLite) build. Crucially,
// this file does NOT import pgx, pgconn, or pgxpool — keeping cmd/akashi-local
// free from PostgreSQL compile-time dependencies (ADR-009).

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/akashi/internal/model"
)

// ---------------------------------------------------------------------------
// Trace types (originally in trace.go)
// ---------------------------------------------------------------------------

// CreateTraceParams holds all data needed to create a complete decision trace
// within a single database transaction.
type CreateTraceParams struct {
	AgentID      string
	OrgID        uuid.UUID
	TraceID      *string
	Metadata     map[string]any
	Decision     model.Decision
	Alternatives []model.Alternative
	Evidence     []model.Evidence
	SessionID    *uuid.UUID
	AgentContext map[string]any

	// AuditEntry, when non-nil, is inserted into mutation_audit_log inside the
	// same transaction. ResourceID is populated automatically from the generated
	// decision ID. This ensures the audit record is atomic with the trace —
	// if the tx rolls back, the audit entry never persists.
	AuditEntry *MutationAuditEntry
}

// AdjudicateConflictInTraceParams holds data needed for the conflict adjudication
// that should be committed atomically with the trace.
type AdjudicateConflictInTraceParams struct {
	ConflictID        uuid.UUID
	ResolvedBy        string
	ResNote           *string
	Audit             MutationAuditEntry
	WinningDecisionID *uuid.UUID // optional; must be decision_a_id or decision_b_id if set
}

// ---------------------------------------------------------------------------
// Audit types (originally in audit.go)
// ---------------------------------------------------------------------------

// MutationAuditEntry is an append-only audit event for a state-changing API call.
type MutationAuditEntry struct {
	RequestID    string
	OrgID        uuid.UUID
	ActorAgentID string
	ActorRole    string
	HTTPMethod   string
	Endpoint     string
	Operation    string
	ResourceType string
	ResourceID   string
	BeforeData   any
	AfterData    any
	Metadata     map[string]any
}

// ---------------------------------------------------------------------------
// Conflict types (originally in conflicts.go)
// ---------------------------------------------------------------------------

// ConflictFilters holds optional filters for conflict queries.
type ConflictFilters struct {
	DecisionType *string
	AgentID      *string
	ConflictKind *string    // "cross_agent" or "self_contradiction"
	Status       *string    // "open", "acknowledged", "resolved", "wont_fix"
	StatusIn     []string   // Multi-value status filter (OR). Takes precedence over Status when set.
	Severity     *string    // "critical", "high", "medium", "low"
	Category     *string    // "factual", "assessment", "strategic", "temporal"
	DecisionID   *uuid.UUID // conflicts involving this decision (A or B side)
}

// ConflictStatusCounts holds the number of conflicts in each resolution status.
type ConflictStatusCounts struct {
	Total        int
	Open         int
	Acknowledged int
	Resolved     int
	WontFix      int
}

// ConflictGroupFilters holds optional filters for conflict group queries.
type ConflictGroupFilters struct {
	DecisionType *string
	AgentID      *string
	ConflictKind *string
	// OpenOnly restricts results to groups that have at least one open or
	// acknowledged member conflict. When false, all groups are returned.
	OpenOnly bool
}

// ---------------------------------------------------------------------------
// Decision types (originally in decisions.go)
// ---------------------------------------------------------------------------

// DecisionQualityStats holds aggregate completeness metrics for an org's decisions.
type DecisionQualityStats struct {
	Total            int
	AvgCompleteness  float64
	BelowHalf        int // completeness_score < 0.5
	BelowThird       int // completeness_score < 0.33
	WithReasoning    int // reasoning IS NOT NULL AND reasoning != ''
	WithAlternatives int // decisions that have at least one alternative
}

// UnembeddedDecision holds the minimal fields needed to backfill an embedding.
type UnembeddedDecision struct {
	ID           uuid.UUID
	OrgID        uuid.UUID
	DecisionType string
	Outcome      string
	Reasoning    *string
}

// DecisionRef is a lightweight reference to a decision for batch operations.
type DecisionRef struct {
	ID    uuid.UUID
	OrgID uuid.UUID
}

// ---------------------------------------------------------------------------
// Evidence types (originally in evidence.go)
// ---------------------------------------------------------------------------

// EvidenceCoverageStats holds evidence coverage metrics for an org.
type EvidenceCoverageStats struct {
	TotalDecisions       int
	WithEvidence         int
	WithoutEvidenceCount int
	CoveragePercent      float64
	TotalRecords         int     // total evidence rows
	AvgPerDecision       float64 // average evidence records per decision
}

// ---------------------------------------------------------------------------
// Claims types (originally in claims.go)
// ---------------------------------------------------------------------------

// Claim is a sentence-level assertion extracted from a decision outcome,
// stored with its own embedding for fine-grained conflict detection.
type Claim struct {
	ID         uuid.UUID
	DecisionID uuid.UUID
	OrgID      uuid.UUID
	ClaimIdx   int
	ClaimText  string
	Category   *string // finding, recommendation, assessment, status (nil = uncategorized/regex-extracted)
	Embedding  *pgvector.Vector
}

// ClaimRetryRef is a reference to a decision eligible for claim embedding retry,
// including the current attempt count for backoff and metric attribution.
type ClaimRetryRef struct {
	ID       uuid.UUID
	OrgID    uuid.UUID
	Attempts int
}

// ---------------------------------------------------------------------------
// Idempotency types (originally in idempotency.go)
// ---------------------------------------------------------------------------

var (
	// ErrIdempotencyPayloadMismatch is returned when the same idempotency key is reused
	// with a different request payload hash for the same (org, agent, endpoint).
	ErrIdempotencyPayloadMismatch = errors.New("idempotency key reused with different payload")
	// ErrIdempotencyInProgress indicates a matching idempotency key is currently being processed.
	ErrIdempotencyInProgress = errors.New("idempotency key request already in progress")
)

// IdempotencyLookup describes the current state of an idempotency key lookup.
type IdempotencyLookup struct {
	Completed    bool
	StatusCode   int
	ResponseData json.RawMessage
}

// ---------------------------------------------------------------------------
// Trace health types (originally in tracehealth.go)
// ---------------------------------------------------------------------------

// OutcomeSignalsSummary holds org-level aggregate outcome signal counts
// for the trace-health endpoint (Spec 35).
type OutcomeSignalsSummary struct {
	DecisionsTotal    int `json:"decisions_total"`
	NeverSuperseded   int `json:"never_superseded"`
	RevisedWithin48h  int `json:"revised_within_48h"`
	NeverCited        int `json:"never_cited"`
	CitedAtLeastOnce  int `json:"cited_at_least_once"`
	ConflictsWon      int `json:"conflicts_won"`
	ConflictsLost     int `json:"conflicts_lost"`
	ConflictsNoWinner int `json:"conflicts_no_winner"`
}

// ---------------------------------------------------------------------------
// Notification constants (originally in notify.go)
// ---------------------------------------------------------------------------

// NotifyChannel is a Postgres LISTEN/NOTIFY channel name.
const (
	ChannelDecisions = "akashi_decisions"
	ChannelConflicts = "akashi_conflicts"
)
