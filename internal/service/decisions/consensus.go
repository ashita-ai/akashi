package decisions

import (
	"context"
	"math"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/ashita-ai/akashi/internal/search"
)

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

	// Per-decision Qdrant queries, parallelized with bounded concurrency.
	// Collect all neighbor IDs for a single batch Postgres fetch, avoiding
	// N+1 embedding lookups.
	neighborResultsByID := make(map[uuid.UUID][]search.Result, len(embMap))
	allNeighborIDs := make(map[uuid.UUID]struct{})
	var mu sync.Mutex

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(10) // bound concurrent Qdrant RPCs

	for id, embs := range embMap {
		g.Go(func() error {
			results, qErr := cf.FindSimilar(gCtx, orgID, embs[0].Slice(), id, nil, 50)
			if qErr != nil {
				s.logger.Warn("consensus batch: qdrant find similar failed", "decision_id", id, "error", qErr)
				return nil // non-fatal: skip this decision
			}
			mu.Lock()
			neighborResultsByID[id] = results
			for _, r := range results {
				allNeighborIDs[r.DecisionID] = struct{}{}
			}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return result, nil //nolint:nilerr // Qdrant errors are non-fatal for consensus
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
