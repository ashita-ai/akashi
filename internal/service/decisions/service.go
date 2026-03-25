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
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashita-ai/akashi/internal/conflicts"
	"github.com/ashita-ai/akashi/internal/ctxutil"
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
	db             storage.Store
	embedder       embedding.Provider
	searcher       search.Searcher
	conflictScorer ConflictScorer
	claimExtractor conflicts.ClaimExtractor // nil = fall back to regex SplitClaims
	logger         *slog.Logger

	embeddingDuration      metric.Float64Histogram
	searchDuration         metric.Float64Histogram
	claimEmbeddingFailures metric.Int64Counter
	embeddingSkips         metric.Int64Counter

	percentileCache *search.PercentileCache // nil = use log fallback in ReScore.
	rescoreMetrics  *search.ReScoreMetrics  // nil = skip signal contribution recording.

	// asyncWg tracks in-flight post-trace goroutines (claim generation,
	// conflict scoring) so Shutdown can wait for them before closing the DB.
	asyncWg sync.WaitGroup
	// shutdownCtx is cancelled during DrainAsync to signal background
	// goroutines to abandon work. Replaces context.Background() in
	// postTraceAsync so goroutines respect shutdown.
	shutdownCtx  context.Context
	shutdownStop context.CancelFunc
}

// SetPercentileCache configures the percentile cache used by ReScore for
// distribution-aware citation normalization. Safe to call before Run().
func (s *Service) SetPercentileCache(c *search.PercentileCache) { s.percentileCache = c }

// SetReScoreMetrics configures per-signal contribution metrics for ReScore.
func (s *Service) SetReScoreMetrics(m *search.ReScoreMetrics) { s.rescoreMetrics = m }

// SetClaimExtractor configures an LLM-based claim extractor. When set,
// generateClaims uses the extractor instead of regex-based SplitClaims,
// producing categorized claims (finding, recommendation, assessment, status).
func (s *Service) SetClaimExtractor(e conflicts.ClaimExtractor) { s.claimExtractor = e }

// DrainAsync waits for in-flight post-trace goroutines (claim generation and
// conflict scoring) to complete. If ctx expires first, it cancels the
// goroutines' shared context and returns ctx.Err(). Call this during shutdown
// before closing the database pool.
func (s *Service) DrainAsync(ctx context.Context) error {
	// Check context before waiting — an already-cancelled context must
	// always return an error, even when the WaitGroup counter is zero.
	// Without this, the select below races between <-done and <-ctx.Done()
	// when both are ready simultaneously.
	if err := ctx.Err(); err != nil {
		s.shutdownStop()
		return err
	}
	done := make(chan struct{})
	go func() {
		s.asyncWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.shutdownStop()
		return ctx.Err()
	}
}

// New creates a new decision Service.
// searcher may be nil if Qdrant is not configured (falls back to text search).
// conflictScorer may be nil to disable semantic conflict detection.
func New(db storage.Store, embedder embedding.Provider, searcher search.Searcher, logger *slog.Logger, conflictScorer ConflictScorer) *Service {
	meter := telemetry.Meter("akashi/decisions")
	embDur, _ := meter.Float64Histogram("akashi.embedding.duration",
		metric.WithDescription("Time to generate embeddings (ms)"),
		metric.WithUnit("ms"),
	)
	searchDur, _ := meter.Float64Histogram("akashi.search.duration",
		metric.WithDescription("Time to execute search queries (ms)"),
		metric.WithUnit("ms"),
	)
	claimFail, _ := meter.Int64Counter("akashi.claims.embedding_failures",
		metric.WithDescription("Claim embedding generation failures"),
	)
	embSkips, _ := meter.Int64Counter("akashi.embedding.skips",
		metric.WithDescription("Decisions traced without embedding (provider unavailable or errored)"),
	)
	shutdownCtx, shutdownStop := context.WithCancel(context.Background())
	return &Service{
		db:                     db,
		embedder:               embedder,
		searcher:               searcher,
		conflictScorer:         conflictScorer,
		logger:                 logger,
		embeddingDuration:      embDur,
		searchDuration:         searchDur,
		claimEmbeddingFailures: claimFail,
		embeddingSkips:         embSkips,
		shutdownCtx:            shutdownCtx,
		shutdownStop:           shutdownStop,
	}
}

// TraceInput contains the data needed to record a decision.
type TraceInput struct {
	AgentID         string
	TraceID         *string
	Metadata        map[string]any
	Decision        model.TraceDecision
	PrecedentRef    *uuid.UUID
	PrecedentReason *string
	SessionID       *uuid.UUID     // MCP session or X-Akashi-Session header.
	AgentContext    map[string]any // Merged server-extracted + client-supplied context.
	APIKeyID        *uuid.UUID     // Managed API key that authenticated this request.

	// AuditMeta, when non-nil, causes the trace to include a mutation audit
	// record inside the same transaction. This closes the gap where mutations
	// could commit without an audit trail.
	AuditMeta *ctxutil.AuditMeta
}

