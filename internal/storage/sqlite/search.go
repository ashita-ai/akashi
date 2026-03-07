package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Check searches for decision precedents using the best available strategy.
//
// Dual-mode with graceful fallback:
//  1. If embedding is non-empty, search by cosine similarity (brute-force).
//  2. If cosine returns nothing or fails, fall back to FTS5 text search.
//  3. If FTS5 fails (bad query syntax), fall back to LIKE search.
//  4. If query is empty and no embedding, return most recent decisions.
func (s *Store) Check(ctx context.Context, query string, embedding []float32, limit int) ([]CheckResult, error) {
	if limit <= 0 {
		limit = 5
	}

	// Mode 1: Semantic search when a query embedding is available.
	if len(embedding) > 0 && !isZeroVector(embedding) {
		results, err := s.searchByCosine(ctx, embedding, limit)
		if err != nil {
			s.logger.Warn("sqlite: cosine search failed, falling back to FTS5", "error", err)
		} else if len(results) > 0 {
			return results, nil
		}
		// Fall through to FTS5 if cosine returned nothing
		// (e.g., no stored embeddings yet).
	}

	// Mode 2: FTS5 text search (always available, zero config).
	if query != "" {
		return s.searchByFTS(ctx, query, limit)
	}

	// No query and no embedding: return most recent decisions.
	return s.recentDecisions(ctx, limit)
}

// searchByCosine performs brute-force cosine similarity search over all stored
// embeddings. This is O(N) where N is the number of decisions with embeddings.
// For local-lite scale (<10k decisions), this completes in single-digit milliseconds.
func (s *Store) searchByCosine(ctx context.Context, queryEmb []float32, limit int) ([]CheckResult, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, decision_type, outcome, reasoning, confidence, agent_id, created_at, embedding
		 FROM decisions
		 WHERE embedding IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: cosine scan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type scored struct {
		result CheckResult
		sim    float64
	}
	var candidates []scored

	for rows.Next() {
		r, embBlob, err := scanDecisionWithEmbedding(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: cosine scan row: %w", err)
		}

		storedEmb, err := unmarshalFloat32s(embBlob)
		if err != nil {
			s.logger.Warn("sqlite: corrupt embedding blob, skipping",
				"decision_id", r.DecisionID, "error", err)
			continue
		}

		sim := cosineSimilarity(queryEmb, storedEmb)
		if sim <= 0 {
			continue // Skip irrelevant or anti-correlated results.
		}

		r.Score = sim
		candidates = append(candidates, scored{result: r, sim: sim})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: cosine scan rows: %w", err)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].sim > candidates[j].sim
	})

	results := make([]CheckResult, 0, min(limit, len(candidates)))
	for i := range candidates {
		if i >= limit {
			break
		}
		results = append(results, candidates[i].result)
	}
	return results, nil
}

