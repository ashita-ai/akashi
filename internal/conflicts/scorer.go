// Package conflicts provides semantic conflict detection and scoring.
package conflicts

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
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
	backfillWorkers int
	decayLambda     float64 // Temporal decay rate. 0 disables decay.
	finder          search.CandidateFinder
	// pairwiseScorer is an optional external override for the confirmation step.
	// When non-nil, it replaces the built-in Validator-backed scoring for each candidate pair.
	pairwiseScorer PairwiseScorer
}

// WithCandidateFinder wires a Qdrant-backed CandidateFinder for conflict candidate
// discovery. Without one, ScoreForDecision skips candidate retrieval and inserts
// no conflicts. Must be called before any scoring starts.
func (s *Scorer) WithCandidateFinder(cf search.CandidateFinder) *Scorer {
	s.finder = cf
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
	return &Scorer{
		db:              db,
		logger:          logger,
		threshold:       significanceThreshold,
		validator:       validator,
		backfillWorkers: backfillWorkers,
		decayLambda:     decayLambda,
	}
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

	qdrantResults, err := s.finder.FindSimilar(ctx, orgID, d.Embedding.Slice(), decisionID, d.Project, 50)
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

	inserted := 0
	for _, cand := range candidates {
		if cand.OutcomeEmbedding == nil {
			continue
		}

		// Skip decisions in the same revision chain — intentional
		// replacements should not be flagged as conflicts.
		if revisionChain[cand.ID] {
			continue
		}

		topicSim := cosineSimilarity(d.Embedding.Slice(), cand.Embedding.Slice())

		// --- Pass 1: full-outcome scoring (existing behavior) ---
		outcomeSim := cosineSimilarity(d.OutcomeEmbedding.Slice(), cand.OutcomeEmbedding.Slice())
		outcomeDiv := math.Max(0, 1.0-outcomeSim)
		outcomeSig := topicSim * outcomeDiv

		// Track the best signal across both passes.
		bestSig := outcomeSig
		bestDiv := outcomeDiv
		bestMethod := "embedding"
		bestOutcomeA := d.Outcome
		bestOutcomeB := cand.Outcome

		// --- Pass 2: claim-level scoring for high topic-similarity pairs ---
		if topicSim >= decisionTopicSimFloor {
			claimSig, claimDiv, claimA, claimB := s.bestClaimConflict(ctx, d.ID, cand.ID, orgID, topicSim)
			if claimSig > bestSig {
				bestSig = claimSig
				bestDiv = claimDiv
				bestMethod = "claim"
				bestOutcomeA = claimA
				bestOutcomeB = claimB
			}
		}

		// Confidence weighting: low-confidence decisions get lower significance.
		confWeight := math.Sqrt(float64(d.Confidence) * float64(cand.Confidence))
		bestSig *= confWeight

		// Temporal decay: older decision pairs get lower significance.
		var decay = 1.0
		if s.decayLambda > 0 {
			daysBetween := math.Abs(d.ValidFrom.Sub(cand.ValidFrom).Hours() / 24)
			decay = math.Exp(-s.decayLambda * daysBetween)
			bestSig *= decay
		}

		// High-topic-similarity pairs bypass the cosine-divergence significance gate
		// when an LLM validator or external pairwise scorer is active. This applies
		// to both cross-agent and same-agent pairs: bi-encoders cannot detect stance
		// opposition for same-topic decisions ("X is correct" vs "X is wrong" embed
		// close together because they share domain vocabulary). The LLM is the right
		// classifier for this, regardless of whether the two decisions came from the
		// same agent or different agents. See: NLI literature on bi-encoder limits.
		hasScorer := s.pairwiseScorer != nil || !isNoop
		directToScorer := hasScorer && topicSim >= decisionTopicSimFloor

		if bestSig < s.threshold && !directToScorer {
			continue
		}

		// Confirmation gate: classify the candidate pair as conflict or not.
		// Priority: (1) external pairwise scorer, (2) built-in LLM validator, (3) noop.
		// NoopValidator always returns "contradiction" (preserving legacy embedding-only behavior).
		var explanation *string
		var category, severity, relationship *string

		if s.pairwiseScorer != nil {
			// External pairwise scorer (enterprise override).
			if cache != nil && cache.checkAndMark(decisionID, cand.ID) {
				s.logger.Debug("conflict scorer: pair already evaluated, skipping external scorer call",
					"decision_a", decisionID, "decision_b", cand.ID)
				continue
			}
			extScore, extExpl, err := s.pairwiseScorer.ScorePair(ctx, d, cand)
			if err != nil {
				s.logger.Warn("conflict scorer: external pairwise scorer failed, skipping candidate",
					"error", err, "decision_a", decisionID, "decision_b", cand.ID)
				continue // fail-safe: don't insert unscored conflicts
			}
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

			result, err := s.validator.Validate(ctx, ValidateInput{
				OutcomeA:        bestOutcomeA,
				OutcomeB:        bestOutcomeB,
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
				TopicSimilarity: topicSim,
			})
			if err != nil {
				s.logger.Warn("conflict scorer: LLM validation failed, skipping candidate",
					"error", err, "decision_a", decisionID, "decision_b", cand.ID)
				continue // fail-safe: don't insert unvalidated conflicts
			}
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
			TopicSimilarity:   ptr(topicSim),
			OutcomeDivergence: ptr(bestDiv),
			Significance:      ptr(bestSig),
			ScoringMethod:     bestMethod,
			Explanation:       explanation,
			Category:          category,
			Severity:          severity,
			Relationship:      relationship,
			ConfidenceWeight:  ptr(confWeight),
			TemporalDecay:     ptr(decay),
			Status:            "open",
		}
		if err := s.db.InsertScoredConflict(ctx, c); err != nil {
			s.logger.Warn("conflict scorer: insert failed", "decision_a", decisionID, "decision_b", cand.ID, "error", err)
			continue
		}
		inserted++
		if err := s.db.Notify(ctx, storage.ChannelConflicts, `{"source":"scorer","org_id":"`+orgID.String()+`"}`); err != nil {
			s.logger.Debug("conflict scorer: notify failed", "error", err)
		}
	}
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
		if ca.Embedding == nil {
			continue
		}
		for _, cb := range claimsB {
			if cb.Embedding == nil {
				continue
			}
			claimSim := cosineSimilarity(ca.Embedding.Slice(), cb.Embedding.Slice())
			if claimSim < claimTopicSimFloor {
				continue // Claims are about different things.
			}
			claimDiv := 1.0 - claimSim
			if claimDiv < claimDivFloor {
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
func (s *Scorer) ClearUnvalidatedConflicts(ctx context.Context) (int, error) {
	tag, err := s.db.Pool().Exec(ctx, `DELETE FROM scored_conflicts WHERE scoring_method NOT IN ('llm_v2')`)
	if err != nil {
		return 0, fmt.Errorf("conflicts: clear unvalidated: %w", err)
	}
	return int(tag.RowsAffected()), nil
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

// ClearAllConflicts deletes all scored_conflicts regardless of scoring method.
// Used after prompt improvements to force re-evaluation of all decision pairs.
// Returns the number of rows deleted.
// SECURITY: Intentionally global — one-time startup operation that clears
// conflicts across all orgs when the conflict prompt is improved.
func (s *Scorer) ClearAllConflicts(ctx context.Context) (int, error) {
	tag, err := s.db.Pool().Exec(ctx, `DELETE FROM scored_conflicts`)
	if err != nil {
		return 0, fmt.Errorf("conflicts: clear all: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptr[T any](v T) *T { return &v }