// TraceResult is the outcome of recording a decision.
type TraceResult struct {
	RunID      uuid.UUID
	DecisionID uuid.UUID
	EventCount int
	// Decision is the full model as stored, available for event hooks.
	// Populated by Trace() and AdjudicateConflictWithTrace().
	Decision model.Decision
	// EmbeddingSkipped is true when the embedding provider was unavailable or
	// returned an error. Conflict detection and semantic search may be degraded
	// for this decision.
	EmbeddingSkipped bool
}

// Trace records a complete decision with its alternatives and evidence.
// Embeddings and quality scores are computed first, then all database writes
// happen atomically within a single transaction. Notification is sent after commit.
func (s *Service) Trace(ctx context.Context, orgID uuid.UUID, input TraceInput) (TraceResult, error) {
	params, err := s.prepareTrace(ctx, orgID, input)
	if err != nil {
		return TraceResult{}, err
	}

	var run model.AgentRun
	var decision model.Decision
	err = storage.WithRetry(ctx, 3, 10*time.Millisecond, func() error {
		var txErr error
		run, decision, txErr = s.db.CreateTraceTx(ctx, params)
		return txErr
	})
	if err != nil {
		return TraceResult{}, fmt.Errorf("trace: %w", err)
	}

	s.postTraceAsync(ctx, orgID, input, decision)
	return TraceResult{
		RunID:            run.ID,
		DecisionID:       decision.ID,
		EventCount:       len(params.Alternatives) + len(params.Evidence) + 1,
		Decision:         decision,
		EmbeddingSkipped: decision.Embedding == nil,
	}, nil
}

// AdjudicateConflictWithTrace creates an adjudication decision trace AND resolves a
// conflict in a single atomic transaction. This prevents the failure mode where
// an adjudication decision is created but the conflict remains unresolved due to
// a crash between two separate transactions.
func (s *Service) AdjudicateConflictWithTrace(ctx context.Context, orgID uuid.UUID, input TraceInput, conflictParams storage.AdjudicateConflictInTraceParams) (TraceResult, error) {
	params, err := s.prepareTrace(ctx, orgID, input)
	if err != nil {
		return TraceResult{}, err
	}

	var run model.AgentRun
	var decision model.Decision
	err = storage.WithRetry(ctx, 3, 10*time.Millisecond, func() error {
		var txErr error
		run, decision, txErr = s.db.CreateTraceAndAdjudicateConflictTx(ctx, params, conflictParams)
		return txErr
	})
	if err != nil {
		return TraceResult{}, fmt.Errorf("trace+adjudicate: %w", err)
	}

	s.postTraceAsync(ctx, orgID, input, decision)
	return TraceResult{
		RunID:            run.ID,
		DecisionID:       decision.ID,
		EventCount:       len(params.Alternatives) + len(params.Evidence) + 1,
		Decision:         decision,
		EmbeddingSkipped: decision.Embedding == nil,
	}, nil
}

