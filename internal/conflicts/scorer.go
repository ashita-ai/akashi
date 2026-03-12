//go:build !lite

// Package conflicts provides semantic conflict detection and scoring.
package conflicts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/storage"
)

// agentContextString extracts a string value from the agent_context JSONB map.
// Returns "" if the map is nil, the key is missing, or the value is not a string.
func agentContextString(ctx map[string]any, key string) string {
	if ctx == nil {
		return ""
	}
	v, ok := ctx[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// uuidString returns the string representation of a UUID pointer, or "" if nil.
func uuidString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// PairwiseScorer performs pairwise conflict scoring between two decisions.
// The default implementation uses the Validator (embedding similarity + LLM confirmation).
// An external implementation may be injected via conflicts.Scorer.WithPairwiseScorer
// to replace the built-in confirmation step (enterprise override).
//
// ScorePair returns (score, explanation, error):
//   - score: 0.0 = no conflict, positive value = conflict detected.
//   - explanation: human-readable reason for the classification.
//   - error: only for transient failures; the caller skips the pair on error.
type PairwiseScorer interface {
	ScorePair(ctx context.Context, a, b model.Decision) (score float32, explanation string, err error)
}

// Scorer finds and scores semantic conflicts for new decisions.
type Scorer struct {
	db              *storage.DB
	logger          *slog.Logger
	threshold       float64
	validator       Validator
	validatorLabel  string // low-cardinality label for OTel attributes
	backfillWorkers int
	decayLambda     float64 // Temporal decay rate. 0 disables decay.
	candidateLimit  int     // Max candidates retrieved from Qdrant per decision.
	finder          search.CandidateFinder
	// pairwiseScorer is an optional external override for the confirmation step.
	// When non-nil, it replaces the built-in Validator-backed scoring for each candidate pair.
	pairwiseScorer PairwiseScorer
	metrics        Metrics

	// Scoring thresholds (configurable via env vars, defaults match package constants).
	claimTopicSimFloor    float64
	claimDivFloor         float64
	decisionTopicSimFloor float64

	// crossEncoder is an optional cross-encoder reranker that pre-filters candidate
	// pairs before LLM validation. Pairs below crossEncoderThreshold are skipped.
	crossEncoder          CrossEncoder
	crossEncoderThreshold float64

	// earlyExitFloor is the minimum pre-LLM significance score for a candidate
	// to be worth examining. Candidates below this floor are skipped during
	// sorted iteration (unless they qualify for the directToScorer bypass).
	// 0 disables early exit. Default: 0.25.
	earlyExitFloor float64
}

// WithCandidateFinder wires a Qdrant-backed CandidateFinder for conflict candidate
// discovery. Without one, ScoreForDecision skips candidate retrieval and inserts
// no conflicts. Must be called before any scoring starts.
func (s *Scorer) WithCandidateFinder(cf search.CandidateFinder) *Scorer {
	s.finder = cf
	return s
}

// WithCandidateLimit overrides the maximum number of candidates retrieved from
// Qdrant per decision (default: 50). Lower values reduce LLM cost when an
// expensive validator is configured; higher values improve recall when using
// embedding-only scoring.
func (s *Scorer) WithCandidateLimit(n int) *Scorer {
	if n > 0 {
		s.candidateLimit = n
	}
	return s
}

// WithScoringThresholds overrides the default claim-level scoring thresholds.
// Zero values are ignored (the default is preserved).
func (s *Scorer) WithScoringThresholds(claimTopicSimFloor, claimDivFloor, decisionTopicSimFloor float64) *Scorer {
	if claimTopicSimFloor > 0 {
		s.claimTopicSimFloor = claimTopicSimFloor
	}
	if claimDivFloor > 0 {
		s.claimDivFloor = claimDivFloor
	}
	if decisionTopicSimFloor > 0 {
		s.decisionTopicSimFloor = decisionTopicSimFloor
	}
	return s
}

// WithPairwiseScorer sets an external pairwise scorer to replace the built-in
// Validator-backed confirmation step. When set, each candidate pair is classified
// by the external scorer instead of the embedding+LLM pipeline. Candidate finding
// via Qdrant still runs in OSS. Must be called before any scoring starts.
func (s *Scorer) WithPairwiseScorer(ps PairwiseScorer) *Scorer {
	s.pairwiseScorer = ps
	return s
}

// WithEarlyExitFloor overrides the minimum pre-LLM significance for the early
// exit optimisation. Candidates are sorted by significance descending; once a
// candidate drops below this floor (and does not qualify for the
// directToScorer bypass), the remaining candidates are skipped. Set to 0 to
// disable early exit. Negative values are ignored. Default: 0.25.
func (s *Scorer) WithEarlyExitFloor(floor float64) *Scorer {
	if floor >= 0 {
		s.earlyExitFloor = floor
	}
	return s
}

// WithCrossEncoder configures a cross-encoder reranking step between significance
// scoring and LLM validation. Pairs scoring below the threshold are skipped
// without an LLM call, reducing validation cost. Only active when using the
// built-in LLM validator (not the enterprise pairwise scorer override or noop).
func (s *Scorer) WithCrossEncoder(ce CrossEncoder, threshold float64) *Scorer {
	s.crossEncoder = ce
	s.crossEncoderThreshold = threshold
	return s
}

// NewScorer creates a conflict scorer. If validator is nil, a NoopValidator is
// used (current behavior: embedding-scored candidates are inserted without LLM
// confirmation). backfillWorkers controls how many decisions are scored
// concurrently during BackfillScoring (default: 4). decayLambda controls the
// temporal decay rate for significance (default: 0.01, 0 disables).
func NewScorer(db *storage.DB, logger *slog.Logger, significanceThreshold float64, validator Validator, backfillWorkers int, decayLambda float64) *Scorer {
	if significanceThreshold <= 0 {
		significanceThreshold = 0.30
	}
	if validator == nil {
		validator = NoopValidator{}
	}
	if backfillWorkers <= 0 {
		backfillWorkers = 4
	}
	s := &Scorer{
		db:                    db,
		logger:                logger,
		threshold:             significanceThreshold,
		validator:             validator,
		validatorLabel:        validatorTypeLabel(validator),
		backfillWorkers:       backfillWorkers,
		decayLambda:           decayLambda,
		candidateLimit:        20,
		earlyExitFloor:        0.25,
		claimTopicSimFloor:    claimTopicSimFloor,
		claimDivFloor:         claimDivFloor,
		decisionTopicSimFloor: decisionTopicSimFloor,
	}
	s.registerMetrics()
	return s
}

// claimTopicSimFloor is the minimum cosine similarity for two claims to be
// considered "about the same thing." Below this, claims are too unrelated
// to constitute a conflict even if they diverge. Empirically tuned against
// 30 real decisions with mxbai-embed-large (1024d): 0.4 produced 157 false
// positives, 0.55 produced 142 (same-codebase claims cluster at 0.55-0.60),
// 0.60 reduces to ~5 with the genuine ReScore contradiction (sim=0.62) retained.
const claimTopicSimFloor = 0.60

// claimDivFloor is the minimum outcome divergence between two claims to be
// considered a genuine disagreement. Below this, the claims effectively agree.
const claimDivFloor = 0.15

// decisionTopicSimFloor is the minimum decision-level topic similarity for
// claim-level scoring to activate. Below this, the decisions are about
// sufficiently different topics that claim-level analysis adds noise.
const decisionTopicSimFloor = 0.7

// pairCache tracks decision pairs that have already been evaluated within a
// single backfill run. This prevents duplicate LLM calls when both sides of
// a pair are processed concurrently (decision A finds B as candidate, and
// decision B finds A as candidate — only one LLM call is needed).
type pairCache struct {
	mu   sync.Mutex
	seen map[[2]uuid.UUID]bool
}

// normalizePair returns a canonical ordering (smaller UUID first) for
// consistent deduplication.
func normalizePair(a, b uuid.UUID) [2]uuid.UUID {
	if bytes.Compare(a[:], b[:]) > 0 {
		return [2]uuid.UUID{b, a}
	}
	return [2]uuid.UUID{a, b}
}

// checkAndMark returns true if the pair was already seen (skip it).
// If not seen, marks it and returns false (proceed with LLM call).
func (p *pairCache) checkAndMark(a, b uuid.UUID) bool {
	key := normalizePair(a, b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen[key] {
		return true
	}
	p.seen[key] = true
	return false
}

// ScoreForDecision finds similar decisions, computes significance using both
// full-outcome and claim-level comparison, and inserts the strongest conflict
// above the threshold. Runs asynchronously; non-fatal errors are logged.
func (s *Scorer) ScoreForDecision(ctx context.Context, decisionID, orgID uuid.UUID) {
	s.scoreForDecision(ctx, decisionID, orgID, nil)
}

// scoreForDecision is the internal implementation. The optional pairCache
// prevents duplicate LLM calls during backfill when multiple goroutines
// process different decisions that find each other as candidates.
func (s *Scorer) scoreForDecision(ctx context.Context, decisionID, orgID uuid.UUID, cache *pairCache) {
	start := time.Now()
	defer func() {
		s.metrics.scoringDuration.Record(ctx, float64(time.Since(start).Milliseconds()))
	}()

	d, err := s.db.GetDecisionForScoring(ctx, decisionID, orgID)
	if err != nil {
		s.logger.Debug("conflict scorer: skip decision", "decision_id", decisionID, "error", err)
		return
	}
	if d.Embedding == nil || d.OutcomeEmbedding == nil {
		s.logger.Debug("conflict scorer: decision lacks embeddings", "decision_id", decisionID)
		return
	}

	if s.finder == nil {
		s.logger.Debug("conflict scorer: no candidate finder configured, skipping", "decision_id", decisionID)
		return
	}

	// Build the project scope for candidate search. When the decision has a
	// project, include it plus any linked projects (via project_links table).
	// When no project is set, projects stays nil → matches only nil-project decisions.
	var projects []string
	if d.Project != nil && *d.Project != "" {
		projects = []string{*d.Project}
		linked, err := s.db.LinkedProjects(ctx, orgID, *d.Project, "conflict_scope")
		if err != nil {
			s.logger.Warn("conflict scorer: linked projects lookup failed", "decision_id", decisionID, "error", err)
			// Continue with just the source project — graceful degradation.
		} else {
			projects = append(projects, linked...)
		}
	}

	qdrantResults, err := s.finder.FindSimilar(ctx, orgID, d.Embedding.Slice(), decisionID, projects, s.candidateLimit)
	if err != nil {
		s.logger.Warn("conflict scorer: qdrant find similar failed", "decision_id", decisionID, "error", err)
		return
	}
	if len(qdrantResults) == 0 {
		return
	}

	// Hydrate candidate IDs from Postgres to get full model data
	// (outcome_embedding, reasoning, agent_context) for the scoring pipeline.
	neighborIDs := make([]uuid.UUID, len(qdrantResults))
	for i, r := range qdrantResults {
		neighborIDs[i] = r.DecisionID
	}
	embMap, err := s.db.GetDecisionEmbeddings(ctx, neighborIDs, orgID)
	if err != nil {
		s.logger.Warn("conflict scorer: hydrate embeddings failed", "decision_id", decisionID, "error", err)
		return
	}

	// GetDecisionEmbeddings returns only decisions with both embeddings present.
	// Fetch full decision data for those IDs to get outcome, reasoning, etc.
	hydratedIDs := make([]uuid.UUID, 0, len(embMap))
	for id := range embMap {
		hydratedIDs = append(hydratedIDs, id)
	}
	candidateMap, err := s.db.GetDecisionsByIDs(ctx, orgID, hydratedIDs)
	if err != nil {
		s.logger.Warn("conflict scorer: hydrate decisions failed", "decision_id", decisionID, "error", err)
		return
	}

	// Assemble ordered candidate list preserving Qdrant ranking.
	// GetDecisionsByIDs doesn't include embedding/outcome_embedding; re-attach from embMap.
	candidates := make([]model.Decision, 0, len(candidateMap))
	for id, cand := range candidateMap {
		if embs, ok := embMap[id]; ok {
			cand.Embedding = &embs[0]
			cand.OutcomeEmbedding = &embs[1]
		}
		candidates = append(candidates, cand)
	}

	if len(candidates) == 0 {
		return
	}

	// Build a set of revision chain IDs to exclude. Intentional revisions
	// (via supersedes_id) are corrections, not conflicts.
	revisionChain := make(map[uuid.UUID]bool)
	if chainIDs, err := s.db.GetRevisionChainIDs(ctx, decisionID, orgID); err == nil {
		for _, id := range chainIDs {
			revisionChain[id] = true
		}
	}

	// Check once whether an LLM validator is active. Used both for the
	// directToLLM bypass below and for the validation gate further down.
	_, isNoop := s.validator.(NoopValidator)
	hasScorer := s.pairwiseScorer != nil || !isNoop

	// --- Pre-computation pass: compute cheap significance for all candidates,
	// then sort descending so the most promising pairs are examined first and
	// early exit can prune the tail. ---
	type candidateScore struct {
		cand       model.Decision
		topicSim   float64
		bestSig    float64
		bestDiv    float64
		bestMethod string
		bestOutA   string
		bestOutB   string
		claimFragA *string
		claimFragB *string
		confWeight float64
		decay      float64
	}

	scored := make([]candidateScore, 0, len(candidates))
	for _, cand := range candidates {
		if cand.OutcomeEmbedding == nil {
			continue
		}
		if revisionChain[cand.ID] {
			continue
		}

		topicSim := cosineSimilarity(d.Embedding.Slice(), cand.Embedding.Slice())
		s.metrics.candidatesEvaluated.Add(ctx, 1)

		// Full-outcome scoring.
		outcomeSim := cosineSimilarity(d.OutcomeEmbedding.Slice(), cand.OutcomeEmbedding.Slice())
		outcomeDiv := math.Max(0, 1.0-outcomeSim)
		outcomeSig := topicSim * outcomeDiv

		bestSig := outcomeSig
		bestDiv := outcomeDiv
		bestMethod := "embedding"
		bestOutA := d.Outcome
		bestOutB := cand.Outcome

		// Claim-level scoring for high topic-similarity pairs.
		var claimFragA, claimFragB *string
		if topicSim >= s.decisionTopicSimFloor {
			claimSig, claimDiv, claimA, claimB := s.bestClaimConflict(ctx, d.ID, cand.ID, orgID, topicSim)
			if claimSig > bestSig {
				bestSig = claimSig
				bestDiv = claimDiv
				bestMethod = "claim"
				bestOutA = claimA
				bestOutB = claimB
				claimFragA = &claimA
				claimFragB = &claimB
				s.metrics.claimLevelWins.Add(ctx, 1)
			}
		}

		// Confidence weighting.
		confWeight := math.Sqrt(float64(d.Confidence) * float64(cand.Confidence))
		bestSig *= confWeight

		// Temporal decay.
		decay := 1.0
		if s.decayLambda > 0 {
			daysBetween := math.Abs(d.ValidFrom.Sub(cand.ValidFrom).Hours() / 24)
			decay = math.Exp(-s.decayLambda * daysBetween)
			bestSig *= decay
		}

		scored = append(scored, candidateScore{
			cand:       cand,
			topicSim:   topicSim,
			bestSig:    bestSig,
			bestDiv:    bestDiv,
			bestMethod: bestMethod,
			bestOutA:   bestOutA,
			bestOutB:   bestOutB,
			claimFragA: claimFragA,
			claimFragB: claimFragB,
			confWeight: confWeight,
			decay:      decay,
		})
	}

	// Sort candidates by pre-computed significance descending so the
	// most promising pairs are examined first and early exit is effective.
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].bestSig > scored[j].bestSig
	})

	// --- Sorted iteration with early exit ---
	examined := 0
	inserted := 0
	for _, sc := range scored {
		// High-topic-similarity pairs bypass the cosine-divergence significance
		// gate when an LLM validator or external pairwise scorer is active.
		// Bi-encoders cannot detect stance opposition for same-topic decisions
		// ("X is correct" vs "X is wrong" embed close together). The LLM is the
		// right classifier for this. See: NLI literature on bi-encoder limits.
		directToScorer := hasScorer && sc.topicSim >= s.decisionTopicSimFloor

		// Early exit: if significance is below the early-exit floor AND this
		// candidate does not qualify for the directToScorer bypass, skip it.
		// When no scorer is active, no candidate can bypass, so we can break
		// immediately (all remaining candidates are worse). When a scorer IS
		// active, later candidates with high topicSim might still qualify via
		// the bypass, so we continue instead of break.
		if s.earlyExitFloor > 0 && sc.bestSig < s.earlyExitFloor && !directToScorer {
			if !hasScorer {
				s.logger.Debug("conflict scorer: early exit (no scorer)",
					"decision_id", decisionID,
					"significance", sc.bestSig,
					"floor", s.earlyExitFloor,
					"remaining", len(scored)-examined)
				break
			}
			continue
		}

		if sc.bestSig < s.threshold && !directToScorer {
			continue
		}

		// Complementary workflow filter: suppress conflict creation when the
		// pair matches a structural pattern (review→fix, same-agent refinement,
		// precedent chain). Applied after scoring but before the confirmation
		// gate to save LLM cost and eliminate false positives that embedding
		// math cannot distinguish from genuine conflicts.
		if isComplementaryWorkflowPair(d, sc.cand) {
			s.metrics.workflowFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: workflow filter suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"type_a", d.DecisionType, "type_b", sc.cand.DecisionType,
				"agent_a", d.AgentID, "agent_b", sc.cand.AgentID)
			continue
		}

		examined++

		cand := sc.cand

		// Cross-encoder reranking: pre-filter before expensive LLM validation.
		// Only applies to the built-in LLM path — skipped when using the
		// enterprise pairwise scorer or when no LLM validator is configured.
		if s.crossEncoder != nil && s.pairwiseScorer == nil && !isNoop {
			ceScore, ceErr := s.crossEncoder.ScoreContradiction(ctx, sc.bestOutA, sc.bestOutB)
			switch {
			case ceErr != nil:
				// Fail-open: cross-encoder failure means proceed to LLM as fallback.
				s.logger.Warn("conflict scorer: cross-encoder failed, falling back to LLM",
					"error", ceErr, "decision_a", decisionID, "decision_b", cand.ID)
			case ceScore < s.crossEncoderThreshold:
				s.logger.Debug("conflict scorer: cross-encoder filtered out pair",
					"score", ceScore, "threshold", s.crossEncoderThreshold,
					"decision_a", decisionID, "decision_b", cand.ID)
				continue
			default:
				s.logger.Debug("conflict scorer: cross-encoder passed pair to LLM",
					"score", ceScore, "threshold", s.crossEncoderThreshold,
					"decision_a", decisionID, "decision_b", cand.ID)
			}
		}

		// Confirmation gate: classify the candidate pair as conflict or not.
		// Priority: (1) external pairwise scorer, (2) built-in LLM validator, (3) noop.
		// NoopValidator always returns "contradiction" (preserving legacy embedding-only behavior).
		bestMethod := sc.bestMethod
		var explanation *string
		var category, severity, relationship *string

		if s.pairwiseScorer != nil {
			// External pairwise scorer (enterprise override).
			if cache != nil && cache.checkAndMark(decisionID, cand.ID) {
				s.logger.Debug("conflict scorer: pair already evaluated, skipping external scorer call",
					"decision_a", decisionID, "decision_b", cand.ID)
				continue
			}
			llmStart := time.Now()
			extScore, extExpl, err := s.pairwiseScorer.ScorePair(ctx, d, cand)
			s.metrics.llmCallDuration.Record(ctx, float64(time.Since(llmStart).Milliseconds()))
			if err != nil {
				result := "error"
				if errors.Is(err, context.DeadlineExceeded) {
					result = "timeout"
				}
				s.metrics.llmCalls.Add(ctx, 1, metric.WithAttributes(
					attribute.String("result", result),
					attribute.String("validator", "external"),
				))
				s.logger.Warn("conflict scorer: external pairwise scorer failed, skipping candidate",
					"error", err, "decision_a", decisionID, "decision_b", cand.ID)
				continue // fail-safe: don't insert unscored conflicts
			}
			s.metrics.llmCalls.Add(ctx, 1, metric.WithAttributes(
				attribute.String("result", "success"),
				attribute.String("validator", "external"),
			))
			if extScore <= 0 {
				s.logger.Debug("conflict scorer: external scorer classified as non-conflict",
					"decision_a", decisionID, "decision_b", cand.ID)
				continue
			}
			bestMethod = "external"
			if extExpl != "" {
				explanation = &extExpl
			}
		} else if !isNoop {
			// Built-in LLM validation gate.
			// Skip LLM call if this pair was already evaluated during backfill.
			if cache != nil && cache.checkAndMark(decisionID, cand.ID) {
				s.logger.Debug("conflict scorer: pair already evaluated, skipping LLM call",
					"decision_a", decisionID, "decision_b", cand.ID)
				continue
			}

			llmStart := time.Now()
			result, err := s.validator.Validate(ctx, ValidateInput{
				OutcomeA:        sc.bestOutA,
				OutcomeB:        sc.bestOutB,
				TypeA:           d.DecisionType,
				TypeB:           cand.DecisionType,
				AgentA:          d.AgentID,
				AgentB:          cand.AgentID,
				CreatedA:        d.ValidFrom,
				CreatedB:        cand.ValidFrom,
				ReasoningA:      derefString(d.Reasoning),
				ReasoningB:      derefString(cand.Reasoning),
				ProjectA:        derefString(d.Project),
				ProjectB:        derefString(cand.Project),
				TaskA:           agentContextString(d.AgentContext, "task"),
				TaskB:           agentContextString(cand.AgentContext, "task"),
				SessionIDA:      uuidString(d.SessionID),
				SessionIDB:      uuidString(cand.SessionID),
				FullOutcomeA:    d.Outcome,
				FullOutcomeB:    cand.Outcome,
				TopicSimilarity: sc.topicSim,
			})
			s.metrics.llmCallDuration.Record(ctx, float64(time.Since(llmStart).Milliseconds()))
			if err != nil {
				llmResult := "error"
				if errors.Is(err, context.DeadlineExceeded) {
					llmResult = "timeout"
				}
				s.metrics.llmCalls.Add(ctx, 1, metric.WithAttributes(
					attribute.String("result", llmResult),
					attribute.String("validator", s.validatorLabel),
				))
				s.logger.Warn("conflict scorer: LLM validation failed, skipping candidate",
					"error", err, "decision_a", decisionID, "decision_b", cand.ID)
				continue // fail-safe: don't insert unvalidated conflicts
			}
			s.metrics.llmCalls.Add(ctx, 1, metric.WithAttributes(
				attribute.String("result", "success"),
				attribute.String("validator", s.validatorLabel),
			))
			if !result.IsConflict() {
				s.logger.Debug("conflict scorer: LLM classified as non-conflict",
					"decision_a", decisionID, "decision_b", cand.ID,
					"relationship", result.Relationship, "explanation", result.Explanation)
				continue
			}
			bestMethod = "llm_v2"
			relationship = &result.Relationship
			if result.Explanation != "" {
				explanation = &result.Explanation
			}
			if result.Category != "" {
				category = &result.Category
			}
			if result.Severity != "" {
				severity = &result.Severity
			}
		}

		kind := model.ConflictKindCrossAgent
		if d.AgentID == cand.AgentID {
			kind = model.ConflictKindSelfContradiction
		}
		// Always store full outcomes on the conflict record, even when the
		// claim method won (claim fragments are used as OutcomeA/B in the
		// ValidateInput for LLM comparison only).
		c := model.DecisionConflict{
			ConflictKind:      kind,
			DecisionAID:       decisionID,
			DecisionBID:       cand.ID,
			OrgID:             orgID,
			AgentA:            d.AgentID,
			AgentB:            cand.AgentID,
			DecisionTypeA:     d.DecisionType,
			DecisionTypeB:     cand.DecisionType,
			OutcomeA:          d.Outcome,
			OutcomeB:          cand.Outcome,
			TopicSimilarity:   ptr(sc.topicSim),
			OutcomeDivergence: ptr(sc.bestDiv),
			Significance:      ptr(sc.bestSig),
			ScoringMethod:     bestMethod,
			Explanation:       explanation,
			Category:          category,
			Severity:          severity,
			Relationship:      relationship,
			ConfidenceWeight:  ptr(sc.confWeight),
			TemporalDecay:     ptr(sc.decay),
			Status:            "open",
			ClaimTextA:        sc.claimFragA,
			ClaimTextB:        sc.claimFragB,
		}

		// Topic-aware group assignment: find or create a group whose
		// representative conflict is semantically similar to this one.
		if d.OutcomeEmbedding != nil {
			topicLabel := storage.TruncateOutcome(d.Outcome, 120)
			groupID, grpErr := s.db.FindOrCreateTopicGroup(ctx,
				orgID, d.AgentID, cand.AgentID, kind,
				d.DecisionType, *d.OutcomeEmbedding, topicLabel,
			)
			if grpErr != nil {
				s.logger.Warn("conflict scorer: topic group lookup failed, falling back",
					"error", grpErr, "decision_a", decisionID, "decision_b", cand.ID)
			} else {
				c.GroupID = &groupID
			}
		}

		// Precedent-aware escalation: check if this conflict contradicts the
		// winning side of a previously resolved conflict. If so, auto-escalate
		// to critical severity and link the reopened resolution.
		if d.OutcomeEmbedding != nil && cand.OutcomeEmbedding != nil {
			match, matchErr := s.db.FindReopenedResolution(ctx, orgID,
				decisionID, cand.ID,
				*d.OutcomeEmbedding, *cand.OutcomeEmbedding,
				0.80, // similarity threshold
			)
			if matchErr != nil {
				s.logger.Warn("conflict scorer: reopened resolution check failed",
					"error", matchErr, "decision_a", decisionID, "decision_b", cand.ID)
			} else if match != nil {
				c.ReopensResolutionID = &match.ResolutionID
				critical := "critical"
				c.Severity = &critical

				// Prepend prior-resolution context to the explanation.
				priorNote := fmt.Sprintf("ESCALATED: contradicts prior resolution (conflict %s) where %s's approach prevailed.",
					match.ResolutionID, match.WinningAgent)
				if c.Explanation != nil {
					combined := priorNote + " " + *c.Explanation
					c.Explanation = &combined
				} else {
					c.Explanation = &priorNote
				}

				// Increment times_reopened on the group.
				if c.GroupID != nil {
					if incErr := s.db.IncrementGroupTimesReopened(ctx, *c.GroupID, orgID); incErr != nil {
						s.logger.Warn("conflict scorer: increment times_reopened failed",
							"error", incErr, "group_id", c.GroupID)
					}
				}

				s.logger.Warn("conflict scorer: precedent reopened",
					"decision_a", decisionID, "decision_b", cand.ID,
					"prior_resolution", match.ResolutionID,
					"winning_agent", match.WinningAgent,
				)
			}
		}

		conflictID, err := s.db.InsertScoredConflict(ctx, c)
		if err != nil {
			s.logger.Warn("conflict scorer: insert failed", "decision_a", decisionID, "decision_b", cand.ID, "error", err)
			continue
		}
		s.metrics.detected.Add(ctx, 1, metric.WithAttributes(
			attribute.String("scoring_method", bestMethod),
			attribute.String("relationship", derefOrUnknown(relationship)),
			attribute.String("conflict_kind", string(kind)),
			attribute.String("severity", derefOrUnknown(c.Severity)),
		))
		s.metrics.significanceDist.Record(ctx, sc.bestSig)
		inserted++

		notifyPayload := `{"source":"scorer","org_id":"` + orgID.String() + `"}`
		if c.ReopensResolutionID != nil {
			notifyPayload = `{"source":"scorer","org_id":"` + orgID.String() +
				`","event":"conflict_reopened","conflict_id":"` + conflictID.String() +
				`","reopens_resolution_id":"` + c.ReopensResolutionID.String() + `"}`
		}
		if err := s.db.Notify(ctx, storage.ChannelConflicts, notifyPayload); err != nil {
			s.logger.Debug("conflict scorer: notify failed", "error", err)
		}
	}
	s.metrics.candidatesExamined.Record(ctx, float64(examined))
	if inserted > 0 {
		s.logger.Info("conflict scorer: scored conflicts", "decision_id", decisionID, "inserted", inserted)
	}

	// Mark this decision as scored so the next backfill skips it.
	if err := s.db.MarkDecisionConflictScored(ctx, decisionID, orgID); err != nil {
		s.logger.Warn("conflict scorer: mark scored failed", "decision_id", decisionID, "error", err)
	}
}

