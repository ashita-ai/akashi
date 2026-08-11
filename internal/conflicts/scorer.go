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
	"github.com/pgvector/pgvector-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"

	"github.com/ashita-ai/akashi/internal/compact"
	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/search"
	"github.com/ashita-ai/akashi/internal/storage"
)

const (
	// supersedesSourceSameTicket attributes a suggestion to the pre-LLM
	// same-agent same-ticket structural filter, which has no judge verdict.
	supersedesSourceSameTicket = "detector:same_agent_same_ticket"
	// supersedesSourceLLM attributes a suggestion to the LLM validator's
	// supersession verdict, with judge direction and recorded order agreeing.
	supersedesSourceLLM = "detector:llm_supersession"
	// supersedesSourceLLMBackdated is the same verdict where the judge named a
	// superseding side whose valid_from is strictly EARLIER than the side it
	// retires — a backdated trace, a late-filed decision, or a revert. Split
	// into its own source so the two populations can be counted and, when
	// per-source precision is finally measured, scored separately. There is no
	// CHECK on decision_supersedes.suggested_by values, so this needs no migration.
	supersedesSourceLLMBackdated = "detector:llm_supersession_backdated"
	// supersedesReasonMaxDetail bounds the judge language copied into
	// suggested_reason. The column is unbounded TEXT, so this is a payload
	// contract rather than a storage one: internal/mcp/tools.go passes reason
	// to agents verbatim with no truncation. 300 runes matches the bound
	// formatPrompt already applies to reasoning text.
	supersedesReasonMaxDetail = 300
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

	// outcomeSimFloor is the minimum outcome embedding cosine similarity above
	// which two decisions are considered to effectively agree. Pairs at or above
	// this threshold are suppressed as complementary without an LLM call, even
	// when the directToScorer bypass would otherwise apply. 0 disables.
	// Default: 0.85 (matches claim-level agreement: claimDivFloor 0.15 implies
	// agreement at similarity >= 0.85, calibrated against 30 real mxbai-embed-large
	// decisions — see claimDivFloor and defaultOutcomeSimFloor constants).
	outcomeSimFloor float64
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

// WithOutcomeSimFloor overrides the outcome similarity floor. Candidate pairs
// with outcome cosine similarity at or above this value are suppressed as
// complementary (outcomes effectively agree). 0 disables the check. Negative
// values are ignored. Default: 0.85.
func (s *Scorer) WithOutcomeSimFloor(floor float64) *Scorer {
	if floor >= 0 {
		s.outcomeSimFloor = floor
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
		outcomeSimFloor:       defaultOutcomeSimFloor,
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

// defaultOutcomeSimFloor is the minimum outcome embedding cosine similarity
// above which two decisions are considered to effectively agree. Derived from
// the same calibration basis as claimDivFloor: claimDivFloor = 0.15 implies
// that claims with divergence < 0.15 (similarity >= 0.85) are in agreement.
// Full outcomes are longer and more context-rich, so the same threshold
// applies conservatively. See claimDivFloor calibration notes above.
const defaultOutcomeSimFloor = 0.85

// confidenceFloorProduct is the minimum product of confA * confB below which
// candidate pairs are skipped entirely. sqrt(0.0225) = 0.15 — decisions where
// both parties have very low confidence are exploratory and should not trigger
// conflicts. Applied before computing cosine similarities to save CPU.
const confidenceFloorProduct = 0.0225

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

	// Bindings are joined before any text scoring. A declared parameter
	// collision is exact and does not depend on the two decisions looking
	// alike, so it must not be gated behind embedding retrieval — the pairs
	// that declare the same parameter are not necessarily the pairs that
	// score as similar.
	s.scoreBindings(ctx, d, orgID)
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
	// GetDecisionEmbeddings and GetDecisionsByIDs are independent queries on
	// the same ID set — run them in parallel to halve the latency (#554).
	neighborIDs := make([]uuid.UUID, len(qdrantResults))
	for i, r := range qdrantResults {
		neighborIDs[i] = r.DecisionID
	}

	var (
		embMap       map[uuid.UUID][2]pgvector.Vector
		candidateMap map[uuid.UUID]model.Decision
	)
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		embMap, err = s.db.GetDecisionEmbeddings(gCtx, neighborIDs, orgID)
		return err
	})
	g.Go(func() error {
		var err error
		candidateMap, err = s.db.GetDecisionsByIDs(gCtx, orgID, neighborIDs)
		return err
	})
	if err := g.Wait(); err != nil {
		s.logger.Warn("conflict scorer: hydrate candidates failed", "decision_id", decisionID, "error", err)
		return
	}

	// Assemble candidate list: only include decisions present in both maps
	// (i.e. those that have both embeddings). Re-attach embeddings since
	// GetDecisionsByIDs doesn't return them.
	candidates := make([]model.Decision, 0, len(embMap))
	for id, embs := range embMap {
		cand, ok := candidateMap[id]
		if !ok {
			continue
		}
		cand.Embedding = &embs[0]
		cand.OutcomeEmbedding = &embs[1]
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
		outcomeSim float64 // raw outcome embedding cosine similarity (before divergence)
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

		// Confidence floor: skip exploratory decision pairs where both parties
		// have very low confidence. Applied before cosine similarity to save CPU.
		if float64(d.Confidence)*float64(cand.Confidence) < confidenceFloorProduct {
			s.metrics.confidenceFloorFiltered.Add(ctx, 1)
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
			outcomeSim: outcomeSim,
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

	// --- FP pattern cache: lazy-loaded historical FP rates by decision type pair ---
	type typePairKey struct{ a, b string }
	fpRateCache := make(map[typePairKey]float64)
	fpRateLookup := func(typeA, typeB string) float64 {
		// Normalize order so (A,B) and (B,A) share a cache entry.
		a, b := strings.ToLower(typeA), strings.ToLower(typeB)
		if a > b {
			a, b = b, a
		}
		key := typePairKey{a, b}
		if rate, ok := fpRateCache[key]; ok {
			return rate
		}
		rate, sampleSize, err := s.db.GetTypePairFPRate(ctx, orgID, a, b)
		if err != nil || sampleSize < 5 {
			fpRateCache[key] = 0
			return 0
		}
		fpRateCache[key] = rate
		return rate
	}

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

		// FP pattern suppression: when the historical FP rate for this
		// decision type pair exceeds 80%, double the significance threshold.
		// This creates a feedback loop from labeled data — type pairs that
		// consistently produce false positives get stricter gating.
		effectiveThreshold := s.threshold
		fpRate := fpRateLookup(d.DecisionType, sc.cand.DecisionType)
		if fpRate > 0.80 {
			effectiveThreshold *= 2.0
			s.metrics.fpPatternFiltered.Add(ctx, 1)
		}
		if sc.bestSig < effectiveThreshold && !directToScorer {
			continue
		}

		// Complementary workflow filter: suppress conflict creation when the
		// pair matches a structural pattern (review→fix, same-agent refinement,
		// or non-superseding precedent chain). Applied after scoring but before
		// the confirmation gate to save LLM cost and eliminate false positives
		// that embedding math cannot distinguish from genuine conflicts.
		// Precedent-linked pairs with supersession keywords pass through (#452).
		if isComplementaryWorkflowPair(d, sc.cand) {
			s.metrics.workflowFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: workflow filter suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"type_a", d.DecisionType, "type_b", sc.cand.DecisionType,
				"agent_a", d.AgentID, "agent_b", sc.cand.AgentID)
			continue
		}

		// Coordinated change filter: suppress conflict when decisions share
		// the same commit, PR, or branch (within a temporal window). Two
		// decisions from the same commit/PR are coordinated by definition —
		// they implement the same change across different layers (model,
		// handler, storage, UI, docs) and are complementary, not contradictory.
		// See issue #517 for the false positive analysis motivating this.
		if isCoordinatedChange(d, sc.cand) {
			s.metrics.coordinatedFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: coordinated change filter suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"agent_a", d.AgentID, "agent_b", sc.cand.AgentID)
			continue
		}

		// Cross-branch mechanical operation filter: suppress conflict when two
		// decisions on different branches both describe mechanical operations
		// (migration renumbering, rebase conflict resolution). These are
		// parallel correct work, not disagreement. See issue #692.
		if isCrossBranchMechanical(d, sc.cand) {
			s.metrics.crossBranchFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: cross-branch mechanical filter suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"branch_a", nestedContextString(d.AgentContext, "git_branch"),
				"branch_b", nestedContextString(sc.cand.AgentContext, "git_branch"))
			continue
		}

		// Same-branch self-correction filter: when the same agent revises
		// their own decision on the same branch, this is iterative refinement
		// rather than a self-contradiction. Auto-suppress before LLM call.
		// See issue #692 §3.
		if isSameBranchSelfCorrection(d, sc.cand) {
			s.metrics.selfCorrectionFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: same-branch self-correction suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"agent_id", d.AgentID,
				"branch", nestedContextString(d.AgentContext, "git_branch"))
			continue
		}

		// Same-agent same-ticket refinement filter: when the same agent
		// traces a refinement of their own prior decision on the same ticket
		// without setting supersedes_id, treat it as a missed-link refinement
		// rather than a self-contradiction. Broader than the same-branch
		// filter above — refinements can span branches (e.g. layer-2 PR
		// followed by layer-3 PR on the same ticket).
		// See issue #709; the suggestion record (PR-2, #710) is written
		// below so akashi_check can surface it on the agent's next call.
		if isSameAgentSameTicketRefinement(d, sc.cand) {
			s.metrics.supersedesCandidateFiltered.Add(ctx, 1)
			ticket := extractTicketRef(d)
			s.logger.Debug("conflict scorer: same-agent same-ticket refinement suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"agent_id", d.AgentID,
				"ticket_ref", ticket)
			s.recordSupersedesSuggestion(ctx, d, sc.cand, sc.topicSim, ticket)
			continue
		}

		// Cross-agent precedent-linked refinement filter: when two different
		// agents trace decisions on the same ticket and one explicitly cites
		// the other via precedent_ref, the cite IS the declaration that the
		// later decision builds on the earlier — even when the LLM validator
		// (which already sees PrecedentLinked, see validator.go) classifies
		// them as contradiction because the outcomes mention competing
		// implementation details. The agent's explicit cite is a stronger
		// signal than the LLM's inference. Bails on supersession keywords so
		// declared reversals still reach the validator.
		if isCrossAgentPrecedentRefinement(d, sc.cand) {
			s.metrics.crossAgentPrecedentFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: cross-agent precedent refinement suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"agent_a", d.AgentID, "agent_b", sc.cand.AgentID,
				"ticket_ref", extractTicketRef(d))
			continue
		}

		// Temporal re-assessment filter: two review-type decisions on the
		// same project, recorded sufficiently far apart with no precedent
		// link, are re-measurements rather than contradictions. Quantitative
		// metrics in assessments (FP rates, scores, percentages) drift over
		// time without invalidating earlier snapshots. See issue #705.
		if isTemporalReassessment(d, sc.cand) {
			s.metrics.temporalReassessmentFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: temporal re-assessment suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"type_a", d.DecisionType, "type_b", sc.cand.DecisionType,
				"project", derefString(d.Project),
				"delta", d.ValidFrom.Sub(sc.cand.ValidFrom).Abs())
			continue
		}

		// Operational state-progression filter: two operational-execution
		// decisions on the same project, recorded far apart with no reversal
		// vocabulary and no precedent link, are sequential steps in an incident
		// timeline rather than a contradiction. Operational sibling of the
		// temporal re-assessment filter above — operational state mutates over
		// time the way measured metrics drift over time, so an action recorded
		// long ago does not contradict a later one on the same system. This is
		// the dominant live FP class (2026-06 cross-ticket pgstream/CDC
		// over-clustering audit). See isOperationalStateProgression.
		if isOperationalStateProgression(d, sc.cand) {
			s.metrics.operationalProgressionFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: operational state-progression suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"type_a", d.DecisionType, "type_b", sc.cand.DecisionType,
				"project", derefString(d.Project),
				"delta", d.ValidFrom.Sub(sc.cand.ValidFrom).Abs())
			continue
		}

		// Disjoint work-item filter: two work-item-scoped decisions (reviews,
		// plans) on the same project that reference entirely different work
		// items — different PRs or different tickets — examine different
		// code/scope and cannot contradict each other. This is the structural,
		// PR-aware escalation of the disjoint-ticket validator prompt hint
		// (eef7eb2c), which the LLM ignored on dense shared-subsystem
		// vocabulary; it is the dominant 2026-06-23 false-positive class (9 of
		// 19 open) and the mechanism behind the single-review fan-out (a review
		// of "PR #1539" matched against nine other-PR/-ticket decisions).
		// Direction-setting types are excluded so genuine design forks stay
		// detectable. See isDisjointWorkItem.
		if isDisjointWorkItem(d, sc.cand) {
			s.metrics.disjointWorkItemFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: disjoint work-item filter suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"type_a", d.DecisionType, "type_b", sc.cand.DecisionType,
				"project", derefString(d.Project),
				"refs_a", extractWorkItemRefs(d), "refs_b", extractWorkItemRefs(sc.cand))
			continue
		}

		// Disjoint resource filter: two operational/investigation decisions on
		// the same project that reference entirely different infrastructure
		// resources — different connector_/org_ identifiers — act on different
		// data planes and cannot contradict. This is the resource-identity
		// sibling of the disjoint work-item filter above: live incident-recovery
		// traces carry no PR/ticket ref, so that filter never sees them; they
		// name a connector_/org_ token instead. Dominant live FP class
		// (2026-06-24 cross-connector restart-storm over-clustering). Direction-
		// setting types are excluded and the data-loss guard is preserved so
		// genuine forks and data-safety pairs still reach the validator. See
		// isDisjointResource.
		if isDisjointResource(d, sc.cand) {
			s.metrics.disjointResourceFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: disjoint resource filter suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"type_a", d.DecisionType, "type_b", sc.cand.DecisionType,
				"project", derefString(d.Project),
				"refs_a", extractResourceRefs(d), "refs_b", extractResourceRefs(sc.cand))
			continue
		}

		// Outcome similarity floor: when outcome embeddings are nearly
		// identical AND the pair did not qualify for the directToScorer
		// bypass, suppress as complementary. This catches coordinated
		// changes without PR/commit metadata (the fallback for #517).
		// Threshold is calibrated from the same basis as claimDivFloor
		// (see defaultOutcomeSimFloor). 0 disables.
		//
		// Two exceptions prevent this from suppressing genuine conflicts:
		// 1. directToScorer pairs are exempt: the bypass exists to handle
		//    bi-encoder stance blindness ("X is correct" vs "X is wrong"
		//    embed close together). The LLM is the right classifier for these.
		// 2. claim-level divergence: when claim-level scoring found genuine
		//    disagreement, claims provide a more precise signal than
		//    full-outcome similarity.
		if s.outcomeSimFloor > 0 && sc.outcomeSim >= s.outcomeSimFloor &&
			sc.bestMethod != "claim" && !directToScorer {
			s.metrics.outcomeSimFiltered.Add(ctx, 1)
			s.logger.Debug("conflict scorer: outcome similarity floor suppressed pair",
				"decision_a", decisionID, "decision_b", sc.cand.ID,
				"outcome_sim", sc.outcomeSim, "floor", s.outcomeSimFloor)
			continue
		}

		examined++

		cand := sc.cand
		ticketRefsA := extractTicketRefs(d)
		ticketRefsB := extractTicketRefs(cand)

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
		// Priority: (1) external pairwise scorer, (2) built-in LLM validator,
		// (3) noop with claim-level confirmation.
		bestMethod := sc.bestMethod
		var explanation *string
		var category, severity, relationship, disputedQuestion *string

		switch {
		case s.pairwiseScorer != nil:
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

		case !isNoop:
			// Built-in LLM validation gate.
			// Skip LLM call if this pair was already evaluated during backfill.
			if cache != nil && cache.checkAndMark(decisionID, cand.ID) {
				s.logger.Debug("conflict scorer: pair already evaluated, skipping LLM call",
					"decision_a", decisionID, "decision_b", cand.ID)
				continue
			}

			llmStart := time.Now()
			// A is always d and B is always cand. validatorSideDecision maps the
			// judge's REPLACES token back through this correspondence — keep them
			// in sync.
			result, err := s.validator.Validate(ctx, ValidateInput{
				OutcomeA:          sc.bestOutA,
				OutcomeB:          sc.bestOutB,
				TypeA:             d.DecisionType,
				TypeB:             cand.DecisionType,
				AgentA:            d.AgentID,
				AgentB:            cand.AgentID,
				CreatedA:          d.ValidFrom,
				CreatedB:          cand.ValidFrom,
				ReasoningA:        derefString(d.Reasoning),
				ReasoningB:        derefString(cand.Reasoning),
				ProjectA:          derefString(d.Project),
				ProjectB:          derefString(cand.Project),
				TaskA:             agentContextString(d.AgentContext, "task"),
				TaskB:             agentContextString(cand.AgentContext, "task"),
				SessionIDA:        uuidString(d.SessionID),
				SessionIDB:        uuidString(cand.SessionID),
				FullOutcomeA:      d.Outcome,
				FullOutcomeB:      cand.Outcome,
				BranchA:           nestedContextString(d.AgentContext, "git_branch"),
				BranchB:           nestedContextString(cand.AgentContext, "git_branch"),
				TicketRefsA:       ticketRefsA,
				TicketRefsB:       ticketRefsB,
				TopicSimilarity:   sc.topicSim,
				PrecedentLinked:   isPrecedentLinked(d, cand),
				OutcomeSimilarity: sc.outcomeSim,
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
			// Supersession is a lifecycle event, not a standing disagreement:
			// the validator judged that one decision explicitly REPLACES the
			// other (vs CONTRADICTION, where both positions are live and
			// incompatible). Record it as a supersedes suggestion — which drives
			// the supersedes_id link on the agent's next akashi_check (#710) and
			// retires the stale side — instead of opening a conflict a human must
			// triage. Without this, every "closed PR #1543 as obsolete once the
			// work merged via #1545" trace opened a conflict against the earlier
			// plan that proposed #1543: a dominant false-positive class in the
			// 2026-06-23 open-conflict audit, and the mechanism behind the
			// never_superseded=3045/3045 trace-health signal (supersessions were
			// landing in the conflict queue instead of as supersedes links). The
			// validator still RETURNS "supersession" so the IsConflict-based
			// precision/recall eval is unchanged; only the scorer's handling of
			// that verdict changes. Placed before IsConflict() (which counts
			// supersession as actionable) so supersession never reaches insertion.
			if result.Relationship == "supersession" {
				s.metrics.supersessionSuppressed.Add(ctx, 1)
				s.logger.Debug("conflict scorer: supersession recorded as supersedes suggestion, not opened as conflict",
					"decision_a", decisionID, "decision_b", cand.ID,
					"explanation", result.Explanation)
				s.recordSupersedesSuggestionFromValidator(ctx, d, cand, sc.topicSim, result)
				continue
			}
			if !result.IsConflict() {
				// A contract downgrade is not the same event as an honest
				// non-conflict verdict, but both land here. Count it separately —
				// this branch logs at Debug, which is off at the default info
				// level, so without the counter a parser or prompt regression that
				// discards valid verdicts is indistinguishable from a quiet corpus.
				if result.DowngradedBy != "" {
					s.metrics.contractDowngraded.Add(ctx, 1,
						metric.WithAttributes(attribute.String("contract", result.DowngradedBy)))
					s.logger.Info("conflict scorer: verdict downgraded by parser contract",
						"decision_a", decisionID, "decision_b", cand.ID,
						"contract", result.DowngradedBy, "relationship", result.Relationship)
				}
				s.logger.Debug("conflict scorer: LLM classified as non-conflict",
					"decision_a", decisionID, "decision_b", cand.ID,
					"relationship", result.Relationship, "explanation", result.Explanation)
				continue
			}
			bestMethod = "llm_v2"
			relationship = &result.Relationship
			// The disputed question is the validator's justification for a
			// contradiction verdict and the parser rejects verdicts that cannot
			// name one, so carrying it onto the record is what makes the
			// conflict auditable rather than an assertion.
			if result.SharedQuestion != "" {
				q := result.SharedQuestion
				disputedQuestion = &q
			}
			if result.Explanation != "" {
				explanation = &result.Explanation
			}
			if result.Category != "" {
				category = &result.Category
			}
			if result.Severity != "" {
				severity = &result.Severity
			}

		default:
			// Noop validator path: require claim-level confirmation.
			// Full-outcome embeddings are stance-blind ("use Redis" and "avoid
			// Redis" embed close together). Without an LLM, only claim-level
			// scoring can distinguish genuine disagreement from topic overlap.
			// This eliminates the primary source of false positives on the
			// embedding-only path.
			if sc.bestMethod != "claim" {
				s.metrics.noopClaimGateFiltered.Add(ctx, 1)
				s.logger.Debug("conflict scorer: noop claim gate filtered pair",
					"decision_a", decisionID, "decision_b", cand.ID,
					"method", sc.bestMethod, "significance", sc.bestSig)
				continue
			}
		}

		// Severity fallback: when the LLM/external scorer did not provide
		// severity, compute it from decision metadata (type tier, confidence,
		// category). This ensures severity reflects impact independent of the
		// significance score. See ADR-015.
		if severity == nil {
			computed := ComputeSeverity(SeverityInput{
				DecisionTypeA: d.DecisionType,
				DecisionTypeB: cand.DecisionType,
				ConfidenceA:   d.Confidence,
				ConfidenceB:   cand.Confidence,
				Category:      derefString(category),
			})
			if computed != "" {
				severity = &computed
			}
		}

		kind := model.ConflictKindCrossAgent
		if d.AgentID == cand.AgentID {
			kind = model.ConflictKindSelfContradiction
		}
		// Always store full outcomes on the conflict record, even when the
		// claim method won (claim fragments are used as OutcomeA/B in the
		// ValidateInput for LLM comparison only).
		// A conflict cannot exist before both decisions exist. Use the later
		// decision's transaction_time as the earliest possible detection time
		// for group creation (instead of now()).
		earliestAt := d.TransactionTime
		if cand.TransactionTime.After(earliestAt) {
			earliestAt = cand.TransactionTime
		}

		c := model.DecisionConflict{
			ConflictKind:       kind,
			DecisionAID:        decisionID,
			DecisionBID:        cand.ID,
			OrgID:              orgID,
			AgentA:             d.AgentID,
			AgentB:             cand.AgentID,
			DecisionTypeA:      d.DecisionType,
			DecisionTypeB:      cand.DecisionType,
			OutcomeA:           d.Outcome,
			OutcomeB:           cand.Outcome,
			TopicSimilarity:    ptr(sc.topicSim),
			OutcomeDivergence:  ptr(sc.bestDiv),
			Significance:       ptr(sc.bestSig),
			ScoringMethod:      bestMethod,
			Explanation:        explanation,
			Category:           category,
			Severity:           severity,
			Relationship:       relationship,
			ConfidenceWeight:   ptr(sc.confWeight),
			TemporalDecay:      ptr(sc.decay),
			Status:             "open",
			ClaimTextA:         sc.claimFragA,
			ClaimTextB:         sc.claimFragB,
			EarliestPossibleAt: &earliestAt,
			ProjectA:           d.Project,
			ProjectB:           cand.Project,
			DisputedQuestion:   disputedQuestion,
		}

		// Annotate explanation with branch context when both branches are
		// known. This ensures the stored conflict record surfaces branch
		// information even when viewing conflicts outside the scorer context.
		branchA := nestedContextString(d.AgentContext, "git_branch")
		branchB := nestedContextString(cand.AgentContext, "git_branch")
		if branchA != "" && branchB != "" && branchA != branchB {
			note := fmt.Sprintf(" [Branch context: Decision A on %q, Decision B on %q — consider whether this represents parallel work.]", branchA, branchB)
			if c.Explanation != nil {
				annotated := *c.Explanation + note
				c.Explanation = &annotated
			} else {
				c.Explanation = &note
			}
		}
		if note := ticketContextNote(ticketRefsA, ticketRefsB); note != "" {
			if c.Explanation != nil {
				annotated := *c.Explanation + note
				c.Explanation = &annotated
			} else {
				c.Explanation = &note
			}
		}

		// Topic-aware group assignment: find or create a group whose
		// representative conflict is semantically similar to this one.
		if d.OutcomeEmbedding != nil {
			topicLabel := storage.TruncateOutcome(d.Outcome, 120)
			groupID, grpErr := s.db.FindOrCreateTopicGroup(ctx,
				orgID, d.AgentID, cand.AgentID, kind,
				d.DecisionType, *d.OutcomeEmbedding, topicLabel,
				c.EarliestPossibleAt,
			)
			if grpErr != nil {
				s.logger.Warn("conflict scorer: topic group lookup failed, falling back",
					"error", grpErr, "decision_a", decisionID, "decision_b", cand.ID)
			} else {
				c.GroupID = &groupID
			}
		}

		// Transitive group dedup: if both decisions already participate in
		// open or resolved conflicts within the same group, adding A↔B is
		// redundant — the group already captures the full disagreement topology.
		// This eliminates the N×N explosion from supersession chains where
		// iterative refinements generate O(n²) pairwise conflicts.
		if c.GroupID != nil {
			alreadyGrouped, grpErr := s.db.HasGroupParticipation(ctx, orgID, *c.GroupID, decisionID, cand.ID)
			if grpErr != nil {
				s.logger.Warn("conflict scorer: group participation check failed",
					"error", grpErr, "group_id", c.GroupID)
			} else if alreadyGrouped {
				s.metrics.transitiveGroupFiltered.Add(ctx, 1)
				s.logger.Debug("conflict scorer: transitive group dedup suppressed pair",
					"decision_a", decisionID, "decision_b", cand.ID,
					"group_id", c.GroupID)
				continue
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
// Only open conflicts are removed; resolved and false_positive conflicts
// represent explicit human decisions and are preserved.
func (s *Scorer) ClearUnvalidatedConflicts(ctx context.Context) (int, error) {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("conflicts: begin clear unvalidated tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Gold-labelled pairs are excluded from every bulk clear. They are the
	// only ground truth the detector is measured against, nothing in this repo
	// can regenerate them, and a cascade delete would take them silently — the
	// mutation-audit row counts scored_conflicts, not labels, so a partial loss
	// leaves no trace and simply skews the next corpus-projected precision.
	tag, err := tx.Exec(ctx,
		`DELETE FROM scored_conflicts
		 WHERE scoring_method NOT IN ('llm_v2')
		   AND status = 'open'
		   AND NOT EXISTS (
		       SELECT 1 FROM conflict_gold_labels g
		       WHERE g.scored_conflict_id = scored_conflicts.id
		   )`)
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

// ClearAllConflicts deletes open scored_conflicts regardless
// of scoring method, forcing re-evaluation of all pending decision pairs.
// Conflicts with status 'resolved' or 'false_positive' represent explicit human
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

	// Same exclusion as ClearUnvalidatedConflicts: a rescore must never cost us
	// the labelled corpus. Both clear paths are siblings; guarding one is how
	// the other silently becomes the data-loss path.
	tag, err := tx.Exec(ctx,
		`DELETE FROM scored_conflicts
		 WHERE status = 'open'
		   AND NOT EXISTS (
		       SELECT 1 FROM conflict_gold_labels g
		       WHERE g.scored_conflict_id = scored_conflicts.id
		   )`)
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
	s.logger.Warn("startup: cleared all open conflicts", "deleted", n, "reason", "AKASHI_FORCE_CONFLICT_RESCORE")
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
//     meaning the agent explicitly linked them. However, precedent links can
//     represent supersession (reversal) rather than refinement. When the later
//     decision's outcome contains supersession keywords, the pair is passed
//     through to LLM validation instead of being suppressed. See issue #452.
func isComplementaryWorkflowPair(d, cand model.Decision) bool {
	// Heuristic 3: Precedent chain. If either decision cites the other,
	// they are linked by design. But linked decisions can still conflict
	// (supersession). Check the later decision's outcome for reversal signals;
	// if found, let the LLM decide instead of suppressing.
	if isPrecedentLinked(d, cand) {
		later := cand
		if cand.ValidFrom.Before(d.ValidFrom) {
			later = d
		}
		if containsSupersessionKeyword(later.Outcome) {
			return false // probable supersession — let LLM validate
		}
		return true // probable refinement — suppress
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

// isCoordinatedChange returns true when two decisions share provenance metadata
// that proves they are part of the same coordinated change — same commit, same
// PR, or same branch within a short temporal window. This is a binary signal
// (no thresholds) that is semantically correct: decisions from the same PR are
// coordinated by definition, even when they implement different layers (model,
// handler, storage, UI, docs) whose outcome text diverges.
//
// Checked fields (via nestedContextString, which searches client.*, server.*,
// and flat namespaces):
//   - commit_sha: exact match → always coordinated (globally unique)
//   - pr_number: exact match + same project → always coordinated
//   - branch: exact match + same project + decisions within 24h → likely coordinated
//
// PR numbers and branch names are scoped to a repository, not globally unique.
// Two decisions in the same org but different repos sharing pr_number "42" are
// not coordinated. commit_sha is exempt because SHA-256 hashes are globally unique.
//
// Branch alone is a weaker signal (branches are reused across PRs), so it
// requires temporal proximity as a secondary qualifier.
func isCoordinatedChange(d, cand model.Decision) bool {
	// Same commit SHA → definitively coordinated.
	// Commit SHAs are globally unique, no project scoping needed.
	commitA := nestedContextString(d.AgentContext, "commit_sha")
	commitB := nestedContextString(cand.AgentContext, "commit_sha")
	if commitA != "" && commitA == commitB {
		return true
	}

	// For pr_number and branch checks, require same project. PR numbers and
	// branch names are repo-scoped, not globally unique — pr_number "42" in
	// repo-alpha is unrelated to pr_number "42" in repo-beta. When both
	// projects are nil/empty we can't distinguish repos, so we require at
	// least one non-empty project to avoid false suppression in multi-repo orgs.
	projA := derefString(d.Project)
	projB := derefString(cand.Project)
	sameProject := projA == projB && (projA != "" || projB != "")

	// Same PR number + same project → definitively coordinated.
	prA := nestedContextString(d.AgentContext, "pr_number")
	prB := nestedContextString(cand.AgentContext, "pr_number")
	if prA != "" && prA == prB && sameProject {
		return true
	}

	// Same branch + same project + temporal proximity → likely coordinated.
	// 24h window accommodates multi-session work on a single branch
	// while excluding branch reuse across separate PRs (which typically
	// spans days/weeks with a merge in between).
	branchA := nestedContextString(d.AgentContext, "branch")
	branchB := nestedContextString(cand.AgentContext, "branch")
	if branchA != "" && branchA == branchB && sameProject {
		const coordinationWindow = 24 * time.Hour
		timeDelta := d.ValidFrom.Sub(cand.ValidFrom).Abs()
		if timeDelta <= coordinationWindow {
			return true
		}
	}

	return false
}

// mechanicalKeywords are outcome substrings that indicate a mechanical/routine
// operation (migration renumbering, rebase conflict resolution, version bumps)
// rather than a genuine design or architecture decision.
var mechanicalKeywords = []string{
	"renumber",
	"renumbering",
	"renumbered",
	"rebase",
	"rebasing",
	"rebased",
	"merge conflict",
	"merge conflicts",
	"merged origin/main",
	"merged main",
	"version bump",
	"bumped version",
}

// isMechanicalOperation returns true when the outcome text describes a routine
// mechanical operation like migration renumbering or rebase conflict resolution.
// These operations produce semantically similar outcomes across branches but
// represent parallel correct work, not disagreement.
func isMechanicalOperation(outcome string) bool {
	lower := strings.ToLower(outcome)
	for _, kw := range mechanicalKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// isCrossBranchMechanical returns true when two decisions are on different git
// branches and both describe mechanical operations (e.g. migration renumbering
// during a rebase). These are parallel correct work whose resolution is
// determined by merge order, not a genuine conflict.
//
// Requires both decisions to have branch metadata in agent_context and for the
// branches to differ. Same-branch or missing-branch pairs are not affected.
func isCrossBranchMechanical(d, cand model.Decision) bool {
	branchA := nestedContextString(d.AgentContext, "git_branch")
	branchB := nestedContextString(cand.AgentContext, "git_branch")

	if branchA == "" || branchB == "" || branchA == branchB {
		return false
	}

	return isMechanicalOperation(d.Outcome) && isMechanicalOperation(cand.Outcome)
}

// isSameBranchSelfCorrection returns true when the same agent made two
// sequential decisions on the same branch — a self-correction pattern, not a
// contradiction. The later decision supersedes the earlier one as part of
// iterative work on the same branch.
//
// Requires both decisions to have branch metadata, be from the same agent,
// and be on the same branch. The temporal order doesn't matter — the fact
// that the same agent revised their own decision on the same branch is
// sufficient to classify this as a self-correction.
func isSameBranchSelfCorrection(d, cand model.Decision) bool {
	if d.AgentID != cand.AgentID {
		return false
	}

	branchA := nestedContextString(d.AgentContext, "git_branch")
	branchB := nestedContextString(cand.AgentContext, "git_branch")

	return branchA != "" && branchB != "" && branchA == branchB
}

// validatorSideDecision maps the side token the validator named on its REPLACES
// line back onto the scored pair.
//
// The mapping is fixed by the ValidateInput literal in scoreForDecision:
// OutcomeA / TypeA / AgentA / CreatedA / FullOutcomeA / BranchA / TicketRefsA
// are ALL built from d, and every *B field from cand. So "A" is d and "B" is
// cand. Any change to that literal must change this function with it; there is
// no other place the correspondence is recorded.
func validatorSideDecision(side string, d, cand model.Decision) (superseding, superseded model.Decision, ok bool) {
	switch side {
	case "A":
		return d, cand, true
	case "B":
		return cand, d, true
	default:
		return model.Decision{}, model.Decision{}, false
	}
}

// recordSupersedesSuggestion writes the detector-inferred supersedes link for a
// suppressed same-agent same-ticket pair so akashi_check can surface it on the
// agent's next call (PR-2 of #708, see issue #710).
//
// This path runs in the pre-LLM structural-filter block: there is no judge
// verdict, so recorded order is the ONLY direction signal available and the
// reason string says so explicitly. Do not borrow the LLM path's language here
// — the attribution has to stay honest about what produced it.
func (s *Scorer) recordSupersedesSuggestion(ctx context.Context, d, cand model.Decision, topicSim float64, ticket string) {
	superseding, superseded := d, cand
	if cand.ValidFrom.After(d.ValidFrom) {
		superseding, superseded = cand, d
	}
	s.insertSupersedesSuggestion(ctx, superseding, superseded, topicSim,
		supersedesSourceSameTicket,
		fmt.Sprintf("same agent %q, same ticket %q; direction from recorded order (no validator verdict on this path)",
			d.AgentID, ticket))
}

// recordSupersedesSuggestionFromValidator records the supersedes link the LLM
// validator inferred when it classified a pair as a supersession. A supersession
// is a lifecycle event — one decision explicitly replaces another — not a
// standing disagreement between live positions, so it is surfaced as a supersedes
// suggestion (which drives the supersedes_id link on the agent's next
// akashi_check, issue #710) instead of being opened as a conflict a human must
// triage.
//
// Direction comes from the judge's REPLACES line, never from ValidFrom. The
// prompt refuses recency as evidence ("every candidate pair has a time order,
// so ordering alone means nothing") and re-deriving direction from the clock
// here reintroduced exactly that, breaking on backdated traces, late-filed
// decisions, and reverts. When the judge's direction and recorded order
// disagree, that is signal about the trace, not an error to resolve in favour
// of the clock: the row is written with the judge's direction under a distinct
// suggested_by so the two populations stay separable.
func (s *Scorer) recordSupersedesSuggestionFromValidator(ctx context.Context, d, cand model.Decision, topicSim float64, result ValidationResult) {
	superseding, superseded, ok := validatorSideDecision(result.SupersedingSide, d, cand)
	if !ok {
		// ParseValidatorResponse guarantees a supersession verdict carries a
		// side token — it downgrades one that does not. So an empty or
		// unrecognised side here means the ValidationResult did not come from
		// the parser: an in-process validator, or a test double constructing
		// the struct directly.
		//
		// Direction IS the content of this record, so refusing to write it is
		// the only honest option; falling back to ValidFrom is the clock
		// inference this function exists to remove. Logged at error rather than
		// warn because it is a contract violation, not a transient write
		// failure — but it still returns rather than propagating, because this
		// is a fire-and-forget path inside the scoring loop and the documented
		// contract is that suggestion writes never block scoring. Loud with no
		// side effect beats silent with a guessed direction.
		s.metrics.supersessionSideMissing.Add(ctx, 1)
		s.logger.Error("conflict scorer: supersession verdict carried no superseding side, no supersedes suggestion written",
			"decision_a", d.ID, "decision_b", cand.ID,
			"side", result.SupersedingSide,
			"validator", s.validatorLabel)
		return
	}

	// The judge quoted replacement language when it could; its one-sentence
	// explanation is the fallback. Either way the reason carries the judge's
	// own finding instead of a template rendered from an inference we just made.
	detail := result.ReplacementEvidence
	if detail == "" {
		detail = result.Explanation
	}
	detail = compact.Truncate(strings.TrimSpace(detail), supersedesReasonMaxDetail)

	// Decision IDs are included because agent IDs do not disambiguate the sides
	// of a same-agent pair, which is the common case for this detector.
	reason := fmt.Sprintf("LLM validator: %q's decision %s replaces %q's decision %s — %s",
		superseding.AgentID, superseding.ID.String()[:8],
		superseded.AgentID, superseded.ID.String()[:8], detail)

	suggestedBy := supersedesSourceLLM
	// Disagreement is strict: equal timestamps mean the clock has no opinion,
	// not that it dissents.
	if superseded.ValidFrom.After(superseding.ValidFrom) {
		suggestedBy = supersedesSourceLLMBackdated
		reason = "judge direction disagrees with recorded order (the retired decision has the later valid_from): " + reason
		s.metrics.supersessionDirectionDisagreement.Add(ctx, 1)
		s.logger.Warn("conflict scorer: supersedes direction from validator disagrees with recorded order",
			"superseding_id", superseding.ID, "superseded_id", superseded.ID,
			"superseding_valid_from", superseding.ValidFrom,
			"superseded_valid_from", superseded.ValidFrom,
			"side", result.SupersedingSide,
			"validator", s.validatorLabel)
	}

	s.insertSupersedesSuggestion(ctx, superseding, superseded, topicSim, suggestedBy, reason)
}

// insertSupersedesSuggestion is the shared core behind the supersedes-suggestion
// callers. Direction is the CALLER's decision — this function does not infer it
// — because the two callers derive it from different evidence (the judge's
// REPLACES line vs recorded order on a path with no judge). It clamps topicSim
// into the [0,1] confidence contract (cosine similarity is mathematically in
// [-1,1] but the API bounds confidence to [0,1]) and writes fire-and-forget: a
// write failure is logged at warn and never blocks scoring, because the pair has
// already been filtered or redirected away from the open queue by the time this
// runs.
func (s *Scorer) insertSupersedesSuggestion(ctx context.Context, superseding, superseded model.Decision, topicSim float64, suggestedBy, reason string) {
	switch {
	case topicSim < 0:
		topicSim = 0
	case topicSim > 1:
		topicSim = 1
	}
	conf := float32(topicSim)
	inverseExists, err := s.db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID:         superseding.OrgID,
		SupersedingID: superseding.ID,
		SupersededID:  superseded.ID,
		SuggestedBy:   suggestedBy,
		Confidence:    &conf,
		Reason:        reason,
	})
	if err != nil {
		s.logger.Warn("conflict scorer: insert supersedes suggestion failed",
			"superseding_id", superseding.ID, "superseded_id", superseded.ID,
			"suggested_by", suggestedBy, "error", err)
		return
	}
	if inverseExists {
		// The opposite direction is already recorded for this pair. Since the
		// judge now decides direction and is sampled, this means two passes over
		// the same pair disagreed about which decision retired which. Nothing was
		// written. Surface it at WARN rather than resolving it silently — a
		// direction picked by whichever pass ran last is exactly the guessed link
		// this change exists to stop recording.
		s.metrics.supersedesInverseSuppressed.Add(ctx, 1)
		s.logger.Warn("conflict scorer: supersedes suggestion contradicts an existing inverse link, not recorded",
			"superseding_id", superseding.ID, "superseded_id", superseded.ID,
			"suggested_by", suggestedBy)
	}
}

// isSameAgentSameTicketRefinement returns true when the same agent traces
// what looks like a clean refinement of their own prior decision on the same
// ticket without setting supersedes_id. The newer decision likely intends
// to supersede the earlier one but the agent forgot to set the link.
//
// Sibling of isSameBranchSelfCorrection: that filter requires identical
// git_branch metadata. This one is broader — refinements can span branches
// (e.g. layer-2 PR followed by layer-3 PR on the same ticket) so the join key
// is an extracted ticket reference (see extractTicketRef in ticket_extract.go).
//
// Excluded:
//   - precedent-linked pairs (the agent already linked them — let LLM judge)
//   - outcomes containing supersession keywords (probable explicit reversal,
//     not a forgotten link — let LLM judge)
//   - different agents
//   - missing or differing ticket references
//
// Tracks issue #709 (PR-1 of #708). PR-2 (#710) will surface a supersedes_id
// suggestion to the agent via akashi_check.
func isSameAgentSameTicketRefinement(d, cand model.Decision) bool {
	if d.AgentID != cand.AgentID {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	later := d
	if cand.ValidFrom.After(d.ValidFrom) {
		later = cand
	}
	if containsSupersessionKeyword(later.Outcome) {
		return false
	}
	refA := extractTicketRef(d)
	if refA == "" {
		return false
	}
	return refA == extractTicketRef(cand)
}

// isCrossAgentPrecedentRefinement returns true when two different agents
// trace decisions on the same ticket, one explicitly cites the other via
// precedent_ref, and the later outcome contains no supersession keywords.
//
// Cross-agent analogue of isSameAgentSameTicketRefinement: the same-agent
// filter targets forgotten-supersedes within one agent's work; this filter
// targets the case where agent B explicitly declares "I am building on A"
// (by setting precedent_ref to A on the same ticket) yet the LLM validator
// still classifies the pair as contradiction because the two outcomes
// mention competing implementation details.
//
// Conservative by construction. Required signals:
//   - different agents
//   - precedent_ref explicit between the two decisions (either direction)
//   - identical extracted ticket reference on both sides
//   - same project (defense against pathological cross-project precedent_refs)
//   - later outcome free of supersession keywords (so declared reversals
//     and partial supersessions still reach the validator)
//
// Does NOT catch (deliberately):
//   - same-ticket cross-agent pairs without precedent_ref — those may be
//     genuine disagreements (e.g. ARD-1168 broad vs narrow shared-env
//     fallback, which resolved with codex winning)
//   - pairs sharing only a common-ancestor precedent_ref but not citing
//     each other — shared ancestry is too weak a signal to suppress, in
//     line with the 8f5124c8 reversal of bare same-branch suppression
//
// See sibling isSameAgentSameTicketRefinement for the same-agent variant.
func isCrossAgentPrecedentRefinement(d, cand model.Decision) bool {
	if d.AgentID == cand.AgentID {
		return false
	}
	if !isPrecedentLinked(d, cand) {
		return false
	}
	if derefString(d.Project) != derefString(cand.Project) {
		return false
	}
	later := d
	if cand.ValidFrom.After(d.ValidFrom) {
		later = cand
	}
	if containsSupersessionKeyword(later.Outcome) {
		return false
	}
	refA := extractTicketRef(d)
	if refA == "" {
		return false
	}
	return refA == extractTicketRef(cand)
}

// temporalReassessmentWindow is defined in shared.go — it is referenced by the
// lite-compiled validator.go and so must live in an untagged file.

// isTemporalReassessment returns true when two decisions are both
// review/assessment types on the same project, separated by at least
// temporalReassessmentWindow, with neither citing the other and not in
// the same session. This is the re-measurement pattern: the newer
// assessment updates a prior measurement of the same system and naturally
// has different numbers without contradicting the earlier conclusions.
//
// Pairs caught by this filter would otherwise embed close together (same
// domain) and produce a CONTRADICTION verdict from the LLM whenever quoted
// metrics differ — see issue #705. Linked or same-session re-assessments
// are deliberately excluded so the LLM can still classify intentional
// supersessions.
func isTemporalReassessment(d, cand model.Decision) bool {
	if !reviewTypes[strings.ToLower(d.DecisionType)] {
		return false
	}
	if !reviewTypes[strings.ToLower(cand.DecisionType)] {
		return false
	}
	if derefString(d.Project) != derefString(cand.Project) {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	if d.SessionID != nil && cand.SessionID != nil && *d.SessionID == *cand.SessionID {
		return false
	}
	delta := d.ValidFrom.Sub(cand.ValidFrom)
	if delta < 0 {
		delta = -delta
	}
	return delta >= temporalReassessmentWindow
}

// operationalProgressionWindow is the minimum time delta between two
// operational decisions on the same project below which the pair is never
// treated as sequential progression — near-simultaneous competing directives
// are the genuine operational-clash shape and must stay detectable. Beyond the
// window, a large gap is a *weak* signal that the system state has moved on and
// the earlier action no longer describes what the later one is changing.
//
// The signal is only weak, and deliberately not relied on alone: a single
// resource can stay in one incident longer than the window (the live SalesPatriot
// connector_c3fed173 incident ran 2026-05-26 → 06-18), in which case directives
// spanning >7 days are still about the same live system, not unrelated steps.
// Time-apart cannot distinguish that case from genuine progression, so
// isOperationalStateProgression layers other guards on top: the data-loss guard
// exempts high-stakes pairs outright regardless of gap, and the
// precedent/session/supersession guards exempt declared lineage. Matches
// temporalReassessmentWindow (7 days = the SessionStart "recent" window): the
// same "state moved on" rationale applied to operational state instead of
// measured metrics.
const operationalProgressionWindow = 7 * 24 * time.Hour

// isOperationalStateProgression returns true when two decisions are both
// operational-execution types on the same project, separated by at least
// operationalProgressionWindow, with neither framed as a reversal of a prior
// approach and no explicit precedent link between them.
//
// Motivation: the dominant live false-positive class (2026-06 open-conflict
// audit) was cross-ticket operational steps on one hot subsystem
// (pgstream / Debezium / SalesPatriot CDC) clustered by shared vocabulary —
// e.g. "rolled kafka2pg back to its pre-rollout digest" eight days after
// "verified the connector ONLINE". Embeddings place them close (topic_sim
// 0.70-0.85) and the LLM validator returns CONTRADICTION, but a rollback or
// pause executed a week after a prior deploy is the next step in an incident
// timeline, not a disagreement with it.
//
// Operational sibling of isTemporalReassessment: that filter covers
// review-type re-measurements whose numbers drift over time; this one covers
// operational state that mutates over time. Both rest on the same principle —
// an action taken long ago does not contradict a later one just because they
// touch the same system.
//
// Deliberately narrow, to keep genuine conflicts detectable:
//   - both decisions must be operationalTypes. architecture / trade_off /
//     design forks (e.g. the real Debezium-vs-pgstream disagreement) are
//     never eligible, so they still reach the validator. This is why
//     trade_off and bug_fix are excluded even though some operational FPs
//     carry those types — widening would risk suppressing real design forks.
//   - same non-empty project (untagged-vs-untagged pairs are not suppressed)
//   - >= operationalProgressionWindow apart, so near-simultaneous competing
//     directives (the genuine operational-clash shape) stay detectable
//   - neither outcome contains a supersession keyword — an action that
//     explicitly reverts/abandons a prior approach is a declared reversal and
//     reaches the LLM (checked on both sides: the safe, under-suppressing
//     direction)
//   - neither side's task or outcome reports a data-loss / corruption /
//     quarantine event — a data-safety event is categorically high-stakes and
//     must reach the validator regardless of the time gap (checked on both
//     sides, both text fields). This is the guard that keeps same-resource
//     incident disagreements like the live SalesPatriot DATALOSS pause from
//     being silently dropped pre-LLM. See decisionContainsDataLossKeyword.
//   - not precedent-linked and not the same session — explicit links and
//     intra-session pairs are left to the LLM, mirroring isTemporalReassessment
//
// Known limitation: suppression keys on (type, project, gap, keywords) with no
// resource-identity signal, so a same-resource contradiction that mentions no
// data-safety event can still be suppressed once it is past the window. The
// data-loss guard closes the high-stakes slice of that gap — the only slice
// with verified instances in the trail. The complementary different-resource
// slice (and operational pairs inside the same incident window, which this
// filter's gap requirement skips) is now handled pre-LLM by isDisjointResource,
// which keys on the structured connector_/org_ tokens this filter declined to
// parse — those tokens are regular identifiers, not the fragile free-text
// customer names that motivated the original deferral.
//
// Scope: lives only in the cloud scorer (this file is !lite). The lite scorer
// has none of the structural suppression family; porting it is out of scope.
func isOperationalStateProgression(d, cand model.Decision) bool {
	if !operationalTypes[strings.ToLower(d.DecisionType)] {
		return false
	}
	if !operationalTypes[strings.ToLower(cand.DecisionType)] {
		return false
	}
	projA, projB := derefString(d.Project), derefString(cand.Project)
	if projA != projB || (projA == "" && projB == "") {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	if d.SessionID != nil && cand.SessionID != nil && *d.SessionID == *cand.SessionID {
		return false
	}
	if containsSupersessionKeyword(d.Outcome) || containsSupersessionKeyword(cand.Outcome) {
		return false
	}
	// Data-safety guard: never auto-suppress a pair where either side reports a
	// data-loss / corruption / quarantine event. Such a decision is not a routine
	// sequential step, and a same-system pair that includes one is precisely the
	// disagreement that must reach the validator and the conflict queue rather
	// than be dropped pre-LLM with only a Debug log. Both sides checked, across
	// task and outcome (under-suppressing direction), mirroring the supersession
	// guard above. (The supersession guard stays outcome-only on purpose: a
	// reversal verb in a task title — "migrate connector from kafka to pg" — is
	// routine phrasing, and scanning the task for it would re-admit the very FPs
	// this filter kills. The data-safety guard has no such cost: firing it only
	// ever routes a pair to the validator, never suppresses one.)
	if decisionContainsDataLossKeyword(d) || decisionContainsDataLossKeyword(cand) {
		return false
	}
	delta := d.ValidFrom.Sub(cand.ValidFrom)
	if delta < 0 {
		delta = -delta
	}
	return delta >= operationalProgressionWindow
}

// isDisjointWorkItem returns true when two work-item-scoped decisions (reviews,
// plans) on the same project reference entirely different work items — different
// PRs or different tickets — and therefore cannot be contradicting each other.
// A review of PR #1539 and a review of PR #1543 examine different code; a plan
// for ARD-1606 and a plan for ARD-1662 scope different tickets. They routinely
// share dense subsystem vocabulary (cutover, pgstream, freeze, replica-identity,
// shadow-apply) that places them close in embedding space, and the LLM validator
// then manufactures a CONTRADICTION between them.
//
// This is the hard-rule escalation of the disjoint-ticket signal that
// eef7eb2c shipped as a *validator prompt hint* ("DIFFERENT TICKETS ... classify
// as UNRELATED or COMPLEMENTARY unless ..."). The live 2026-06-23 open-conflict
// audit showed the hint is ignored once shared vocabulary is dense enough: 9 of
// 19 open false positives were disjoint-work-item review/planning pairs the
// prompt failed to deflect. Promoting it to a structural pre-LLM suppressor is
// the membership-parity follow-through, and — unlike the ticket-only hint — this
// one is PR-aware via extractWorkItemRefs, which is what closes the dominant
// fan-out hole: the hub decision in that audit (a review of "PR #1539") carries
// NO ARD-style ticket ref at all, so a ticket-only rule could never have matched
// the nine pairs it spawned.
//
// Deliberately narrow, to keep genuine conflicts detectable:
//   - both decisions must be workItemScopedTypes. architecture / trade_off /
//     design forks are never eligible — a cross-cutting design disagreement
//     (e.g. the real Debezium-vs-pgstream fork) can legitimately span work
//     items and must reach the validator. Same exclusion rationale as
//     isOperationalStateProgression.
//   - same non-empty project (PR/ticket numbers are repo-scoped; an untagged
//     pair, or a cross-repo pair sharing a PR number, is never suppressed —
//     mirrors isCoordinatedChange's same-project requirement for pr_number)
//   - both decisions must expose at least one extractable work-item ref, and
//     the two ref sets must be fully disjoint. Any shared PR/ticket (including
//     one outcome naming the other's work item, e.g. "closed #1543 in favor of
//     #1545") makes the sets overlap and the pair reaches the validator.
//   - not precedent-linked — an explicit lineage cite is left to the LLM
//   - neither side's task or outcome reports a data-loss / corruption /
//     quarantine event — a data-safety finding is categorically high-stakes and
//     must never be dropped pre-LLM (both sides, both text fields — the same
//     task+outcome extractWorkItemRefs mines for the work-item refs that make
//     the pair eligible, so a keyword cannot hide in a field the ref scan reads
//     but the guard does not). This is NOT a redundant copy of the operational
//     filter's guard: cross-ticket data-safety contradictions are a real,
//     resolved class in this trail (e.g. two investigations root-causing the
//     same DATALOSS incident to different causes under different tickets), and
//     disjoint work items do not make that pair safe to drop. Same guard, same
//     reasoning as isOperationalStateProgression; see
//     decisionContainsDataLossKeyword.
//
// Deliberately NOT guarded on supersession keywords, unlike the operational and
// refinement siblings. Those siblings have no work-item identity signal, so a
// "replaced X" / "superseded by Y" phrase is their only cue that a pair might be
// a declared reversal that should reach the validator. This filter HAS that
// signal: a genuine cross-work-item supersession names the other work item, so
// the ref sets overlap and the pair already reaches the validator (then the
// post-LLM supersession→suggestion path). Adding a keyword guard here would only
// re-admit the false positives this filter exists to kill — review prose is full
// of code-diff verbs ("replaced the boolean", "dropped the dead reader") that
// describe a diff, not a decision reversal. A vague reversal that names no work
// item is the sole edge this forgoes; it stays suppressed, which is correct (an
// unnamed antecedent cannot be linked anyway). The difference from the siblings
// is intentional, not drift.
//
// Failure mode is under-suppression: a missed PR/ticket extraction leaves the
// ref sets looking smaller (or one side empty), which disables the filter and
// sends the pair to the validator — never a wrongful suppression.
//
// Scope: lives only in the cloud scorer (this file is !lite), alongside the rest
// of the structural suppression family.
func isDisjointWorkItem(d, cand model.Decision) bool {
	if !workItemScopedTypes[strings.ToLower(d.DecisionType)] {
		return false
	}
	if !workItemScopedTypes[strings.ToLower(cand.DecisionType)] {
		return false
	}
	projA, projB := derefString(d.Project), derefString(cand.Project)
	if projA != projB || (projA == "" && projB == "") {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	if decisionContainsDataLossKeyword(d) || decisionContainsDataLossKeyword(cand) {
		return false
	}
	refsA := extractWorkItemRefs(d)
	refsB := extractWorkItemRefs(cand)
	if len(refsA) == 0 || len(refsB) == 0 {
		return false
	}
	return !ticketRefsOverlap(refsA, refsB)
}

// isDisjointResource returns true when two operational / investigation decisions
// on the same project act on entirely different infrastructure resources —
// different connector_/org_ identifiers — and therefore cannot contradict each
// other. A diagnosis of connector_57a42328 and a recovery of connector_9b38b47d
// touch different data planes; they routinely share dense incident vocabulary
// (restart_storm, kafka2pg, replication slot, wal_status, snapshot) that places
// them close in embedding space, and the validator then manufactures a
// CONTRADICTION between two unrelated incidents.
//
// This is the resource-identity sibling of isDisjointWorkItem. That filter keys
// on PR/ticket refs, which live operational traces do not carry — they name a
// connector_/org_ token instead — so it could never see this class. Together
// they are the membership-parity pair: work-item identity for code reviews and
// plans, resource identity for incident recovery. It is the dominant live FP
// class (2026-06-24 cross-connector restart-storm over-clustering) and the
// reason operational conflicts pile up despite the work-item filter.
//
// It also fills the resource-identity gap that isOperationalStateProgression
// documents as deferred: that filter suppresses same-project operational pairs
// only once they are >= operationalProgressionWindow apart, so different-resource
// pairs inside the same incident window (hours apart) slip past it. This filter
// has no time requirement — different resources cannot contradict regardless of
// when the two decisions were traced.
//
// Deliberately narrow, with the same discipline as isDisjointWorkItem:
//   - both decisions must be resourceScopedTypes. architecture / trade_off /
//     design forks are never eligible — a cross-cutting design disagreement can
//     legitimately span resources and must reach the validator.
//   - same non-empty project (operational traces are all project "mono"; an
//     untagged pair is never suppressed), mirroring the sibling filters.
//   - both must expose at least one extractable resource ref, and the two ref
//     sets must be provably disjoint by namespace (see
//     resourceRefsProvablyDisjoint). Disjointness is only judged WITHIN a
//     namespace: a different connector vs connector, or org vs org, is disjoint;
//     a connector vs an org is NOT comparable — the connector may be owned by
//     that org, and the scorer holds no connector→org mapping — so that pair
//     reaches the validator. Any shared connector/org, or any namespace present
//     on only one side, also sends the pair to the validator — a shared
//     resource is exactly where a genuine operational clash lives.
//   - not precedent-linked — an explicit lineage cite is left to the LLM.
//
// No data-safety guard here (removed 2026-07-23). The guard is load-bearing and
// deliberately chosen in the TEMPORAL sibling isOperationalStateProgression
// (decision efc42730): two decisions on the SAME writer a week apart can be one
// incident disagreeing with itself — verified against the SalesPatriot
// connector_c3fed173 DATALOSS pause. That rationale cannot hold here, because a
// connector_/org_ id is the PHYSICAL identity of a data plane: connector_57a42328
// and connector_f924df29 are different incidents by construction, so a DATALOSS
// finding on one can neither be the same incident as, nor contradict, a finding
// on the other. The guard was copied here for "membership parity" with the
// temporal filter, but it only ever governed provably-disjoint pairs (this
// function returns true solely when resourceRefsProvablyDisjoint) and, measured
// against the live corpus, was a false-positive driver: "quarantine"/"corrupt"
// are ordinary pgstream design vocabulary, so the guard re-admitted the exact
// connector-disjoint over-clustering this filter exists to kill. Same-writer
// data-safety disagreements stay protected where the verified evidence put them,
// in isOperationalStateProgression. (isDisjointWorkItem keeps its guard on
// purpose: a ticket is an administrative label, not a physical identity, so two
// disjoint tickets CAN describe one incident — a deliberate, documented
// difference, not drift.)
//
// Failure mode is under-suppression: a decision that names its resource only by
// customer name ("Trellis") and not by a connector_/org_ token yields no ref,
// which disables the filter and sends the pair to the validator. So does a
// cross-namespace pair (a connector on one side, an org on the other) and any
// pair sharing a resource — never a wrongful suppression, because disjointness
// is only ever declared between same-namespace identifiers that genuinely
// differ.
//
// Scope: lives only in the cloud scorer (this file is !lite), alongside the rest
// of the structural suppression family.
func isDisjointResource(d, cand model.Decision) bool {
	if !resourceScopedTypes[strings.ToLower(d.DecisionType)] {
		return false
	}
	if !resourceScopedTypes[strings.ToLower(cand.DecisionType)] {
		return false
	}
	projA, projB := derefString(d.Project), derefString(cand.Project)
	if projA != projB || (projA == "" && projB == "") {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	refsA := extractResourceRefs(d)
	refsB := extractResourceRefs(cand)
	if len(refsA) == 0 || len(refsB) == 0 {
		return false
	}
	return resourceRefsProvablyDisjoint(refsA, refsB)
}

// resourceRefsProvablyDisjoint reports whether two non-empty canonical
// resource-reference sets ("CONNECTOR-<hex>", "ORG-<hex>") describe provably
// different infrastructure — so a pair naming only these resources cannot be
// acting on the same data plane and is safe to suppress pre-LLM.
//
// Disjointness is only provable WITHIN a namespace. CONNECTOR-* names a single
// pipeline; ORG-* names a whole tenant, which owns many connectors. Two
// different connector ids are different pipelines, and two different org ids are
// different tenants — both provably disjoint. But a connector id and an org id
// are NOT comparable: the connector may be owned by that very org, and the
// scorer holds no connector→org mapping to rule it out. The original check
// flattened both namespaces into one set and ran a plain overlap test, so a
// decision naming only connector_57a42328 and one naming only its owning
// org_54b2e846 looked "disjoint" (no string in common) and were suppressed —
// even though they may describe the same incident on the same data plane. That
// is a wrongful pre-LLM suppression, the one failure mode this family exists to
// avoid.
//
// The fix types the refs by namespace and declares disjointness only when the
// comparison is apples-to-apples in every namespace:
//   - both sides must populate exactly the same set of namespaces. A
//     connector-only side and an org-only side — or a connector-only side and a
//     connector+org side — are not comparable, because there is a ref on one
//     side whose ownership relative to the other side is unknown, so the pair
//     reaches the validator.
//   - within every populated namespace the two id sets must be fully disjoint.
//     Any shared connector or shared org makes the pair overlap and reach the
//     validator.
//
// This is strictly more conservative than the flattened check: it can only send
// MORE pairs to the validator, never fewer (the safe, under-suppressing
// direction the whole family is tuned to). The dominant live FP class —
// connector-vs-connector, both sides naming only connectors — is unaffected:
// same namespace set {CONNECTOR}, disjoint ids, still suppressed.
func resourceRefsProvablyDisjoint(refsA, refsB []string) bool {
	byNsA := groupResourceRefsByNamespace(refsA)
	byNsB := groupResourceRefsByNamespace(refsB)
	if len(byNsA) != len(byNsB) {
		return false // different namespaces populated → not comparable
	}
	for ns, idsA := range byNsA {
		idsB, ok := byNsB[ns]
		if !ok {
			return false // a namespace present on one side only → not comparable
		}
		for id := range idsA {
			if _, shared := idsB[id]; shared {
				return false // a shared resource in this namespace → overlap
			}
		}
	}
	return true
}

// groupResourceRefsByNamespace partitions canonical resource refs into id sets
// keyed by their namespace prefix ("CONNECTOR", "ORG"). extractResourceRefs
// produces every ref as "<PREFIX>-<hex>" with exactly one hyphen, so the split
// is unambiguous; a malformed ref carrying no hyphen is skipped rather than
// mis-bucketed.
func groupResourceRefsByNamespace(refs []string) map[string]map[string]struct{} {
	byNs := make(map[string]map[string]struct{}, 2)
	for _, ref := range refs {
		ns, id, ok := strings.Cut(ref, "-")
		if !ok {
			continue
		}
		ids, exists := byNs[ns]
		if !exists {
			ids = make(map[string]struct{}, 1)
			byNs[ns] = ids
		}
		ids[id] = struct{}{}
	}
	return byNs
}

// isPrecedentLinked returns true if either decision cites the other via
// precedent_ref, meaning there is an explicit lineage link between them.
func isPrecedentLinked(d, cand model.Decision) bool {
	if cand.PrecedentRef != nil && *cand.PrecedentRef == d.ID {
		return true
	}
	if d.PrecedentRef != nil && *d.PrecedentRef == cand.ID {
		return true
	}
	return false
}

// supersessionKeywords are outcome substrings that indicate the decision
// reverses or replaces a prior decision rather than refining it.
//
// The "walk-back" group (shelved, dropped, pivoting, …) was added after the
// #717 FP audit observed agents stating their own reversal in plain English
// — "Shelved the draft; pivoting to a different topic" — without the
// existing same-agent same-ticket filter recognising the reversal because
// the keyword list missed the verb.
var supersessionKeywords = []string{
	"switched",
	"superseding",
	"superseded",
	"replaced",
	"replacing",
	"reversed",
	"reversing",
	"reverted",
	"reverting",
	"migrated from",
	"migrating from",
	"instead of",
	"no longer",
	"abandoned",
	"abandoning",
	// Walk-back / pivot vocabulary surfaced by issue #717 FP audit.
	"shelved",
	"shelving",
	"dropped",
	"dropping",
	"pivoting",
	"pivoted",
	"walked back",
	"walking back",
	"rewrote",
	"rewriting",
	"rewritten",
	"scrapped",
	"scrapping",
	"discarded",
	"discarding",
	"retracted",
	"retracting",
	"withdrew",
	"withdrawing",
	"tabled",
	"parked",
	"deprecated this",
	"deprecating",
}

// containsSupersessionKeyword returns true if the outcome text contains any
// keyword indicating the decision reverses or replaces a prior one.
func containsSupersessionKeyword(outcome string) bool {
	lower := strings.ToLower(outcome)
	for _, kw := range supersessionKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// dataLossKeywords are outcome substrings that mark an operational decision as
// describing a data-safety event (loss, corruption, or quarantined/skipped
// writes) rather than a routine state change. isOperationalStateProgression
// refuses to suppress any pair where either side carries one: a data-safety
// event is categorically high-stakes, and silently dropping a same-system pair
// that reports one — before the LLM validator or a human ever sees it — would
// trade the system's core guarantee (a complete paper trail of disagreements)
// for noise reduction. Surfaced by the 2026-06 SalesPatriot connector_c3fed173
// incident, where "paused … active target-side DATALOSS quarantine growth" sat
// >7 days from "scaled … resume loses no data" on the same writer.
//
// Deliberately broad and matched on both sides (the under-suppressing
// direction): even a reassuring "verified no data loss" disables suppression
// for the pair, which is correct — if data safety is in question at all, the
// pair belongs in front of the validator.
var dataLossKeywords = []string{
	"dataloss",
	"data loss",
	"data-loss",
	"lost data",
	"data was lost",
	"quarantine", // pgstream parks unappliable rows in a quarantine table
	"corrupt",    // corrupted / corruption
}

// containsDataLossKeyword returns true if the given text references a
// data-safety event (loss, corruption, or quarantined writes).
func containsDataLossKeyword(text string) bool {
	lower := strings.ToLower(text)
	for _, kw := range dataLossKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// decisionContainsDataLossKeyword reports whether a decision references a
// data-safety event in any free-text field that a pre-LLM suppressor reads to
// judge eligibility — the agent-supplied agent_context.task and the outcome.
//
// The pre-LLM suppressors mine BOTH fields for their identity signal:
// extractResourceRefs and extractWorkItemRefs both scan task and outcome, so a
// pair can become eligible for suppression on the strength of a ref that appears
// only in the task. The data-safety guard must therefore scan exactly those same
// sources — otherwise a "DATALOSS recovery connector_x" task whose outcome omits
// the word is silently dropped pre-LLM while its connector ref still drives the
// disjoint-resource match. Guarding only the outcome (the original behavior) left
// that hole. This is the membership-parity fix for the guard: every suppressor
// that mines the task for identity now guards the task for data safety, so the
// guard and the eligibility signal never read different text.
//
// isOperationalStateProgression shares this guard too, though it keys on a time
// gap rather than task-extracted refs: a data-safety event anywhere in a
// decision's salient text is categorically high-stakes and must reach the
// validator, and a single uniform guard is one fewer place for the data-loss
// check to drift between the sibling filters.
func decisionContainsDataLossKeyword(d model.Decision) bool {
	return containsDataLossKeyword(nestedContextString(d.AgentContext, "task")) ||
		containsDataLossKeyword(d.Outcome)
}

// reviewTypes is defined in shared.go — it is referenced by the lite-compiled
// validator.go and so must live in an untagged file.

// implementationTypes are decision types that represent fix/implementation work.
var implementationTypes = map[string]bool{
	"architecture":   true,
	"bug_fix":        true,
	"fix":            true,
	"implementation": true,
	"refactor":       true,
}

// operationalTypes are decision types that represent execution actions against
// running infrastructure (deploys, rollbacks, scaling, restarts, image
// promotions) rather than design or evaluation decisions. These produce
// time-bound state mutations, which is what isOperationalStateProgression keys
// on. Deliberately excludes architecture/trade_off/design — those carry
// genuine direction-setting forks that must remain detectable.
var operationalTypes = map[string]bool{
	"operations":  true,
	"operational": true,
	"deployment":  true,
}

// workItemScopedTypes are decision types whose conflict scope is a single work
// item — a PR under review, a ticket being planned or investigated. Two such
// decisions about DIFFERENT work items examine different code/scope and cannot
// contradict each other, which is what isDisjointWorkItem keys on. Deliberately
// excludes the direction-setting types (architecture / trade_off / design):
// a cross-cutting design fork can legitimately span work items and must remain
// detectable, the same reason operationalTypes excludes them.
var workItemScopedTypes = map[string]bool{
	"code_review":   true,
	"review":        true,
	"assessment":    true,
	"investigation": true,
	"analysis":      true,
	"audit":         true,
	"planning":      true,
}

// resourceScopedTypes are decision types whose conflict scope is a single
// infrastructure resource — a connector being recovered, an org/customer
// incident being investigated or deployed against. Two such decisions about
// DIFFERENT resources act on different data planes and cannot contradict, which
// is what isDisjointResource keys on (via extractResourceRefs). investigation
// is deliberately a member of BOTH this set and workItemScopedTypes: an
// investigation can be disjoint by ticket OR by connector, and either filter may
// suppress it. Direction-setting types (architecture / trade_off / design) are
// excluded for the same reason as the sibling type sets — a cross-cutting design
// fork can legitimately span resources and must remain detectable.
var resourceScopedTypes = map[string]bool{
	"investigation":        true,
	"deployment":           true,
	"operational_recovery": true,
	"operations":           true,
	"operational":          true,
	"incident":             true,
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

func ticketContextNote(a, b []string) string {
	if len(a) == 0 || len(b) == 0 {
		return ""
	}
	refsA := strings.Join(a, ", ")
	refsB := strings.Join(b, ", ")
	if ticketRefsOverlap(a, b) {
		return fmt.Sprintf(" [Ticket context: Decision A references %s; Decision B references %s. Shared ticket references were available during validation.]", refsA, refsB)
	}
	return fmt.Sprintf(" [Ticket context: Decision A references %s; Decision B references %s. Non-overlapping ticket references were available during validation; the conflict was retained because validation found the same design question or an explicit supersession/rejection.]", refsA, refsB)
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