// prepareTrace handles all pre-transaction work: OTEL span, embeddings, quality
// scoring, alternatives, evidence, and audit entry construction. Returns the
// fully-prepared CreateTraceParams ready for a transactional write.
func (s *Service) prepareTrace(ctx context.Context, orgID uuid.UUID, input TraceInput) (storage.CreateTraceParams, error) {
	// 0. Normalize decision_type to lowercase. This is the canonical
	// normalization point — all paths (HTTP, MCP, SDK) converge here.
	input.Decision.DecisionType = strings.ToLower(strings.TrimSpace(input.Decision.DecisionType))

	// 0a. Set OTEL span attributes for trace correlation.
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("akashi.agent_id", input.AgentID),
		attribute.String("akashi.decision_type", input.Decision.DecisionType),
	)
	if input.TraceID != nil {
		span.SetAttributes(attribute.String("akashi.trace_id", *input.TraceID))
	}

	// 1. Generate decision embedding (full) and outcome embedding concurrently.
	embText := input.Decision.DecisionType + ": " + input.Decision.Outcome
	if input.Decision.Reasoning != nil {
		embText += " " + *input.Decision.Reasoning
	}
	var decisionEmb, outcomeEmb *pgvector.Vector
	var decEmbErr error
	var embWg sync.WaitGroup
	embWg.Add(2)
	go func() {
		defer embWg.Done()
		embStart := time.Now()
		emb, err := s.embedder.Embed(ctx, embText)
		if err != nil {
			s.logger.Warn("trace: decision embedding failed, continuing without", "error", err)
			return
		}
		if err := s.validateEmbeddingDims(emb); err != nil {
			decEmbErr = fmt.Errorf("trace: %w (check AKASHI_EMBEDDING_DIMENSIONS config)", err)
			return
		}
		s.embeddingDuration.Record(ctx, float64(time.Since(embStart).Milliseconds()))
		decisionEmb = &emb
	}()
	go func() {
		defer embWg.Done()
		// Outcome-only embedding for precise conflict outcome comparison.
		if outcomeVec, err := s.embedder.Embed(ctx, input.Decision.Outcome); err == nil && s.validateEmbeddingDims(outcomeVec) == nil {
			outcomeEmb = &outcomeVec
		}
	}()
	embWg.Wait()
	if decEmbErr != nil {
		return storage.CreateTraceParams{}, decEmbErr
	}
	if decisionEmb == nil {
		s.embeddingSkips.Add(ctx, 1)
		s.logger.Warn("trace: decision stored without embedding — semantic search and conflict detection degraded",
			"agent_id", input.AgentID,
			"decision_type", input.Decision.DecisionType,
		)
	}

	// 2. Compute quality score.
	qualityScore := quality.Score(input.Decision, input.PrecedentRef != nil)

	// 3. Build alternatives.
	alts := make([]model.Alternative, len(input.Decision.Alternatives))
	for i, a := range input.Decision.Alternatives {
		alts[i] = model.Alternative{
			Label:           a.Label,
			RejectionReason: a.RejectionReason,
		}
	}

	// 4. Build evidence with embeddings (outside tx — may call external API).
	// Parallelize embedding calls since each is an independent API request.
	evs := make([]model.Evidence, len(input.Decision.Evidence))
	if len(input.Decision.Evidence) > 0 {
		var wg sync.WaitGroup
		errs := make([]error, len(input.Decision.Evidence))
		embs := make([]*pgvector.Vector, len(input.Decision.Evidence))
		for i, e := range input.Decision.Evidence {
			if e.Content == "" {
				continue
			}
			wg.Add(1)
			go func(idx int, content string) {
				defer wg.Done()
				vec, err := s.embedder.Embed(ctx, content)
				if err != nil {
					s.logger.Warn("trace: evidence embedding failed", "error", err)
					errs[idx] = err
					return
				}
				if err := s.validateEmbeddingDims(vec); err != nil {
					errs[idx] = fmt.Errorf("trace: evidence %w (check AKASHI_EMBEDDING_DIMENSIONS config)", err)
					return
				}
				embs[idx] = &vec
			}(i, e.Content)
		}
		wg.Wait()

		// Check for dimension validation errors (hard failure).
		for _, err := range errs {
			if err != nil && strings.Contains(err.Error(), "AKASHI_EMBEDDING_DIMENSIONS") {
				return storage.CreateTraceParams{}, err
			}
		}

		for i, e := range input.Decision.Evidence {
			evs[i] = model.Evidence{
				OrgID:          orgID,
				SourceType:     model.SourceType(e.SourceType),
				SourceURI:      e.SourceURI,
				Content:        e.Content,
				RelevanceScore: e.RelevanceScore,
				Metrics:        e.Metrics,
				Embedding:      embs[i],
			}
		}
	}

	// 5. Build optional audit entry for atomic insertion.
	var auditEntry *storage.MutationAuditEntry
	if input.AuditMeta != nil {
		auditEntry = &storage.MutationAuditEntry{
			RequestID:    input.AuditMeta.RequestID,
			OrgID:        input.AuditMeta.OrgID,
			ActorAgentID: input.AuditMeta.ActorAgentID,
			ActorRole:    input.AuditMeta.ActorRole,
			HTTPMethod:   input.AuditMeta.HTTPMethod,
			Endpoint:     input.AuditMeta.Endpoint,
			Operation:    "trace_decision",
			ResourceType: "decision",
			// ResourceID and AfterData are populated by createTraceInTx after
			// the decision ID is generated.
			Metadata: map[string]any{"agent_id": input.AgentID},
		}
	}

	return storage.CreateTraceParams{
		AgentID:  input.AgentID,
		OrgID:    orgID,
		TraceID:  input.TraceID,
		Metadata: input.Metadata,
		Decision: model.Decision{
			DecisionType:      input.Decision.DecisionType,
			Outcome:           input.Decision.Outcome,
			Confidence:        input.Decision.Confidence,
			Reasoning:         input.Decision.Reasoning,
			Embedding:         decisionEmb,
			OutcomeEmbedding:  outcomeEmb,
			CompletenessScore: qualityScore,
			PrecedentRef:      input.PrecedentRef,
			PrecedentReason:   input.PrecedentReason,
			APIKeyID:          input.APIKeyID,
		},
		Alternatives: alts,
		Evidence:     evs,
		SessionID:    input.SessionID,
		AgentContext: input.AgentContext,
		AuditEntry:   auditEntry,
	}, nil
}

