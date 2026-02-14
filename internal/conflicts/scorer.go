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

// ScoreForDecision finds similar decisions, computes significance, and inserts
// scored conflicts above the threshold. Runs asynchronously; non-fatal errors are logged.
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

	inserted := 0
	for _, cand := range candidates {
		if cand.OutcomeEmbedding == nil {
			continue
		}
		topicSim := cosineSimilarity(d.Embedding.Slice(), cand.Embedding.Slice())
		outcomeSim := cosineSimilarity(d.OutcomeEmbedding.Slice(), cand.OutcomeEmbedding.Slice())
		outcomeDiv := 1.0 - outcomeSim
		if outcomeDiv < 0 {
			outcomeDiv = 0
		}
		significance := topicSim * outcomeDiv
		if significance < s.threshold {
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
			OutcomeA:          d.Outcome,
			OutcomeB:          cand.Outcome,
			TopicSimilarity:   ptr(topicSim),
			OutcomeDivergence: ptr(outcomeDiv),
			Significance:      ptr(significance),
			ScoringMethod:     "embedding",
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
