// Package decisions provides the shared business logic for decision operations.
//
// Both the HTTP API and MCP server delegate to this service, eliminating
// duplicated logic and ensuring consistent behavior (embedding generation,
// quality scoring, transactional writes, notification) across all interfaces.
package decisions

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashita-ai/akashi/internal/billing"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/telemetry"
)

// Service encapsulates decision business logic shared by HTTP and MCP handlers.
type Service struct {
	db         *storage.DB
	embedder   embedding.Provider
	searcher   search.Searcher
	billingSvc *billing.Service
	logger     *slog.Logger

	embeddingDuration metric.Float64Histogram
	searchDuration    metric.Float64Histogram
}

// New creates a new decision Service.
// searcher may be nil if Qdrant is not configured (falls back to text search).
// billingSvc may be nil if billing is not configured.
func New(db *storage.DB, embedder embedding.Provider, searcher search.Searcher, billingSvc *billing.Service, logger *slog.Logger) *Service {
	meter := telemetry.Meter("akashi/decisions")
	embDur, _ := meter.Float64Histogram("akashi.embedding.duration",
		metric.WithDescription("Time to generate embeddings (ms)"),
		metric.WithUnit("ms"),
	)
	searchDur, _ := meter.Float64Histogram("akashi.search.duration",
		metric.WithDescription("Time to execute search queries (ms)"),
		metric.WithUnit("ms"),
	)
	return &Service{
		db:                db,
		embedder:          embedder,
		searcher:          searcher,
		billingSvc:        billingSvc,
		logger:            logger,
		embeddingDuration: embDur,
		searchDuration:    searchDur,
	}
}

// TraceInput contains the data needed to record a decision.
type TraceInput struct {
	AgentID      string
	TraceID      *string
	Metadata     map[string]any
	Decision     model.TraceDecision
	PrecedentRef *uuid.UUID
}

// TraceResult is the outcome of recording a decision.
type TraceResult struct {
	RunID      uuid.UUID
	DecisionID uuid.UUID
	EventCount int
}