// postTraceAsync handles post-commit work: subscriber notification and
// asynchronous claim generation + conflict scoring. All operations are
// non-fatal — the trace is already committed.
func (s *Service) postTraceAsync(ctx context.Context, orgID uuid.UUID, input TraceInput, decision model.Decision) {
	// Notify subscribers (after commit, non-fatal).
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

	// Generate claim-level embeddings for fine-grained conflict detection.
	// Must complete BEFORE conflict scoring so the scorer can use claims.
	if decision.Embedding != nil {
		s.asyncWg.Add(1)
		go func() {
			defer s.asyncWg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					s.logger.Error("trace: claim generation panicked", "panic", rec, "decision_id", decision.ID)
				}
			}()
			claimCtx, cancelClaims := context.WithTimeout(s.shutdownCtx, 60*time.Second)
			defer cancelClaims()
			if err := s.generateClaims(claimCtx, decision.ID, orgID, input.Decision.Outcome); err != nil {
				s.logger.Warn("trace: claim generation failed", "decision_id", decision.ID, "error", err)
				s.claimEmbeddingFailures.Add(claimCtx, 1, metric.WithAttributes(
					attribute.Int("attempt_number", 1),
				))
				markCtx, markCancel := context.WithTimeout(s.shutdownCtx, 5*time.Second)
				if markErr := s.db.MarkClaimEmbeddingFailed(markCtx, decision.ID, orgID); markErr != nil {
					s.logger.Error("trace: failed to mark claim embedding failure", "decision_id", decision.ID, "error", markErr)
				}
				markCancel()
			}
			if s.conflictScorer != nil {
				// Scoring can include local LLM validation (Ollama), which may take
				// longer than claim generation on CPU-only machines.
				scoreCtx, cancelScore := context.WithTimeout(s.shutdownCtx, 2*time.Minute)
				defer cancelScore()
				s.conflictScorer.ScoreForDecision(scoreCtx, decision.ID, orgID)
			}
		}()
	} else if s.conflictScorer != nil {
		// No embeddings available — still try conflict scoring (it will use full-outcome only).
		s.asyncWg.Add(1)
		go func() {
			defer s.asyncWg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					s.logger.Error("trace: conflict scorer panicked", "panic", rec, "decision_id", decision.ID, "org_id", orgID)
				}
			}()
			scoreCtx, cancel := context.WithTimeout(s.shutdownCtx, 2*time.Minute)
			defer cancel()
			s.conflictScorer.ScoreForDecision(scoreCtx, decision.ID, orgID)
		}()
	}
}

// CheckInput holds the parameters for a precedent check.
type CheckInput struct {
	DecisionType string
	Query        string
	AgentID      string
	Project      string
	Limit        int
}

// Check performs a precedent lookup by semantic search or structured query.
func (s *Service) Check(ctx context.Context, orgID uuid.UUID, input CheckInput) (model.CheckResponse, error) {
	if input.Limit <= 0 {
		input.Limit = 5
	}

	// Build the shared filter set (applied on both search and structured query paths).
	// DecisionType is optional — omitting it broadens the search across all types.
	filters := model.QueryFilters{}
	if input.DecisionType != "" {
		dt := input.DecisionType
		filters.DecisionType = &dt
	}
	if input.AgentID != "" {
		filters.AgentIDs = []string{input.AgentID}
	}
	if input.Project != "" {
		filters.Project = &input.Project
	}

	// Run the three independent lookups concurrently.
	var (
		decisions   []model.Decision
		searchErr   error
		conflicts   []model.DecisionConflict
		conflictErr error
		resolutions []model.ConflictResolution
	)
	// Silence the linter — conflictErr is written in the goroutine and checked implicitly
	// via the conflicts slice (nil on error). Keep the variable for future error propagation.
	_ = conflictErr

	var wg sync.WaitGroup

	// 1. Search or structured query.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if input.Query != "" {
			results, err := s.Search(ctx, orgID, input.Query, true, filters, input.Limit)
			if err != nil {
				searchErr = fmt.Errorf("check: search: %w", err)
				return
			}
			for _, sr := range results {
				if sr.SimilarityScore >= 0.3 {
					decisions = append(decisions, sr.Decision)
				}
			}
		} else {
			queried, _, err := s.db.QueryDecisions(ctx, orgID, model.QueryRequest{
				Filters:  filters,
				Include:  []string{"alternatives"},
				OrderBy:  "valid_from",
				OrderDir: "desc",
				Limit:    input.Limit,
			})
			if err != nil {
				searchErr = fmt.Errorf("check: query: %w", err)
				return
			}
			decisions = queried
		}
	}()

	// 2. List open conflicts (filtered in SQL via StatusIn).
	wg.Add(1)
	go func() {
		defer wg.Done()
		conflictFilter := storage.ConflictFilters{
			StatusIn: []string{"open"},
		}
		if input.DecisionType != "" {
			dt := input.DecisionType
			conflictFilter.DecisionType = &dt
		}
		listed, err := s.db.ListConflicts(ctx, orgID, conflictFilter, input.Limit, 0)
		if err != nil {
			conflictErr = err
			s.logger.Warn("check: list conflicts", "error", err)
			return
		}
		conflicts = listed
	}()

	// 3. Fetch prior resolutions (only when decision type is specified).
	if input.DecisionType != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.db.GetResolvedConflictsByType(ctx, orgID, input.DecisionType, input.Limit)
			if err != nil {
				s.logger.Warn("check: get resolved conflicts by type", "decision_type", input.DecisionType, "error", err)
				return
			}
			resolutions = res
		}()
	}

	wg.Wait()

	if searchErr != nil {
		return model.CheckResponse{}, searchErr
	}

	resp := model.CheckResponse{
		HasPrecedent:     len(decisions) > 0,
		Decisions:        decisions,
		Conflicts:        conflicts,
		PriorResolutions: resolutions,
	}

	return resp, nil
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

