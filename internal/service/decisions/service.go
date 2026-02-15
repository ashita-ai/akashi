// Package decisions provides the shared business logic for decision operations.
//
// Both the HTTP API and MCP server delegate to this service, eliminating
// duplicated logic and ensuring consistent behavior (embedding generation,
// quality scoring, transactional writes, notification) across all interfaces.
package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/service/embedding"
	"github.com/ashita-ai/akashi/internal/service/quality"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/telemetry"
)

// ConflictScorer scores semantic conflicts for new decisions.
type ConflictScorer interface {
	ScoreForDecision(ctx context.Context, decisionID, orgID uuid.UUID)
}

// Service encapsulates decision business logic shared by HTTP and MCP handlers.
type Service struct {
	db             *storage.DB
	embedder       embedding.Provider
	searcher       search.Searcher
	conflictScorer ConflictScorer
	logger         *slog.Logger

	embeddingDuration metric.Float64Histogram
	searchDuration    metric.Float64Histogram
}

// New creates a new decision Service.
// searcher may be nil if Qdrant is not configured (falls back to text search).
// conflictScorer may be nil to disable semantic conflict detection.
func New(db *storage.DB, embedder embedding.Provider, searcher search.Searcher, logger *slog.Logger, conflictScorer ConflictScorer) *Service {
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
		conflictScorer:    conflictScorer,
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
	SessionID    *uuid.UUID     // MCP session or X-Akashi-Session header.
	AgentContext map[string]any // Merged server-extracted + client-supplied context.
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

	// 1. Generate decision embedding (full) and outcome embedding (Option B).
	embText := input.Decision.DecisionType + ": " + input.Decision.Outcome
	if input.Decision.Reasoning != nil {
		embText += " " + *input.Decision.Reasoning
	}
	var decisionEmb, outcomeEmb *pgvector.Vector
	embStart := time.Now()
	emb, err := s.embedder.Embed(ctx, embText)
	if err != nil {
		s.logger.Warn("trace: decision embedding failed, continuing without", "error", err)
	} else if err := s.validateEmbeddingDims(emb); err != nil {
		return TraceResult{}, fmt.Errorf("trace: %w (check AKASHI_EMBEDDING_DIMENSIONS config)", err)
	} else {
		s.embeddingDuration.Record(ctx, float64(time.Since(embStart).Milliseconds()))
		decisionEmb = &emb
	}
	// Outcome-only embedding for precise conflict outcome comparison.
	if outcomeVec, err := s.embedder.Embed(ctx, input.Decision.Outcome); err == nil && s.validateEmbeddingDims(outcomeVec) == nil {
		outcomeEmb = &outcomeVec
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
	// Wrapped in WithRetry to handle Postgres serialization failures (40001) and
	// deadlocks (40P01) that can occur when concurrent traces race on indexes.
	var run model.AgentRun
	var decision model.Decision
	err = storage.WithRetry(ctx, 3, 10*time.Millisecond, func() error {
		var txErr error
		run, decision, txErr = s.db.CreateTraceTx(ctx, storage.CreateTraceParams{
			AgentID:  input.AgentID,
			OrgID:    orgID,
			TraceID:  input.TraceID,
			Metadata: input.Metadata,
			Decision: model.Decision{
				DecisionType:     input.Decision.DecisionType,
				Outcome:          input.Decision.Outcome,
				Confidence:       input.Decision.Confidence,
				Reasoning:        input.Decision.Reasoning,
				Embedding:        decisionEmb,
				OutcomeEmbedding: outcomeEmb,
				QualityScore:     qualityScore,
				PrecedentRef:     input.PrecedentRef,
			},
			Alternatives: alts,
			Evidence:     evs,
			SessionID:    input.SessionID,
			AgentContext: input.AgentContext,
		})
		return txErr
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

	// 7. Generate claim-level embeddings for fine-grained conflict detection.
	// Must complete BEFORE conflict scoring so the scorer can use claims.
	if decisionEmb != nil {
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					s.logger.Error("trace: claim generation panicked", "panic", rec, "decision_id", decision.ID)
				}
			}()
			claimCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := s.generateClaims(claimCtx, decision.ID, orgID, input.Decision.Outcome); err != nil {
				s.logger.Warn("trace: claim generation failed", "decision_id", decision.ID, "error", err)
			}
			// 8. Trigger semantic conflict scoring (after claims are stored).
			if s.conflictScorer != nil {
				s.conflictScorer.ScoreForDecision(claimCtx, decision.ID, orgID)
			}
		}()
	} else if s.conflictScorer != nil {
		// No embeddings available — still try conflict scoring (it will use full-outcome only).
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					s.logger.Error("trace: conflict scorer panicked", "panic", rec, "decision_id", decision.ID, "org_id", orgID)
				}
			}()
			scoreCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.conflictScorer.ScoreForDecision(scoreCtx, decision.ID, orgID)
		}()
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

	// Only surface open/acknowledged conflicts — resolved and wont_fix are hidden.
	conflicts, err := s.db.ListConflicts(ctx, orgID, storage.ConflictFilters{DecisionType: &decisionType}, limit, 0)
	if err != nil {
		s.logger.Warn("check: list conflicts", "error", err)
		conflicts = nil
	}
	// Filter out resolved/wont_fix so akashi_check only shows actionable conflicts.
	filtered := conflicts[:0]
	for _, c := range conflicts {
		if c.Status == "resolved" || c.Status == "wont_fix" {
			continue
		}
		filtered = append(filtered, c)
	}
	conflicts = filtered

	return model.CheckResponse{
		HasPrecedent: len(decisions) > 0,
		Decisions:    decisions,
		Conflicts:    conflicts,
	}, nil
}

