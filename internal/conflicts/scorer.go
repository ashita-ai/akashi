// Package conflicts provides semantic conflict detection and scoring.
package conflicts

import (
	"context"
	"log/slog"
	"math"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Scorer finds and scores semantic conflicts for new decisions.
type Scorer struct {
	db        *storage.DB
	logger    *slog.Logger
	threshold float64
}

// NewScorer creates a conflict scorer.
func NewScorer(db *storage.DB, logger *slog.Logger, significanceThreshold float64) *Scorer {
	if significanceThreshold <= 0 {
		significanceThreshold = 0.30
	}
	return &Scorer{db: db, logger: logger, threshold: significanceThreshold}
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

// ScoreForDecision finds similar decisions, computes significance using both
// full-outcome and claim-level comparison, and inserts the strongest conflict
// above the threshold. Runs asynchronously; non-fatal errors are logged.
func (s *Scorer) ScoreForDecision(ctx context.Context, decisionID, orgID uuid.UUID) {
	d, err := s.db.GetDecisionForScoring(ctx, decisionID, orgID)
	if err != nil {
		s.logger.Debug("conflict scorer: skip decision", "decision_id", decisionID, "error", err)
		return
	}
	if d.Embedding == nil || d.OutcomeEmbedding == nil {
		s.logger.Debug("conflict scorer: decision lacks embeddings", "decision_id", decisionID)
		return
	}

	candidates, err := s.db.FindSimilarDecisionsByEmbedding(ctx, orgID, *d.Embedding, decisionID, 50)
	if err != nil {
		s.logger.Warn("conflict scorer: find similar failed", "decision_id", decisionID, "error", err)
		return
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
			claimSig, claimDiv, claimA, claimB := s.bestClaimConflict(ctx, d.ID, cand.ID, topicSim)
			if claimSig > bestSig {
				bestSig = claimSig
				bestDiv = claimDiv
				bestMethod = "claim"
				bestOutcomeA = claimA
				bestOutcomeB = claimB
			}
		}

		if bestSig < s.threshold {
			continue
		}

		kind := model.ConflictKindCrossAgent
		if d.AgentID == cand.AgentID {
			kind = model.ConflictKindSelfContradiction
		}
		c := model.DecisionConflict{
			ConflictKind:      kind,
			DecisionAID:       decisionID,
			DecisionBID:       cand.ID,
			OrgID:             orgID,
			AgentA:            d.AgentID,
			AgentB:            cand.AgentID,
			DecisionTypeA:     d.DecisionType,
			DecisionTypeB:     cand.DecisionType,
			OutcomeA:          bestOutcomeA,
			OutcomeB:          bestOutcomeB,
			TopicSimilarity:   ptr(topicSim),
			OutcomeDivergence: ptr(bestDiv),
			Significance:      ptr(bestSig),
			ScoringMethod:     bestMethod,
		}
		if err := s.db.InsertScoredConflict(ctx, c); err != nil {
			s.logger.Warn("conflict scorer: insert failed", "decision_a", decisionID, "decision_b", cand.ID, "error", err)
			continue
		}
		inserted++
		if err := s.db.Notify(ctx, storage.ChannelConflicts, `{"source":"scorer"}`); err != nil {
			s.logger.Debug("conflict scorer: notify failed", "error", err)
		}
	}
	if inserted > 0 {
		s.logger.Info("conflict scorer: scored conflicts", "decision_id", decisionID, "inserted", inserted)
	}
}

// bestClaimConflict finds the most significant claim-level conflict between
// two decisions. Returns (significance, divergence, claimTextA, claimTextB).
// If no claim pairs qualify, returns (0, 0, "", "").
func (s *Scorer) bestClaimConflict(ctx context.Context, decisionAID, decisionBID uuid.UUID, topicSim float64) (float64, float64, string, string) {
	claimsA, err := s.db.FindClaimsByDecision(ctx, decisionAID)
	if err != nil || len(claimsA) == 0 {
		return 0, 0, "", ""
	}
	claimsB, err := s.db.FindClaimsByDecision(ctx, decisionBID)
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

// BackfillScoring runs conflict scoring for all decisions that have both
// embeddings. Unlike ScoreForDecision (which runs for a single new decision),
// this iterates all existing decisions so previously backfilled embeddings
// produce scored_conflicts rows. Safe to call multiple times — InsertScoredConflict
// uses ON CONFLICT DO UPDATE, so re-scoring a pair refreshes the values.
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

	var processed int
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			return processed, ctx.Err()
		default:
		}
		s.ScoreForDecision(ctx, ref.ID, ref.OrgID)
		processed++
	}
	return processed, nil
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

func ptr[T any](v T) *T { return &v }