// hydrateAndReScore fetches full decisions from Postgres, enriches them with outcome signals,
// and applies completeness+outcome+recency re-scoring (spec 36).
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

	// Enrich with outcome signals (3 batched SQL queries, no N+1).
	signals, err := s.db.GetDecisionOutcomeSignalsBatch(ctx, ids, orgID)
	if err != nil {
		return nil, fmt.Errorf("search: outcome signals: %w", err)
	}
	for id, sig := range signals {
		if d, ok := decisions[id]; ok {
			d.SupersessionVelocityHours = sig.SupersessionVelocityHours
			d.PrecedentCitationCount = sig.PrecedentCitationCount
			d.ConflictFate = sig.ConflictFate
			d.AgreementCount = sig.AgreementCount
			d.ConflictCount = sig.ConflictCount
			decisions[id] = d
		}
	}

	// Enrich with assessment summaries for ReScore (explicit correctness signal).
	// Non-fatal: a missing batch is just treated as no assessments.
	assessments, aErr := s.db.GetAssessmentSummaryBatch(ctx, orgID, ids)
	if aErr != nil {
		s.logger.Warn("search: assessment summaries batch", "error", aErr)
	} else {
		for id, sum := range assessments {
			if d, ok := decisions[id]; ok {
				cp := sum
				d.AssessmentSummary = &cp
				decisions[id] = d
			}
		}
	}

	// Build ReScore options: percentile normalization + signal contribution metrics.
	var opts *search.ReScoreOpts
	if s.percentileCache != nil || s.rescoreMetrics != nil {
		opts = &search.ReScoreOpts{Ctx: ctx}
		if s.percentileCache != nil {
			opts.Percentiles = s.percentileCache.Get(orgID)
		}
		if s.rescoreMetrics != nil {
			opts.Metrics = s.rescoreMetrics
		}
	}

	return search.ReScore(results, decisions, limit, opts), nil
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

// ConsensusScores returns agreement and conflict counts for a single decision.
//
// Agreement is computed by:
//  1. Asking Qdrant for the top-50 embedding neighbors of this decision.
//  2. Fetching their outcome_embeddings from Postgres.
//  3. Counting neighbors where outcome cosine similarity ≥ 0.75.
//
// Conflict count is an index-backed join on scored_conflicts (no ANN needed).
// Returns 0 for agreement if Qdrant is not available or the decision has no embedding.
func (s *Service) ConsensusScores(ctx context.Context, decisionID, orgID uuid.UUID) (agreementCount, conflictCount int, err error) {
	conflictCount, err = s.db.GetConflictCount(ctx, decisionID, orgID)
	if err != nil {
		return 0, 0, err
	}

	cf, ok := s.searcher.(search.CandidateFinder)
	if !ok {
		return 0, conflictCount, nil
	}
	if s.searcher.Healthy(ctx) != nil {
		return 0, conflictCount, nil
	}

	// Fetch the source decision's embeddings for the Qdrant query and pairwise comparison.
	embMap, err := s.db.GetDecisionEmbeddings(ctx, []uuid.UUID{decisionID}, orgID)
	if err != nil || len(embMap) == 0 {
		return 0, conflictCount, nil //nolint:nilerr // missing embedding is not an error
	}
	embs := embMap[decisionID]
	sourceOutcome := embs[1]

	results, err := cf.FindSimilar(ctx, orgID, embs[0].Slice(), decisionID, nil, 50)
	if err != nil {
		s.logger.Warn("consensus: qdrant find similar failed", "decision_id", decisionID, "error", err)
		return 0, conflictCount, nil
	}
	if len(results) == 0 {
		return 0, conflictCount, nil
	}

	neighborIDs := make([]uuid.UUID, len(results))
	for i, r := range results {
		neighborIDs[i] = r.DecisionID
	}
	neighborEmbs, err := s.db.GetDecisionEmbeddings(ctx, neighborIDs, orgID)
	if err != nil {
		s.logger.Warn("consensus: fetch neighbor embeddings failed", "error", err)
		return 0, conflictCount, nil
	}

	for _, id := range neighborIDs {
		if n, ok := neighborEmbs[id]; ok {
			if cosineSimFloat32(sourceOutcome.Slice(), n[1].Slice()) >= 0.75 {
				agreementCount++
			}
		}
	}
	return agreementCount, conflictCount, nil
}

