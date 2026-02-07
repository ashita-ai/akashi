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

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/akashi/internal/billing"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Service encapsulates decision business logic shared by HTTP and MCP handlers.
type Service struct {
	db         *storage.DB
	embedder   embedding.Provider
	billingSvc *billing.Service
	logger     *slog.Logger
}

// New creates a new decision Service.
// billingSvc may be nil if billing is not configured.
func New(db *storage.DB, embedder embedding.Provider, billingSvc *billing.Service, logger *slog.Logger) *Service {
	return &Service{
		db:         db,
		embedder:   embedder,
		billingSvc: billingSvc,
		logger:     logger,
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
	// 0. Quota check (before any DB writes or embedding calls).
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
	emb, err := s.embedder.Embed(ctx, embText)
	if err != nil {
		s.logger.Warn("trace: decision embedding failed, continuing without", "error", err)
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
			} else {
				evEmb = &vec
			}
		}
		evs[i] = model.Evidence{
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
		filters := model.QueryFilters{DecisionType: &decisionType}
		if agentID != "" {
			filters.AgentIDs = []string{agentID}
		}

		// Try semantic search, fall back to text search if embeddings are zero.
		var results []model.SearchResult
		queryEmb, err := s.embedder.Embed(ctx, query)
		if err != nil {
			return model.CheckResponse{}, fmt.Errorf("check: generate embedding: %w", err)
		}
		isZero := true
		for _, v := range queryEmb.Slice() {
			if v != 0 {
				isZero = false
				break
			}
		}
		if !isZero {
			results, err = s.db.SearchDecisionsByEmbedding(ctx, orgID, queryEmb, filters, limit)
		} else {
			results, err = s.db.SearchDecisionsByText(ctx, orgID, query, filters, limit)
		}
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
// When semantic is true and a real embedding provider is available, it uses
// vector similarity. Otherwise it falls back to keyword matching.
func (s *Service) Search(ctx context.Context, orgID uuid.UUID, query string, semantic bool, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	if semantic {
		queryEmb, err := s.embedder.Embed(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("search: generate embedding: %w", err)
		}
		// Check if the embedding is non-trivial (not all zeros from noop provider).
		isZero := true
		for _, v := range queryEmb.Slice() {
			if v != 0 {
				isZero = false
				break
			}
		}
		if !isZero {
			return s.db.SearchDecisionsByEmbedding(ctx, orgID, queryEmb, filters, limit)
		}
		// Fall through to text search if embeddings are zero (noop provider).
	}
	return s.db.SearchDecisionsByText(ctx, orgID, query, filters, limit)
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