// bestClaimConflict finds the most significant claim-level conflict between
// two decisions. Returns (significance, divergence, claimTextA, claimTextB).
// If no claim pairs qualify, returns (0, 0, "", "").
func (s *Scorer) bestClaimConflict(ctx context.Context, decisionAID, decisionBID, orgID uuid.UUID, topicSim float64) (float64, float64, string, string) {
	claimsA, err := s.db.FindClaimsByDecision(ctx, decisionAID, orgID)
	if err != nil || len(claimsA) == 0 {
		return 0, 0, "", ""
	}
	claimsB, err := s.db.FindClaimsByDecision(ctx, decisionBID, orgID)
	if err != nil || len(claimsB) == 0 {
		return 0, 0, "", ""
	}

	var bestSig, bestDiv float64
	var bestClaimA, bestClaimB string

	for _, ca := range claimsA {
		if ca.Embedding == nil || !ConflictRelevantCategory(ca.Category) {
			continue
		}
		for _, cb := range claimsB {
			if cb.Embedding == nil || !ConflictRelevantCategory(cb.Category) {
				continue
			}
			claimSim := cosineSimilarity(ca.Embedding.Slice(), cb.Embedding.Slice())
			if claimSim < s.claimTopicSimFloor {
				continue // Claims are about different things.
			}
			claimDiv := 1.0 - claimSim
			if claimDiv < s.claimDivFloor {
				continue // Claims effectively agree.
			}
			// Use decision-level topic similarity scaled by claim divergence.
			// This rewards high overall topic overlap (same codebase/domain)
			// combined with specific claim disagreements.
			sig := topicSim * claimDiv
			if sig > bestSig {
				bestSig = sig
				bestDiv = claimDiv
				bestClaimA = ca.ClaimText
				bestClaimB = cb.ClaimText
			}
		}
	}
	return bestSig, bestDiv, bestClaimA, bestClaimB
}