// ConsensusScoresBatch returns agreement and conflict counts for multiple decisions.
// Conflict counts are computed via a batch join on scored_conflicts (no ANN).
// Agreement counts use Qdrant for ANN; decisions without Qdrant have agreement=0.
// The returned map includes all input IDs; absent entries have [0,0] scores.
func (s *Service) ConsensusScoresBatch(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID][2]int, error) {
	result := make(map[uuid.UUID][2]int, len(ids))

	// Conflict counts: index-backed join, no ANN needed.
	conflictCounts, err := s.db.GetConflictCountsBatch(ctx, ids, orgID)
	if err != nil {
		return nil, err
	}
	for id, count := range conflictCounts {
		v := result[id]
		v[1] = count
		result[id] = v
	}

	cf, ok := s.searcher.(search.CandidateFinder)
	if !ok || s.searcher.Healthy(ctx) != nil {
		return result, nil
	}

	// Fetch embeddings for all input decisions (only those with both populated).
	embMap, err := s.db.GetDecisionEmbeddings(ctx, ids, orgID)
	if err != nil || len(embMap) == 0 {
		return result, nil //nolint:nilerr
	}

	// Per-decision Qdrant queries. Collect all neighbor IDs for a single batch
	// Postgres fetch, avoiding N+1 embedding lookups.
	neighborResultsByID := make(map[uuid.UUID][]search.Result, len(embMap))
	allNeighborIDs := make(map[uuid.UUID]struct{})
	for id, embs := range embMap {
		results, qErr := cf.FindSimilar(ctx, orgID, embs[0].Slice(), id, nil, 50)
		if qErr != nil {
			s.logger.Warn("consensus batch: qdrant find similar failed", "decision_id", id, "error", qErr)
			continue
		}
		neighborResultsByID[id] = results
		for _, r := range results {
			allNeighborIDs[r.DecisionID] = struct{}{}
		}
	}

	if len(allNeighborIDs) == 0 {
		return result, nil
	}

	// Batch-fetch outcome embeddings for all unique neighbors.
	neighborIDList := make([]uuid.UUID, 0, len(allNeighborIDs))
	for id := range allNeighborIDs {
		neighborIDList = append(neighborIDList, id)
	}
	neighborEmbs, err := s.db.GetDecisionEmbeddings(ctx, neighborIDList, orgID)
	if err != nil {
		s.logger.Warn("consensus batch: fetch neighbor embeddings failed", "error", err)
		return result, nil
	}

	// Compute agreement counts using outcome cosine similarity in Go.
	for id, embs := range embMap {
		neighbors := neighborResultsByID[id]
		agreement := 0
		for _, r := range neighbors {
			if n, ok := neighborEmbs[r.DecisionID]; ok {
				if cosineSimFloat32(embs[1].Slice(), n[1].Slice()) >= 0.75 {
					agreement++
				}
			}
		}
		v := result[id]
		v[0] = agreement
		result[id] = v
	}

	return result, nil
}