// Trace records a complete decision with its alternatives and evidence.
// Embeddings and quality scores are computed first, then all database writes
// happen atomically within a single transaction. Notification is sent after commit.
func (s *Service) Trace(ctx context.Context, orgID uuid.UUID, input TraceInput) (TraceResult, error) {
	// 0a. Set OTEL span attributes for trace correlation.
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("akashi.agent_id", input.AgentID),
		attribute.String("akashi.decision_type", input.Decision.DecisionType),
	)
	if input.TraceID != nil {
		span.SetAttributes(attribute.String("akashi.trace_id", *input.TraceID))
	}

	// 0b. Quota check (before any DB writes or embedding calls).
	if s.billingSvc != nil {
		if err := s.billingSvc.CheckDecisionQuota(ctx, orgID); err != nil {
			return TraceResult{}, err
		}
	}

	// 1. Generate decision embedding (outside tx — may call external API).
	embText := input.Decision.DecisionType + ": " + input.Decision.Outcome
	if input.Decision.Reasoning != nil {
		embText += " " + *input.Decision.Reasoning
	}
	var decisionEmb *pgvector.Vector
	embStart := time.Now()
	emb, err := s.embedder.Embed(ctx, embText)
	s.embeddingDuration.Record(ctx, float64(time.Since(embStart).Milliseconds()))
	if err != nil {
		s.logger.Warn("trace: decision embedding failed, continuing without", "error", err)
	} else if err := s.validateEmbeddingDims(emb); err != nil {
		return TraceResult{}, fmt.Errorf("trace: %w (check AKASHI_EMBEDDING_DIMENSIONS config)", err)
	} else {
		decisionEmb = &emb
	}

	// 2. Compute quality score.
	qualityScore := quality.Score(input.Decision)

	// 3. Build alternatives.
	alts := make([]model.Alternative, len(input.Decision.Alternatives))
	for i, a := range input.Decision.Alternatives {
		alts[i] = model.Alternative{
			Label:           a.Label,
			Score:           a.Score,
			Selected:        a.Selected,
			RejectionReason: a.RejectionReason,
		}
	}

	// 4. Build evidence with embeddings (outside tx — may call external API).
	evs := make([]model.Evidence, len(input.Decision.Evidence))
	for i, e := range input.Decision.Evidence {
		var evEmb *pgvector.Vector
		if e.Content != "" {
			vec, err := s.embedder.Embed(ctx, e.Content)
			if err != nil {
				s.logger.Warn("trace: evidence embedding failed", "error", err)
			} else if err := s.validateEmbeddingDims(vec); err != nil {
				return TraceResult{}, fmt.Errorf("trace: evidence %w (check AKASHI_EMBEDDING_DIMENSIONS config)", err)
			} else {
				evEmb = &vec
			}
		}
		evs[i] = model.Evidence{
			OrgID:          orgID,
			SourceType:     model.SourceType(e.SourceType),
			SourceURI:      e.SourceURI,
			Content:        e.Content,
			RelevanceScore: e.RelevanceScore,
			Embedding:      evEmb,
		}
	}

	// 5. Execute transactional write (run + decision + alts + evidence + complete).
	run, decision, err := s.db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID:  input.AgentID,
		OrgID:    orgID,
		TraceID:  input.TraceID,
		Metadata: input.Metadata,
		Decision: model.Decision{
			DecisionType: input.Decision.DecisionType,
			Outcome:      input.Decision.Outcome,
			Confidence:   input.Decision.Confidence,
			Reasoning:    input.Decision.Reasoning,
			Embedding:    decisionEmb,
			QualityScore: qualityScore,
			PrecedentRef: input.PrecedentRef,
		},
		Alternatives: alts,
		Evidence:     evs,
	})
	if err != nil {
		return TraceResult{}, fmt.Errorf("trace: %w", err)
	}

	// 6. Notify subscribers (after commit, non-fatal).
	notifyPayload, err := json.Marshal(map[string]any{
		"decision_id": decision.ID,
		"agent_id":    input.AgentID,
		"org_id":      orgID,
		"outcome":     input.Decision.Outcome,
	})
	if err != nil {
		s.logger.Error("trace: marshal notify payload", "error", err)
	} else if err := s.db.Notify(ctx, storage.ChannelDecisions, string(notifyPayload)); err != nil {
		s.logger.Error("trace: notify subscribers", "error", err)
	}

	// 7. Increment usage counter (after successful commit, non-fatal).
	if s.billingSvc != nil {
		if err := s.billingSvc.IncrementDecisionCount(ctx, orgID); err != nil {
			s.logger.Warn("trace: increment usage counter failed (non-fatal)", "error", err, "org_id", orgID)
		}
	}

	eventCount := len(alts) + len(evs) + 1 // +1 for the decision itself
	return TraceResult{
		RunID:      run.ID,
		DecisionID: decision.ID,
		EventCount: eventCount,
	}, nil
}

// Check performs a precedent lookup by semantic search or structured query.
func (s *Service) Check(ctx context.Context, orgID uuid.UUID, decisionType, query, agentID string, limit int) (model.CheckResponse, error) {
	if limit <= 0 {
		limit = 5
	}

	var decisions []model.Decision

	if query != "" {
		// Use the same Qdrant → text fallback chain as Search.
		filters := model.QueryFilters{DecisionType: &decisionType}
		if agentID != "" {
			filters.AgentIDs = []string{agentID}
		}
		results, err := s.Search(ctx, orgID, query, true, filters, limit)
		if err != nil {
			return model.CheckResponse{}, fmt.Errorf("check: search: %w", err)
		}
		for _, sr := range results {
			decisions = append(decisions, sr.Decision)
		}
	} else {
		// Structured query path.
		filters := model.QueryFilters{DecisionType: &decisionType}
		if agentID != "" {
			filters.AgentIDs = []string{agentID}
		}
		queried, _, err := s.db.QueryDecisions(ctx, orgID, model.QueryRequest{
			Filters:  filters,
			Include:  []string{"alternatives"},
			OrderBy:  "valid_from",
			OrderDir: "desc",
			Limit:    limit,
		})
		if err != nil {
			return model.CheckResponse{}, fmt.Errorf("check: query: %w", err)
		}
		decisions = queried
	}

	// Always check for conflicts.
	conflicts, err := s.db.ListConflicts(ctx, orgID, storage.ConflictFilters{DecisionType: &decisionType}, limit, 0)
	if err != nil {
		s.logger.Warn("check: list conflicts", "error", err)
		conflicts = nil
	}

	return model.CheckResponse{
		HasPrecedent: len(decisions) > 0,
		Decisions:    decisions,
		Conflicts:    conflicts,
	}, nil
}