// searchByFTS uses SQLite FTS5 with BM25 ranking and porter stemming.
// Falls back to LIKE search if FTS5 MATCH fails (e.g., bad query syntax).
func (s *Store) searchByFTS(ctx context.Context, query string, limit int) ([]CheckResult, error) {
	// FTS5's rank column returns negative BM25 scores (lower = better match).
	// We negate it so higher Score values mean better matches.
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.decision_type, d.outcome, d.reasoning, d.confidence, d.agent_id, d.created_at,
				-f.rank AS score
		 FROM decisions_fts f
		 JOIN decisions d ON d.id = f.decision_id
		 WHERE decisions_fts MATCH ?
		 ORDER BY f.rank
		 LIMIT ?`,
		query, limit,
	)
	if err != nil {
		// FTS5 MATCH can fail on malformed queries (unbalanced quotes, etc.).
		s.logger.Warn("sqlite: FTS5 MATCH failed, falling back to LIKE",
			"query", query, "error", err)
		return s.searchByLike(ctx, query, limit)
	}
	defer func() { _ = rows.Close() }()

	return scanCheckResultsWithScore(rows)
}

// searchByLike is the terminal fallback: simple pattern matching on decision fields.
// The WHERE clause is built from user-supplied search terms, each bound via
// parameterized LIKE ? patterns. The only dynamic part of the SQL is the number
// of OR-joined clauses, which is capped at 20 terms and uses a fixed template
// string with no user data interpolation. This is safe from SQL injection.
func (s *Store) searchByLike(ctx context.Context, query string, limit int) ([]CheckResult, error) {
	words := strings.Fields(query)
	if len(words) == 0 {
		return nil, nil
	}
	// Cap the number of search terms to prevent query explosion.
	if len(words) > 20 {
		words = words[:20]
	}

	replacer := strings.NewReplacer("%", `\%`, "_", `\_`)
	var clauses []string
	var args []any
	for _, w := range words {
		escaped := replacer.Replace(w)
		pattern := "%" + escaped + "%"
		clauses = append(clauses,
			"(d.outcome LIKE ? ESCAPE '\\' OR d.decision_type LIKE ? ESCAPE '\\' OR d.reasoning LIKE ? ESCAPE '\\')")
		args = append(args, pattern, pattern, pattern)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, buildLikeQuery(clauses), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: like search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanCheckResultsWithScore(rows)
}

// recentDecisions returns the most recent decisions by created_at.
func (s *Store) recentDecisions(ctx context.Context, limit int) ([]CheckResult, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, decision_type, outcome, reasoning, confidence, agent_id, created_at
		 FROM decisions
		 ORDER BY created_at DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: recent decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanCheckResults(rows)
}

// scanCheckResults scans rows into CheckResult with Score=0 (no ranking signal).
func scanCheckResults(rows *sql.Rows) ([]CheckResult, error) {
	var results []CheckResult
	for rows.Next() {
		var (
			idStr, dt, outcome, agentID, createdAtStr string
			reasoning                                 *string
			confidence                                float32
		)
		if err := rows.Scan(&idStr, &dt, &outcome, &reasoning, &confidence, &agentID, &createdAtStr); err != nil {
			return nil, fmt.Errorf("sqlite: scan decision: %w", err)
		}
		did, _ := uuid.Parse(idStr)
		createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
		results = append(results, CheckResult{
			DecisionID:   did,
			DecisionType: dt,
			Outcome:      outcome,
			Reasoning:    reasoning,
			Confidence:   confidence,
			AgentID:      agentID,
			CreatedAt:    createdAt,
		})
	}
	return results, rows.Err()
}

// scanCheckResultsWithScore scans rows that include a trailing score column.
func scanCheckResultsWithScore(rows *sql.Rows) ([]CheckResult, error) {
	var results []CheckResult
	for rows.Next() {
		var (
			idStr, dt, outcome, agentID, createdAtStr string
			reasoning                                 *string
			confidence                                float32
			score                                     float64
		)
		if err := rows.Scan(&idStr, &dt, &outcome, &reasoning, &confidence, &agentID, &createdAtStr, &score); err != nil {
			return nil, fmt.Errorf("sqlite: scan decision with score: %w", err)
		}
		did, _ := uuid.Parse(idStr)
		createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
		results = append(results, CheckResult{
			DecisionID:   did,
			DecisionType: dt,
			Outcome:      outcome,
			Reasoning:    reasoning,
			Confidence:   confidence,
			AgentID:      agentID,
			CreatedAt:    createdAt,
			Score:        score,
		})
	}
	return results, rows.Err()
}

// scanDecisionWithEmbedding scans a row that includes a trailing embedding BLOB.
func scanDecisionWithEmbedding(rows *sql.Rows) (CheckResult, []byte, error) {
	var (
		idStr, dt, outcome, agentID, createdAtStr string
		reasoning                                 *string
		confidence                                float32
		embBlob                                   []byte
	)
	if err := rows.Scan(&idStr, &dt, &outcome, &reasoning, &confidence, &agentID, &createdAtStr, &embBlob); err != nil {
		return CheckResult{}, nil, err
	}
	did, _ := uuid.Parse(idStr)
	createdAt, _ := time.Parse(time.RFC3339Nano, createdAtStr)
	return CheckResult{
		DecisionID:   did,
		DecisionType: dt,
		Outcome:      outcome,
		Reasoning:    reasoning,
		Confidence:   confidence,
		AgentID:      agentID,
		CreatedAt:    createdAt,
	}, embBlob, nil
}

// buildLikeQuery constructs the LIKE fallback SQL from fixed template clauses.
// Each clause is a constant string like "(d.outcome LIKE ? ESCAPE ...)"; no user
// data is interpolated into the SQL. All user-supplied search terms are bound via
// parameterized ? placeholders in the args slice.
func buildLikeQuery(clauses []string) string {
	return `SELECT d.id, d.decision_type, d.outcome, d.reasoning, d.confidence, d.agent_id, d.created_at,
				1.0 AS score
		 FROM decisions d
		 WHERE ` + strings.Join(clauses, " OR ") + ` ORDER BY d.created_at DESC LIMIT ?` //nolint:gosec // clauses are fixed template strings
}

// isZeroVector returns true if all elements are zero.
func isZeroVector(v []float32) bool {
	for _, f := range v {
		if f != 0 {
			return false
		}
	}
	return true
}