// cosineSimFloat32 computes cosine similarity between two float32 vectors.
// Returns 0 for zero-length or mismatched vectors.
func cosineSimFloat32(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		da, db := float64(a[i]), float64(b[i])
		dot += da * db
		normA += da * da
		normB += db * db
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
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
// Returns the resolved or newly created agent so callers can avoid a second
// round-trip to fetch agent metadata (e.g. display name for context enrichment).
// Returns a zero-value model.Agent on non-fatal races (concurrent auto-creation).
//
// When autoRegAudit is non-nil, the auto-registration is recorded in the
// mutation audit log. Callers with HTTP request context should provide this;
// background callers may pass nil.
//
// This eliminates friction when an admin traces on behalf of a new agent for
// the first time — the agent is created implicitly rather than requiring a
// separate POST /v1/agents call.
func (s *Service) ResolveOrCreateAgent(ctx context.Context, orgID uuid.UUID, agentID string, callerRole model.AgentRole, autoRegAudit *storage.MutationAuditEntry) (model.Agent, error) {
	found, err := s.db.GetAgentByAgentID(ctx, orgID, agentID)
	if err == nil {
		return found, nil
	}

	// Only auto-register on not-found errors. Propagate anything else.
	if !errors.Is(err, storage.ErrNotFound) {
		return model.Agent{}, err
	}

	// Non-admin callers cannot auto-register agents.
	if !model.RoleAtLeast(callerRole, model.RoleAdmin) {
		return model.Agent{}, ErrAgentNotFound
	}

	agent := model.Agent{
		AgentID: agentID,
		OrgID:   orgID,
		Name:    agentID,
		Role:    model.RoleAgent,
	}

	// Admin+ caller: auto-register the agent with default role.
	var createErr error
	if autoRegAudit != nil {
		autoRegAudit.Operation = "agent_auto_registered"
		autoRegAudit.ResourceType = "agent"
		autoRegAudit.ResourceID = agentID
		autoRegAudit.AfterData = map[string]any{
			"agent_id": agentID,
			"org_id":   orgID,
			"role":     string(model.RoleAgent),
			"source":   "auto_registration",
		}
		_, createErr = s.db.CreateAgentWithAudit(ctx, agent, *autoRegAudit)
	} else {
		_, createErr = s.db.CreateAgent(ctx, agent)
	}
	if createErr != nil {
		// A concurrent request may have created the same agent between our
		// GetAgentByAgentID and CreateAgent calls. That's fine — treat the
		// duplicate key constraint as success. Return zero agent; the caller
		// will skip any Name-based enrichment, which is acceptable for this
		// rare race condition.
		if s.isDuplicateKey(createErr) {
			return model.Agent{}, nil
		}
		return model.Agent{}, fmt.Errorf("auto-register agent: %w", createErr)
	}

	s.logger.Info("auto-registered agent on first trace", "agent_id", agentID, "org_id", orgID)
	return agent, nil
}

// BackfillEmbeddings generates embeddings for decisions that were stored without
// one (e.g. because the embedding provider was noop at trace time). Each decision
// is embedded, the vector is written to Postgres, and a search outbox entry is
// queued so the outbox worker can sync it to Qdrant.
//
// Returns the number of decisions backfilled. Skips silently if the embedding
// provider is noop (returns 0, nil).
func (s *Service) BackfillEmbeddings(ctx context.Context, batchSize int) (int, error) {
	return s.backfillBatch(ctx, batchSize, backfillSpec{
		find:  s.db.FindUnembeddedDecisions,
		text:  embeddingText,
		write: s.db.BackfillEmbedding,
		label: "backfill: embedded decisions",
	})
}

// BackfillOutcomeEmbeddings populates outcome_embedding for decisions that have
// embedding but no outcome_embedding (Option B). Returns the number backfilled.
func (s *Service) BackfillOutcomeEmbeddings(ctx context.Context, batchSize int) (int, error) {
	return s.backfillBatch(ctx, batchSize, backfillSpec{
		find:  s.db.FindDecisionsMissingOutcomeEmbedding,
		text:  func(d storage.UnembeddedDecision) string { return d.Outcome },
		write: s.db.BackfillOutcomeEmbedding,
		label: "backfill: outcome embeddings",
	})
}

// embeddingText builds the canonical embedding input for a decision (same
// format used by prepareTrace).
func embeddingText(d storage.UnembeddedDecision) string {
	s := d.DecisionType + ": " + d.Outcome
	if d.Reasoning != nil {
		s += " " + *d.Reasoning
	}
	return s
}

// backfillSpec parameterizes the shared backfill loop.
type backfillSpec struct {
	find  func(ctx context.Context, limit int) ([]storage.UnembeddedDecision, error)
	text  func(d storage.UnembeddedDecision) string
	write func(ctx context.Context, id uuid.UUID, orgID uuid.UUID, vec pgvector.Vector) error
	label string
}

// backfillBatch probes the embedding provider, finds records needing backfill,
// embeds them in a single batch, and writes each vector back. Shared by
// BackfillEmbeddings and BackfillOutcomeEmbeddings.
func (s *Service) backfillBatch(ctx context.Context, batchSize int, spec backfillSpec) (int, error) {
	if _, err := s.embedder.Embed(ctx, "probe"); errors.Is(err, embedding.ErrNoProvider) {
		return 0, nil
	}

	decs, err := spec.find(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("%s: find: %w", spec.label, err)
	}
	if len(decs) == 0 {
		return 0, nil
	}

	texts := make([]string, len(decs))
	for i, d := range decs {
		texts[i] = spec.text(d)
	}

	vecs, err := s.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("%s: embed batch: %w", spec.label, err)
	}

	var backfilled int
	for i, d := range decs {
		if err := s.validateEmbeddingDims(vecs[i]); err != nil {
			s.logger.Warn(spec.label+": dimension mismatch, skipping", "decision_id", d.ID, "error", err)
			continue
		}
		if err := spec.write(ctx, d.ID, d.OrgID, vecs[i]); err != nil {
			s.logger.Warn(spec.label+": update failed", "decision_id", d.ID, "error", err)
			continue
		}
		backfilled++
	}

	if backfilled > 0 {
		s.logger.Info(spec.label, "count", backfilled, "batch", len(decs))
	}
	return backfilled, nil
}