// Search performs semantic or text-based search over decisions.
// Fallback chain: Qdrant (semantic) → ILIKE text search (keyword).
// When semantic is true and Qdrant is healthy, it queries Qdrant and hydrates
// results from Postgres. On any Qdrant failure, it falls through to text search.
func (s *Service) Search(ctx context.Context, orgID uuid.UUID, query string, semantic bool, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	if semantic && s.searcher != nil {
		if err := s.searcher.Healthy(ctx); err == nil {
			embStart := time.Now()
			queryEmb, err := s.embedder.Embed(ctx, query)
			s.embeddingDuration.Record(ctx, float64(time.Since(embStart).Milliseconds()))
			if err != nil {
				s.logger.Warn("search: embedding failed, falling back to text", "error", err)
			} else if !isZeroVector(queryEmb) {
				searchStart := time.Now()
				results, err := s.searcher.Search(ctx, orgID, queryEmb.Slice(), filters, limit)
				s.searchDuration.Record(ctx, float64(time.Since(searchStart).Milliseconds()))
				if err != nil {
					s.logger.Warn("search: qdrant query failed, falling back to text", "error", err)
				} else {
					return s.hydrateAndReScore(ctx, orgID, results, limit)
				}
			}
		} else {
			s.logger.Debug("search: qdrant unhealthy, using text search", "error", err)
		}
	}

	return s.db.SearchDecisionsByText(ctx, orgID, query, filters, limit)
}

// hydrateAndReScore fetches full decisions from Postgres and applies quality+recency re-scoring.
func (s *Service) hydrateAndReScore(ctx context.Context, orgID uuid.UUID, results []search.Result, limit int) ([]model.SearchResult, error) {
	if len(results) == 0 {
		return []model.SearchResult{}, nil
	}

	ids := make([]uuid.UUID, len(results))
	for i, r := range results {
		ids[i] = r.DecisionID
	}

	decisions, err := s.db.GetDecisionsByIDs(ctx, orgID, ids)
	if err != nil {
		return nil, fmt.Errorf("search: hydrate decisions: %w", err)
	}

	return search.ReScore(results, decisions, limit), nil
}

// validateEmbeddingDims checks that the vector has the expected number of dimensions.
func (s *Service) validateEmbeddingDims(v pgvector.Vector) error {
	expected := s.embedder.Dimensions()
	got := len(v.Slice())
	if got != expected {
		return fmt.Errorf("embedding dimension mismatch: got %d, want %d", got, expected)
	}
	return nil
}

// isZeroVector returns true if all elements of the vector are zero (noop provider).
func isZeroVector(v pgvector.Vector) bool {
	for _, val := range v.Slice() {
		if val != 0 {
			return false
		}
	}
	return true
}

// Query executes a structured query with filters, ordering, and pagination.
func (s *Service) Query(ctx context.Context, orgID uuid.UUID, req model.QueryRequest) ([]model.Decision, int, error) {
	return s.db.QueryDecisions(ctx, orgID, req)
}

// Recent returns recent decisions with optional filters.
func (s *Service) Recent(ctx context.Context, orgID uuid.UUID, filters model.QueryFilters, limit int) ([]model.Decision, int, error) {
	return s.db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters:  filters,
		Include:  []string{"alternatives"},
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    limit,
	})
}
