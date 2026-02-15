package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/akashi/internal/model"
)

// CreateEvidence inserts a single piece of evidence for a decision.
func (db *DB) CreateEvidence(ctx context.Context, ev model.Evidence) (model.Evidence, error) {
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	if ev.Metadata == nil {
		ev.Metadata = map[string]any{}
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO evidence (id, decision_id, org_id, source_type, source_uri, content,
		 relevance_score, embedding, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		ev.ID, ev.DecisionID, ev.OrgID, string(ev.SourceType), ev.SourceURI, ev.Content,
		ev.RelevanceScore, ev.Embedding, ev.Metadata, ev.CreatedAt,
	)
	if err != nil {
		return model.Evidence{}, fmt.Errorf("storage: create evidence: %w", err)
	}
	return ev, nil
}

// CreateEvidenceBatch inserts multiple evidence entries.
func (db *DB) CreateEvidenceBatch(ctx context.Context, evs []model.Evidence) error {
	if len(evs) == 0 {
		return nil
	}

	columns := []string{"id", "decision_id", "org_id", "source_type", "source_uri", "content",
		"relevance_score", "embedding", "metadata", "created_at"}

	rows := make([][]any, len(evs))
	for i, ev := range evs {
		id := ev.ID
		if id == uuid.Nil {
			id = uuid.New()
		}
		createdAt := ev.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		meta := ev.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		rows[i] = []any{id, ev.DecisionID, ev.OrgID, string(ev.SourceType), ev.SourceURI, ev.Content,
			ev.RelevanceScore, ev.Embedding, meta, createdAt}
	}

	_, err := db.pool.CopyFrom(ctx, pgx.Identifier{"evidence"}, columns, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("storage: copy evidence: %w", err)
	}
	return nil
}

// GetEvidenceByDecisions retrieves all evidence for a set of decision IDs in a single query.
// Results are returned as a map from decision ID to its evidence.
// orgID provides defense-in-depth tenant isolation; even though callers gate access
// upstream, the storage layer enforces org boundaries on every query.
func (db *DB) GetEvidenceByDecisions(ctx context.Context, decisionIDs []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID][]model.Evidence, error) {
	if len(decisionIDs) == 0 {
		return nil, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, decision_id, org_id, source_type, source_uri, content,
		 relevance_score, metadata, created_at
		 FROM evidence WHERE decision_id = ANY($1) AND org_id = $2
		 ORDER BY relevance_score DESC NULLS LAST`, decisionIDs, orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get evidence batch: %w", err)
	}
	defer rows.Close()

	result := make(map[uuid.UUID][]model.Evidence)
	for rows.Next() {
		var ev model.Evidence
		if err := rows.Scan(
			&ev.ID, &ev.DecisionID, &ev.OrgID, &ev.SourceType, &ev.SourceURI, &ev.Content,
			&ev.RelevanceScore, &ev.Metadata, &ev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan evidence: %w", err)
		}
		result[ev.DecisionID] = append(result[ev.DecisionID], ev)
	}
	return result, rows.Err()
}

// GetEvidenceByDecision retrieves all evidence for a decision.
// orgID provides defense-in-depth tenant isolation.
func (db *DB) GetEvidenceByDecision(ctx context.Context, decisionID uuid.UUID, orgID uuid.UUID) ([]model.Evidence, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, decision_id, org_id, source_type, source_uri, content,
		 relevance_score, metadata, created_at
		 FROM evidence WHERE decision_id = $1 AND org_id = $2
		 ORDER BY relevance_score DESC NULLS LAST`, decisionID, orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: get evidence: %w", err)
	}
	defer rows.Close()

	var evs []model.Evidence
	for rows.Next() {
		var ev model.Evidence
		if err := rows.Scan(
			&ev.ID, &ev.DecisionID, &ev.OrgID, &ev.SourceType, &ev.SourceURI, &ev.Content,
			&ev.RelevanceScore, &ev.Metadata, &ev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan evidence: %w", err)
		}
		evs = append(evs, ev)
	}
	return evs, rows.Err()
}

// EvidenceCoverageStats holds evidence coverage metrics for an org.
type EvidenceCoverageStats struct {
	TotalDecisions       int
	WithEvidence         int
	WithoutEvidenceCount int
	CoveragePercent      float64
}

// GetEvidenceCoverageStats returns how many current decisions have at least one evidence record.
func (db *DB) GetEvidenceCoverageStats(ctx context.Context, orgID uuid.UUID) (EvidenceCoverageStats, error) {
	var s EvidenceCoverageStats
	err := db.pool.QueryRow(ctx, `
		SELECT count(DISTINCT d.id) AS total,
		       count(DISTINCT e.decision_id) AS with_evidence
		FROM decisions d
		LEFT JOIN evidence e ON d.id = e.decision_id AND e.org_id = d.org_id
		WHERE d.org_id = $1 AND d.valid_to IS NULL`, orgID).Scan(
		&s.TotalDecisions, &s.WithEvidence)
	if err != nil {
		return s, fmt.Errorf("storage: evidence coverage stats: %w", err)
	}
	s.WithoutEvidenceCount = s.TotalDecisions - s.WithEvidence
	if s.TotalDecisions > 0 {
		s.CoveragePercent = float64(s.WithEvidence) / float64(s.TotalDecisions) * 100
	}
	return s, nil
}

// SearchEvidenceByEmbedding performs semantic similarity search over evidence within an org.
func (db *DB) SearchEvidenceByEmbedding(ctx context.Context, orgID uuid.UUID, embedding pgvector.Vector, limit int) ([]model.Evidence, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.pool.Query(ctx,
		`SELECT id, decision_id, org_id, source_type, source_uri, content,
		 relevance_score, metadata, created_at
		 FROM evidence
		 WHERE org_id = $1 AND embedding IS NOT NULL
		 ORDER BY embedding <=> $2
		 LIMIT $3`, orgID, embedding, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: search evidence: %w", err)
	}
	defer rows.Close()

	var evs []model.Evidence
	for rows.Next() {
		var ev model.Evidence
		if err := rows.Scan(
			&ev.ID, &ev.DecisionID, &ev.OrgID, &ev.SourceType, &ev.SourceURI, &ev.Content,
			&ev.RelevanceScore, &ev.Metadata, &ev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("storage: scan evidence search: %w", err)
		}
		evs = append(evs, ev)
	}
	return evs, rows.Err()
}
