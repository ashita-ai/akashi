// Package storage — Store interface for pluggable backends.
//
// The Store interface defines the subset of storage operations required by the
// MCP server, decisions service, authz, and tracehealth. The PostgreSQL *DB
// satisfies this interface with zero code changes. The SQLite lite-mode
// implementation provides a compatible alternative for zero-infra local use.
//
// See ADR-009 for the architectural rationale behind the two-backend design.
package storage

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/akashi/internal/model"
)

// Store is the storage interface consumed by the MCP tool path.
//
// Every method on this interface is already implemented by *DB (PostgreSQL).
// The SQLite lite-mode backend (internal/storage/sqlite) implements the same
// interface with appropriate degradation (no LISTEN/NOTIFY, no vector indexes,
// FTS5 instead of tsvector).
type Store interface {
	// ---- Lifecycle ----

	Ping(ctx context.Context) error
	Close(ctx context.Context)

	// ---- Organizations ----

	EnsureDefaultOrg(ctx context.Context) error

	// ---- Agents ----

	GetAgentByAgentID(ctx context.Context, orgID uuid.UUID, agentID string) (model.Agent, error)
	CreateAgent(ctx context.Context, agent model.Agent) (model.Agent, error)
	CreateAgentWithAudit(ctx context.Context, agent model.Agent, audit MutationAuditEntry) (model.Agent, error)
	CountAgents(ctx context.Context, orgID uuid.UUID) (int, error)
	ListAgentIDsBySharedTags(ctx context.Context, orgID uuid.UUID, tags []string) ([]string, error)

	// ---- API Keys ----

	GetAPIKeyByID(ctx context.Context, orgID uuid.UUID, keyID uuid.UUID) (model.APIKey, error)

	// ---- Decisions (trace) ----

	CreateTraceTx(ctx context.Context, params CreateTraceParams) (model.AgentRun, model.Decision, error)
	CreateTraceAndAdjudicateConflictTx(ctx context.Context, traceParams CreateTraceParams, conflictParams AdjudicateConflictInTraceParams) (model.AgentRun, model.Decision, error)

	// ---- Decisions (query) ----

	QueryDecisions(ctx context.Context, orgID uuid.UUID, req model.QueryRequest) ([]model.Decision, int, error)
	QueryDecisionsTemporal(ctx context.Context, orgID uuid.UUID, req model.TemporalQueryRequest) ([]model.Decision, error)
	SearchDecisionsByText(ctx context.Context, orgID uuid.UUID, query string, filters model.QueryFilters, limit int) ([]model.SearchResult, error)
	GetDecisionsByIDs(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]model.Decision, error)
	GetDecisionsByAgent(ctx context.Context, orgID uuid.UUID, agentID string, limit, offset int, from, to *time.Time) ([]model.Decision, int, error)
	GetDecisionForScoring(ctx context.Context, id, orgID uuid.UUID) (model.Decision, error)

	// ---- Conflicts ----

	ListConflicts(ctx context.Context, orgID uuid.UUID, filters ConflictFilters, limit, offset int) ([]model.DecisionConflict, error)
	ListConflictGroups(ctx context.Context, orgID uuid.UUID, filters ConflictGroupFilters, limit, offset int) ([]model.ConflictGroup, error)
	GetConflict(ctx context.Context, id, orgID uuid.UUID) (*model.DecisionConflict, error)
	GetConflictCount(ctx context.Context, decisionID, orgID uuid.UUID) (int, error)
	GetConflictCountsBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID]int, error)
	GetResolvedConflictsByType(ctx context.Context, orgID uuid.UUID, decisionType string, limit int) ([]model.ConflictResolution, error)
	UpdateConflictStatusWithAudit(ctx context.Context, id, orgID uuid.UUID, status, resolvedBy string, resolutionNote *string, winningDecisionID *uuid.UUID, audit MutationAuditEntry) (oldStatus string, err error)
	CascadeResolveByOutcome(ctx context.Context, orgID, groupID, winningDecisionID, triggerID uuid.UUID, threshold float64, audit MutationAuditEntry) (int, error)

	// ---- Embeddings ----

	GetDecisionEmbeddings(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID][2]pgvector.Vector, error)
	FindUnembeddedDecisions(ctx context.Context, limit int) ([]UnembeddedDecision, error)
	BackfillEmbedding(ctx context.Context, id, orgID uuid.UUID, emb pgvector.Vector) error
	FindDecisionsMissingOutcomeEmbedding(ctx context.Context, limit int) ([]UnembeddedDecision, error)
	BackfillOutcomeEmbedding(ctx context.Context, id, orgID uuid.UUID, emb pgvector.Vector) error

	// ---- Signals & Assessments ----

	GetDecisionOutcomeSignalsBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID]model.OutcomeSignals, error)
	GetAssessmentSummaryBatch(ctx context.Context, orgID uuid.UUID, decisionIDs []uuid.UUID) (map[uuid.UUID]model.AssessmentSummary, error)
	CreateAssessment(ctx context.Context, orgID uuid.UUID, a model.DecisionAssessment) (model.DecisionAssessment, error)
	UpdateOutcomeScore(ctx context.Context, orgID, decisionID uuid.UUID, score *float32) error

	// ---- Claims ----

	HasClaimsForDecision(ctx context.Context, decisionID, orgID uuid.UUID) (bool, error)
	InsertClaims(ctx context.Context, claims []Claim) error
	FindDecisionIDsMissingClaims(ctx context.Context, limit int) ([]DecisionRef, error)
	MarkClaimEmbeddingFailed(ctx context.Context, decisionID, orgID uuid.UUID) error
	ClearClaimEmbeddingFailure(ctx context.Context, decisionID, orgID uuid.UUID) error
	FindRetriableClaimFailures(ctx context.Context, maxAttempts, limit int) ([]ClaimRetryRef, error)

	// ---- Idempotency ----

	BeginIdempotency(ctx context.Context, orgID uuid.UUID, agentID, endpoint, key, requestHash string) (IdempotencyLookup, error)
	CompleteIdempotency(ctx context.Context, orgID uuid.UUID, agentID, endpoint, key string, statusCode int, responseData any) error
	ClearInProgressIdempotency(ctx context.Context, orgID uuid.UUID, agentID, endpoint, key string) error

	// ---- Notifications ----

	// Notify sends a notification on the given channel. Implementations that
	// lack pub/sub (e.g. SQLite) return nil without sending.
	Notify(ctx context.Context, channel, payload string) error
	HasNotifyConn() bool

	// ---- Grants (authz) ----

	HasAccess(ctx context.Context, orgID uuid.UUID, granteeID uuid.UUID, resourceType, resourceID, permission string) (bool, error)
	ListGrantedAgentIDs(ctx context.Context, orgID uuid.UUID, granteeID uuid.UUID, selfAgentID string) (map[string]bool, error)

	// ---- Trace health ----

	// GetDecisionQualityStats returns aggregate quality metrics. When from/to are non-nil
	// only decisions with valid_from in [from, to) are included; nil means no bound.
	GetDecisionQualityStats(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (DecisionQualityStats, error)
	// GetEvidenceCoverageStats returns evidence coverage metrics. from/to scope decisions by valid_from.
	GetEvidenceCoverageStats(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (EvidenceCoverageStats, error)
	// GetConflictStatusCounts returns conflict status breakdown. from/to scope conflicts by detected_at.
	GetConflictStatusCounts(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (ConflictStatusCounts, error)
	GetWontFixRate(ctx context.Context, orgID uuid.UUID) (WontFixRate, error)
	// GetOutcomeSignalsSummary returns outcome signals. from/to scope decisions by valid_from.
	GetOutcomeSignalsSummary(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (OutcomeSignalsSummary, error)
	// GetConfidenceDistribution returns confidence histogram and per-agent stats. from/to scope decisions by valid_from.
	GetConfidenceDistribution(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (ConfidenceDistribution, error)
	// GetHighConfOutcomeSignals returns behavioral signals for decisions with confidence >= 0.85. from/to scope by valid_from.
	GetHighConfOutcomeSignals(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (HighConfOutcomeSignals, error)
	GetDecisionTypeDistribution(ctx context.Context, orgID uuid.UUID) ([]DecisionTypeCount, error)

	// ---- Error classification ----

	// IsDuplicateKey returns true if the error represents a unique constraint
	// violation. The implementation is database-specific (pgconn error code
	// 23505 for Postgres, SQLITE_CONSTRAINT_UNIQUE for SQLite).
	IsDuplicateKey(err error) bool
}