// Search performs semantic or text-based search over decisions.
// Fallback chain: Qdrant (semantic) → ILIKE text search (keyword).
// When semantic is true and Qdrant is healthy, it queries Qdrant and hydrates
// results from Postgres. On any Qdrant failure or empty result set, it falls
// through to text search.
func (s *Service) Search(ctx context.Context, orgID uuid.UUID, query string, semantic bool, filters model.QueryFilters, limit int) ([]model.SearchResult, error) {
	if semantic && s.searcher != nil {
		if err := s.searcher.Healthy(ctx); err == nil {
			embStart := time.Now()
			queryEmb, err := s.embedder.Embed(ctx, query)
			if err != nil {
				s.logger.Warn("search: embedding failed, falling back to text", "error", err)
			} else if !isZeroVector(queryEmb) {
				s.embeddingDuration.Record(ctx, float64(time.Since(embStart).Milliseconds()))
				searchStart := time.Now()
				results, err := s.searcher.Search(ctx, orgID, queryEmb.Slice(), filters, limit)
				s.searchDuration.Record(ctx, float64(time.Since(searchStart).Milliseconds()))
				switch {
				case err != nil:
					s.logger.Warn("search: qdrant query failed, falling back to text", "error", err)
				case len(results) > 0:
					return s.hydrateAndReScore(ctx, orgID, results, limit)
				default:
					s.logger.Debug("search: qdrant returned no results, falling back to text")
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

// QueryTemporal executes a bi-temporal point-in-time query over decisions.
// Returns decisions visible as of the given timestamp (transaction_time <= as_of,
// and either valid_to IS NULL or valid_to > as_of).
func (s *Service) QueryTemporal(ctx context.Context, orgID uuid.UUID, req model.TemporalQueryRequest) ([]model.Decision, error) {
	return s.db.QueryDecisionsTemporal(ctx, orgID, req)
}

// Recent returns recent decisions with optional filters and pagination.
func (s *Service) Recent(ctx context.Context, orgID uuid.UUID, filters model.QueryFilters, limit, offset int) ([]model.Decision, int, error) {
	return s.db.QueryDecisions(ctx, orgID, model.QueryRequest{
		Filters:  filters,
		Include:  []string{"alternatives"},
		OrderBy:  "valid_from",
		OrderDir: "desc",
		Limit:    limit,
		Offset:   offset,
	})
}

// SemanticSearchAvailable returns true when both Qdrant and a real embedding
// provider are configured. Used by the config endpoint so the UI can accurately
// show whether semantic (vector) search is available.
func (s *Service) SemanticSearchAvailable() bool {
	if s.searcher == nil {
		return false
	}
	_, isNoop := s.embedder.(*embedding.NoopProvider)
	return !isNoop
}

// ErrAgentNotFound indicates the agent does not exist and the caller lacks
// permission to auto-create it.
var ErrAgentNotFound = errors.New("agent_id not found in this organization")

// ResolveOrCreateAgent looks up an agent by agent_id within an org. If the
// agent does not exist and the caller has admin+ privileges, it auto-registers
// a trace-only agent (role=agent, no API key). Non-admin callers receive
// ErrAgentNotFound.
//
// This eliminates friction when an admin traces on behalf of a new agent for
// the first time — the agent is created implicitly rather than requiring a
// separate POST /v1/agents call.
func (s *Service) ResolveOrCreateAgent(ctx context.Context, orgID uuid.UUID, agentID string, callerRole model.AgentRole) error {
	_, err := s.db.GetAgentByAgentID(ctx, orgID, agentID)
	if err == nil {
		return nil
	}

	// Only auto-register on not-found errors. Propagate anything else.
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}

	// Non-admin callers cannot auto-register agents.
	if !model.RoleAtLeast(callerRole, model.RoleAdmin) {
		return ErrAgentNotFound
	}

	// Admin+ caller: auto-register the agent with default role.
	_, createErr := s.db.CreateAgent(ctx, model.Agent{
		AgentID: agentID,
		OrgID:   orgID,
		Name:    agentID,
		Role:    model.RoleAgent,
	})
	if createErr != nil {
		// A concurrent request may have created the same agent between our
		// GetAgentByAgentID and CreateAgent calls. That's fine — treat the
		// duplicate key constraint as success.
		if isDuplicateKey(createErr) {
			return nil
		}
		return fmt.Errorf("auto-register agent: %w", createErr)
	}

	s.logger.Info("auto-registered agent on first trace", "agent_id", agentID, "org_id", orgID)
	return nil
}

// BackfillEmbeddings generates embeddings for decisions that were stored without
// one (e.g. because the embedding provider was noop at trace time). Each decision
// is embedded, the vector is written to Postgres, and a search outbox entry is
// queued so the outbox worker can sync it to Qdrant.
//
// Returns the number of decisions backfilled. Skips silently if the embedding
// provider is noop (returns 0, nil).
func (s *Service) BackfillEmbeddings(ctx context.Context, batchSize int) (int, error) {
	// Probe the provider — skip entirely if noop.
	if _, err := s.embedder.Embed(ctx, "probe"); errors.Is(err, embedding.ErrNoProvider) {
		return 0, nil
	}

	decs, err := s.db.FindUnembeddedDecisions(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("backfill: find unembedded: %w", err)
	}
	if len(decs) == 0 {
		return 0, nil
	}

	// Build embedding texts (same format as Trace).
	texts := make([]string, len(decs))
	for i, d := range decs {
		texts[i] = d.DecisionType + ": " + d.Outcome
		if d.Reasoning != nil {
			texts[i] += " " + *d.Reasoning
		}
	}

	vecs, err := s.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("backfill: embed batch: %w", err)
	}

	var backfilled int
	for i, d := range decs {
		if err := s.validateEmbeddingDims(vecs[i]); err != nil {
			s.logger.Warn("backfill: dimension mismatch, skipping", "decision_id", d.ID, "error", err)
			continue
		}
		if err := s.db.BackfillEmbedding(ctx, d.ID, d.OrgID, vecs[i]); err != nil {
			s.logger.Warn("backfill: update failed", "decision_id", d.ID, "error", err)
			continue
		}
		backfilled++
	}

	if backfilled > 0 {
		s.logger.Info("backfill: embedded decisions", "count", backfilled, "batch", len(decs))
	}
	return backfilled, nil
}

// BackfillOutcomeEmbeddings populates outcome_embedding for decisions that have
// embedding but no outcome_embedding (Option B). Returns the number backfilled.
func (s *Service) BackfillOutcomeEmbeddings(ctx context.Context, batchSize int) (int, error) {
	if _, err := s.embedder.Embed(ctx, "probe"); errors.Is(err, embedding.ErrNoProvider) {
		return 0, nil
	}

	decs, err := s.db.FindDecisionsMissingOutcomeEmbedding(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("backfill outcome: find: %w", err)
	}
	if len(decs) == 0 {
		return 0, nil
	}

	texts := make([]string, len(decs))
	for i, d := range decs {
		texts[i] = d.Outcome
	}

	vecs, err := s.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("backfill outcome: embed: %w", err)
	}

	var backfilled int
	for i, d := range decs {
		if err := s.validateEmbeddingDims(vecs[i]); err != nil {
			s.logger.Warn("backfill outcome: dimension mismatch", "decision_id", d.ID, "error", err)
			continue
		}
		if err := s.db.BackfillOutcomeEmbedding(ctx, d.ID, d.OrgID, vecs[i]); err != nil {
			s.logger.Warn("backfill outcome: update failed", "decision_id", d.ID, "error", err)
			continue
		}
		backfilled++
	}

	if backfilled > 0 {
		s.logger.Info("backfill: outcome embeddings", "count", backfilled, "batch", len(decs))
	}
	return backfilled, nil
}

// generateClaims splits an outcome into sentence-level claims, embeds each,
// and stores them in the decision_claims table. Skips if claims already exist.
func (s *Service) generateClaims(ctx context.Context, decisionID, orgID uuid.UUID, outcome string) error {
	// Skip if claims already exist for this decision.
	exists, err := s.db.HasClaimsForDecision(ctx, decisionID)
	if err != nil {
		return fmt.Errorf("claims: check existing: %w", err)
	}
	if exists {
		return nil
	}

	// Split outcome into claims.
	claimTexts := conflicts.SplitClaims(outcome)
	if len(claimTexts) == 0 {
		return nil
	}

	// Embed all claims in a single batch call.
	vecs, err := s.embedder.EmbedBatch(ctx, claimTexts)
	if err != nil {
		return fmt.Errorf("claims: embed batch: %w", err)
	}

	// Build claim records.
	claims := make([]storage.Claim, 0, len(claimTexts))
	for i, text := range claimTexts {
		if i >= len(vecs) {
			break
		}
		if err := s.validateEmbeddingDims(vecs[i]); err != nil {
			s.logger.Warn("claims: dimension mismatch, skipping claim", "decision_id", decisionID, "claim_idx", i, "error", err)
			continue
		}
		emb := vecs[i]
		claims = append(claims, storage.Claim{
			DecisionID: decisionID,
			OrgID:      orgID,
			ClaimIdx:   i,
			ClaimText:  text,
			Embedding:  &emb,
		})
	}

	if len(claims) == 0 {
		return nil
	}

	if err := s.db.InsertClaims(ctx, claims); err != nil {
		return fmt.Errorf("claims: insert: %w", err)
	}
	s.logger.Debug("claims: generated", "decision_id", decisionID, "count", len(claims))
	return nil
}

// BackfillClaims generates sentence-level claim embeddings for decisions that
// have embeddings but no claims yet. Returns the number of decisions processed.
func (s *Service) BackfillClaims(ctx context.Context, batchSize int) (int, error) {
	if _, err := s.embedder.Embed(ctx, "probe"); errors.Is(err, embedding.ErrNoProvider) {
		return 0, nil
	}

	refs, err := s.db.FindDecisionIDsMissingClaims(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("backfill claims: find: %w", err)
	}
	if len(refs) == 0 {
		return 0, nil
	}

	var backfilled int
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			return backfilled, ctx.Err()
		default:
		}
		// Fetch the decision outcome.
		d, err := s.db.GetDecisionForScoring(ctx, ref.ID, ref.OrgID)
		if err != nil {
			s.logger.Warn("backfill claims: get decision failed", "decision_id", ref.ID, "error", err)
			continue
		}
		if err := s.generateClaims(ctx, ref.ID, ref.OrgID, d.Outcome); err != nil {
			s.logger.Warn("backfill claims: generate failed", "decision_id", ref.ID, "error", err)
			continue
		}
		backfilled++
	}

	if backfilled > 0 {
		s.logger.Info("backfill: claims generated", "count", backfilled, "batch", len(refs))
	}
	return backfilled, nil
}

// isDuplicateKey checks if a Postgres error is a unique_violation (23505).
func isDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
