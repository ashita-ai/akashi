// Package search — LocalSearcher provides brute-force cosine similarity search
// for the lite-mode SQLite backend.
//
// At local scale (<10k decisions), loading all org-scoped embeddings into memory
// and computing cosine similarity in Go is fast enough (single-digit ms). This
// avoids any dependency on Qdrant, sqlite-vec, or CGo.
//
// See ADR-009 for the architectural rationale.
package search

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
)

// LocalSearcher performs brute-force cosine similarity search over decision
// embeddings stored in a SQL database (SQLite or Postgres). It implements both
// the Searcher and CandidateFinder interfaces.
type LocalSearcher struct {
	db *sql.DB
}

// Compile-time interface checks.
var (
	_ Searcher        = (*LocalSearcher)(nil)
	_ CandidateFinder = (*LocalSearcher)(nil)
)

// NewLocalSearcher creates a LocalSearcher backed by the given sql.DB.
func NewLocalSearcher(db *sql.DB) *LocalSearcher {
	return &LocalSearcher{db: db}
}

// Healthy returns nil — the local searcher has no external dependencies.
func (s *LocalSearcher) Healthy(_ context.Context) error {
	return nil
}

// Search loads all embeddings for the given org, computes cosine similarity
// against the query vector, applies filters, and returns the top-K results.
func (s *LocalSearcher) Search(ctx context.Context, orgID uuid.UUID, embedding []float32, filters model.QueryFilters, limit int) ([]Result, error) {
	if len(embedding) == 0 {
		return nil, nil
	}
	return s.search(ctx, orgID, embedding, uuid.Nil, filters, nil, limit)
}

// FindSimilar implements CandidateFinder for conflict detection.
func (s *LocalSearcher) FindSimilar(ctx context.Context, orgID uuid.UUID, embedding []float32, excludeID uuid.UUID, projects []string, limit int) ([]Result, error) {
	if len(embedding) == 0 {
		return nil, nil
	}
	// Ensure non-nil so loadCandidates applies project scoping.
	// nil projects from caller means "no project set" → match only NULL-project decisions.
	if projects == nil {
		projects = []string{}
	}
	return s.search(ctx, orgID, embedding, excludeID, model.QueryFilters{}, projects, limit)
}

// search is the shared implementation for Search and FindSimilar.
func (s *LocalSearcher) search(ctx context.Context, orgID uuid.UUID, queryVec []float32, excludeID uuid.UUID, filters model.QueryFilters, projects []string, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 10
	}

	// Load candidate decision IDs + embeddings.
	candidates, err := s.loadCandidates(ctx, orgID, excludeID, filters, projects)
	if err != nil {
		return nil, fmt.Errorf("local search: load candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Score each candidate by cosine similarity.
	results := make([]Result, 0, len(candidates))
	for _, c := range candidates {
		if len(c.embedding) != len(queryVec) {
			continue // dimension mismatch — skip
		}
		score := cosineSimilarity(queryVec, c.embedding)
		if score > 0 {
			results = append(results, Result{
				DecisionID: c.id,
				Score:      score,
			})
		}
	}

	// Sort descending by score.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

type candidate struct {
	id        uuid.UUID
	embedding []float32
}

// loadCandidates queries the database for decision embeddings matching the filters.
func (s *LocalSearcher) loadCandidates(ctx context.Context, orgID uuid.UUID, excludeID uuid.UUID, filters model.QueryFilters, projects []string) ([]candidate, error) {
	// Build the query dynamically based on filters.
	q := `SELECT id, embedding FROM decisions WHERE org_id = ? AND valid_to IS NULL AND embedding IS NOT NULL`
	args := []any{orgID.String()}

	if excludeID != uuid.Nil {
		q += ` AND id != ?`
		args = append(args, excludeID.String())
	}
	if len(filters.AgentIDs) > 0 {
		q += ` AND agent_id IN (`
		for i, aid := range filters.AgentIDs {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, aid)
		}
		q += ")"
	}
	if filters.DecisionType != nil {
		q += ` AND decision_type = ?`
		args = append(args, *filters.DecisionType)
	}
	if filters.ConfidenceMin != nil {
		q += ` AND confidence >= ?`
		args = append(args, *filters.ConfidenceMin)
	}
	if filters.SessionID != nil {
		q += ` AND session_id = ?`
		args = append(args, filters.SessionID.String())
	}
	if filters.Tool != nil {
		q += ` AND tool = ?`
		args = append(args, *filters.Tool)
	}
	if filters.Model != nil {
		q += ` AND model = ?`
		args = append(args, *filters.Model)
	}
	if filters.Project != nil {
		q += ` AND project = ?`
		args = append(args, *filters.Project)
	}
	// CandidateFinder project filter (separate from QueryFilters.Project).
	// nil = no CandidateFinder scoping (used by Search).
	// empty non-nil = match only NULL-project decisions.
	// non-empty = match decisions in the listed projects.
	if projects != nil {
		switch len(projects) {
		case 0:
			q += ` AND project IS NULL`
		case 1:
			q += ` AND project = ?`
			args = append(args, projects[0])
		default:
			q += ` AND project IN (`
			for i, p := range projects {
				if i > 0 {
					q += ","
				}
				q += "?"
				args = append(args, p)
			}
			q += ")"
		}
	}
	if filters.TimeRange != nil {
		if filters.TimeRange.From != nil {
			q += ` AND valid_from >= ?`
			args = append(args, filters.TimeRange.From.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
		}
		if filters.TimeRange.To != nil {
			q += ` AND valid_from <= ?`
			args = append(args, filters.TimeRange.To.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
		}
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []candidate
	for rows.Next() {
		var (
			idStr string
			blob  []byte
		)
		if err := rows.Scan(&idStr, &blob); err != nil {
			return nil, err
		}
		id, _ := uuid.Parse(idStr)
		emb := blobToFloat32(blob)
		if len(emb) > 0 {
			out = append(out, candidate{id: id, embedding: emb})
		}
	}
	return out, rows.Err()
}

// blobToFloat32 decodes a little-endian float32 BLOB into a []float32 slice.
// This mirrors the encoding used by sqlite/helpers.go vectorToBlob.
func blobToFloat32(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	n := len(b) / 4
	out := make([]float32, n)
	for i := range n {
		bits := uint32(b[i*4]) | uint32(b[i*4+1])<<8 | uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
		out[i] = math.Float32frombits(bits)
	}
	return out
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
// Returns 0 if either vector has zero magnitude.
func cosineSimilarity(a, b []float32) float32 {
	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}