// BackfillScoring runs conflict scoring for decisions that have embeddings but
// have not yet been scored for conflicts (conflict_scored_at IS NULL). Uses
// parallel workers to reduce wall-clock time when LLM validation is active.
// An in-memory pair cache prevents duplicate LLM calls when both sides of a
// pair are processed concurrently.
//
// Safe to call multiple times — InsertScoredConflict uses ON CONFLICT DO UPDATE,
// and decisions are marked scored after processing.
//
// Returns the number of decisions processed.
func (s *Scorer) BackfillScoring(ctx context.Context, batchSize int) (int, error) {
	refs, err := s.db.FindEmbeddedDecisionIDs(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	if len(refs) == 0 {
		return 0, nil
	}

	cache := &pairCache{seen: make(map[[2]uuid.UUID]bool)}
	var processed atomic.Int32

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(s.backfillWorkers)

	for _, ref := range refs {
		g.Go(func() error {
			select {
			case <-gCtx.Done():
				return gCtx.Err()
			default:
			}
			s.scoreForDecision(gCtx, ref.ID, ref.OrgID, cache)
			processed.Add(1)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return int(processed.Load()), err
	}
	return int(processed.Load()), nil
}

// HasLLMValidator returns true if the scorer has a non-noop validator configured.
func (s *Scorer) HasLLMValidator() bool {
	_, isNoop := s.validator.(NoopValidator)
	return !isNoop
}

// Validator returns the underlying Validator for inspection (e.g., type assertions
// to access provider-specific methods like OllamaValidator.Warmup).
func (s *Scorer) Validator() Validator {
	return s.validator
}

// ClearUnvalidatedConflicts deletes all scored_conflicts that were not validated
// by the current LLM classifier (llm_v2). Old 'llm' (binary verdict) and
// non-LLM conflicts are stale and will be re-scored through the new classifier.
// Returns the number of rows deleted.
// SECURITY: Intentionally global — one-time startup migration that clears stale
// conflicts across all orgs when transitioning to LLM validation.
// Only open/acknowledged conflicts are removed; resolved and wont_fix conflicts
// represent explicit human decisions and are preserved.
func (s *Scorer) ClearUnvalidatedConflicts(ctx context.Context) (int, error) {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("conflicts: begin clear unvalidated tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`DELETE FROM scored_conflicts
		 WHERE scoring_method NOT IN ('llm_v2')
		   AND status IN ('open', 'acknowledged')`)
	if err != nil {
		return 0, fmt.Errorf("conflicts: clear unvalidated: %w", err)
	}
	n := int(tag.RowsAffected())

	if n > 0 {
		if err := storage.InsertMutationAuditTx(ctx, tx, storage.MutationAuditEntry{
			ActorAgentID: "system",
			ActorRole:    "platform_admin",
			Operation:    "clear_unvalidated_conflicts",
			ResourceType: "scored_conflicts",
			BeforeData:   map[string]any{"count": n},
			AfterData:    map[string]any{"deleted": n},
			Metadata:     map[string]any{"reason": "scoring_method_migration_to_llm_v2"},
		}); err != nil {
			return 0, fmt.Errorf("conflicts: audit clear unvalidated: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("conflicts: commit clear unvalidated: %w", err)
	}
	s.logger.Warn("startup: cleared unvalidated conflicts", "deleted", n, "reason", "scoring_method_migration_to_llm_v2")
	return n, nil
}

// CosineSimilarity computes the cosine similarity between two float32 vectors.
// Returns 0 if the vectors have different lengths, are empty, or either has zero norm.
func CosineSimilarity(a, b []float32) float64 {
	return cosineSimilarity(a, b)
}

func cosineSimilarity(a, b []float32) float64 {
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

// ClearAllConflicts deletes open and acknowledged scored_conflicts regardless
// of scoring method, forcing re-evaluation of all pending decision pairs.
// Conflicts with status 'resolved' or 'wont_fix' represent explicit human
// decisions and are preserved — they are never wiped by this operation.
// Returns the number of rows deleted.
// SECURITY: Intentionally global — one-time startup operation across all orgs.
// Set AKASHI_FORCE_CONFLICT_RESCORE=true to trigger.
func (s *Scorer) ClearAllConflicts(ctx context.Context) (int, error) {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("conflicts: begin clear all tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`DELETE FROM scored_conflicts WHERE status IN ('open', 'acknowledged')`)
	if err != nil {
		return 0, fmt.Errorf("conflicts: clear all: %w", err)
	}
	n := int(tag.RowsAffected())

	if n > 0 {
		if err := storage.InsertMutationAuditTx(ctx, tx, storage.MutationAuditEntry{
			ActorAgentID: "system",
			ActorRole:    "platform_admin",
			Operation:    "clear_all_conflicts",
			ResourceType: "scored_conflicts",
			BeforeData:   map[string]any{"count": n},
			AfterData:    map[string]any{"deleted": n},
			Metadata:     map[string]any{"reason": "AKASHI_FORCE_CONFLICT_RESCORE"},
		}); err != nil {
			return 0, fmt.Errorf("conflicts: audit clear all: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("conflicts: commit clear all: %w", err)
	}
	s.logger.Warn("startup: cleared all open/acknowledged conflicts", "deleted", n, "reason", "AKASHI_FORCE_CONFLICT_RESCORE")
	return n, nil
}

// isComplementaryWorkflowPair returns true if the decision pair matches a
// structural pattern where two decisions are about the same topic but are
// complementary rather than contradictory. These patterns are invisible to
// embedding-based scoring because topic similarity is high and outcome text
// diverges (review language vs implementation language).
//
// Three heuristics, any of which is sufficient:
//
//  1. Temporal workflow: one decision is a review/assessment/investigation type
//     and the other is an implementation/fix type, with the implementation
//     recorded after the review. This covers review→fix and assessment→implementation.
//
//  2. Same-agent refinement: both decisions are from the same agent and the
//     newer one's outcome contains keywords indicating it built on the older
//     one ("implemented", "fixed", "resolved", "completed", "addressed").
//
//  3. Precedent chain: one decision cites the other via precedent_ref,
//     meaning the agent explicitly linked them as cause-and-effect.
func isComplementaryWorkflowPair(d, cand model.Decision) bool {
	// Heuristic 3: Precedent chain. If either decision cites the other,
	// they are linked by design — not conflicting.
	if cand.PrecedentRef != nil && *cand.PrecedentRef == d.ID {
		return true
	}
	if d.PrecedentRef != nil && *d.PrecedentRef == cand.ID {
		return true
	}

	// Determine temporal order: earlier and later decision.
	earlier, later := d, cand
	if cand.ValidFrom.Before(d.ValidFrom) {
		earlier, later = cand, d
	}

	// Heuristic 1: Temporal workflow — review/assessment type followed by
	// implementation/fix type.
	if isDirectionalWorkflowPair(earlier.DecisionType, later.DecisionType) {
		return true
	}

	// Heuristic 2: Same-agent refinement with outcome keywords.
	if d.AgentID == cand.AgentID {
		lowerOutcome := strings.ToLower(later.Outcome)
		for _, kw := range refinementKeywords {
			if strings.Contains(lowerOutcome, kw) {
				return true
			}
		}
	}

	return false
}

// reviewTypes are decision types that represent analysis/review/investigation work.
var reviewTypes = map[string]bool{
	"code_review":   true,
	"assessment":    true,
	"investigation": true,
	"review":        true,
	"analysis":      true,
	"audit":         true,
}

// implementationTypes are decision types that represent fix/implementation work.
var implementationTypes = map[string]bool{
	"architecture":   true,
	"bug_fix":        true,
	"fix":            true,
	"implementation": true,
	"refactor":       true,
}

// isDirectionalWorkflowPair returns true if earlierType is a review/assessment
// type and laterType is an implementation/fix type. Unlike isWorkflowPair in
// validator.go (which checks both directions for LLM prompt hints), this check
// is directional: the review must come first temporally.
func isDirectionalWorkflowPair(earlierType, laterType string) bool {
	return reviewTypes[strings.ToLower(earlierType)] && implementationTypes[strings.ToLower(laterType)]
}

// refinementKeywords are outcome substrings that indicate the decision
// built on a prior decision by the same agent.
var refinementKeywords = []string{
	"implemented",
	"fixed",
	"resolved",
	"completed",
	"addressed",
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptr[T any](v T) *T { return &v }

// derefOrUnknown returns the dereferenced string, or "unknown" if nil.
func derefOrUnknown(s *string) string {
	if s == nil {
		return "unknown"
	}
	return *s
}