// generateClaims extracts claims from an outcome, embeds each, and stores them
// in the decision_claims table. When an LLM extractor is configured, claims are
// extracted with categories (finding, recommendation, assessment, status).
// Otherwise falls back to regex-based SplitClaims (uncategorized).
// Skips if claims already exist.
func (s *Service) generateClaims(ctx context.Context, decisionID, orgID uuid.UUID, outcome string) error {
	// Skip if claims already exist for this decision.
	exists, err := s.db.HasClaimsForDecision(ctx, decisionID, orgID)
	if err != nil {
		return fmt.Errorf("claims: check existing: %w", err)
	}
	if exists {
		return nil
	}

	type textAndCategory struct {
		text     string
		category *string // nil for regex-extracted claims
	}

	var extracted []textAndCategory

	if s.claimExtractor != nil {
		llmClaims, err := s.claimExtractor.ExtractClaims(ctx, outcome)
		if err != nil {
			// Fall back to regex on LLM failure.
			s.logger.Warn("claims: LLM extraction failed, falling back to regex",
				"decision_id", decisionID, "error", err)
		} else {
			for _, c := range llmClaims {
				cat := c.Category
				extracted = append(extracted, textAndCategory{text: c.Text, category: &cat})
			}
		}
	}

	// Fallback: regex-based extraction (no categories).
	if len(extracted) == 0 {
		for _, text := range conflicts.SplitClaims(outcome) {
			extracted = append(extracted, textAndCategory{text: text, category: nil})
		}
	}

	if len(extracted) == 0 {
		return nil
	}

	// Embed all claims in a single batch call.
	texts := make([]string, len(extracted))
	for i, e := range extracted {
		texts[i] = e.text
	}
	vecs, err := s.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("claims: embed batch: %w", err)
	}

	// Build claim records.
	claims := make([]storage.Claim, 0, len(extracted))
	for i, e := range extracted {
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
			ClaimText:  e.text,
			Category:   e.category,
			Embedding:  &emb,
		})
	}

	if len(claims) == 0 {
		return nil
	}

	if err := s.db.InsertClaims(ctx, claims); err != nil {
		return fmt.Errorf("claims: insert: %w", err)
	}
	method := "regex"
	if s.claimExtractor != nil && extracted[0].category != nil {
		method = "llm"
	}
	s.logger.Debug("claims: generated", "decision_id", decisionID, "count", len(claims), "method", method)
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

// RetryFailedClaimEmbeddings re-attempts claim embedding generation for decisions
// that failed previously and are eligible for retry (exponential backoff, capped
// at maxAttempts). On success, clears the failure state and triggers conflict
// scoring. On failure, increments the attempt counter for longer backoff.
// Returns the number of decisions successfully retried.
func (s *Service) RetryFailedClaimEmbeddings(ctx context.Context, batchSize, maxAttempts int) (int, error) {
	if _, err := s.embedder.Embed(ctx, "probe"); errors.Is(err, embedding.ErrNoProvider) {
		return 0, nil
	}

	refs, err := s.db.FindRetriableClaimFailures(ctx, maxAttempts, batchSize)
	if err != nil {
		return 0, fmt.Errorf("retry claims: find: %w", err)
	}
	if len(refs) == 0 {
		return 0, nil
	}

	var retried int
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			return retried, ctx.Err()
		default:
		}

		d, err := s.db.GetDecisionForScoring(ctx, ref.ID, ref.OrgID)
		if err != nil {
			s.logger.Warn("retry claims: get decision failed", "decision_id", ref.ID, "error", err)
			continue
		}

		if err := s.generateClaims(ctx, ref.ID, ref.OrgID, d.Outcome); err != nil {
			s.logger.Warn("retry claims: generate failed", "decision_id", ref.ID, "error", err)
			s.claimEmbeddingFailures.Add(ctx, 1, metric.WithAttributes(
				attribute.Int("attempt_number", ref.Attempts+1),
			))
			if markErr := s.db.MarkClaimEmbeddingFailed(ctx, ref.ID, ref.OrgID); markErr != nil {
				s.logger.Error("retry claims: failed to mark failure", "decision_id", ref.ID, "error", markErr)
			}
			continue
		}

		if err := s.db.ClearClaimEmbeddingFailure(ctx, ref.ID, ref.OrgID); err != nil {
			s.logger.Error("retry claims: failed to clear failure state", "decision_id", ref.ID, "error", err)
		}

		if s.conflictScorer != nil {
			s.conflictScorer.ScoreForDecision(ctx, ref.ID, ref.OrgID)
		}
		retried++
	}

	if retried > 0 {
		s.logger.Info("retry claims: succeeded", "count", retried, "batch", len(refs))
	}
	return retried, nil
}

// isDuplicateKey delegates to the storage backend to check for unique constraint
// violations (Postgres 23505, SQLite CONSTRAINT_UNIQUE, etc.).
func (s *Service) isDuplicateKey(err error) bool {
	return s.db.IsDuplicateKey(err)
}
