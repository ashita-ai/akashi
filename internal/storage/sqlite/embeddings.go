package sqlite

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/ashita-ai/akashi/internal/storage"
)

// GetDecisionEmbeddings returns embedding pairs for decisions that have both embeddings.
func (l *LiteDB) GetDecisionEmbeddings(ctx context.Context, ids []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID][2]pgvector.Vector, error) {
	if len(ids) == 0 {
		return map[uuid.UUID][2]pgvector.Vector{}, nil
	}
	idsJSON := uuidSliceToJSON(ids)
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, embedding, outcome_embedding FROM decisions
		 WHERE id IN (SELECT value FROM json_each(?)) AND org_id = ? AND valid_to IS NULL
		   AND embedding IS NOT NULL AND outcome_embedding IS NOT NULL`,
		idsJSON, uuidStr(orgID),
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: get decision embeddings: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	result := make(map[uuid.UUID][2]pgvector.Vector, len(ids))
	for rows.Next() {
		var (
			idStr   string
			embBlob []byte
			outBlob []byte
		)
		if err := rows.Scan(&idStr, &embBlob, &outBlob); err != nil {
			return nil, fmt.Errorf("sqlite: scan embedding: %w", err)
		}
		emb := blobToVector(embBlob)
		out := blobToVector(outBlob)
		if emb != nil && out != nil {
			result[parseUUID(idStr)] = [2]pgvector.Vector{*emb, *out}
		}
	}
	return result, rows.Err()
}

// FindUnembeddedDecisions returns decisions without embeddings.
func (l *LiteDB) FindUnembeddedDecisions(ctx context.Context, limit int) ([]storage.UnembeddedDecision, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, org_id, decision_type, outcome, reasoning
		 FROM decisions WHERE embedding IS NULL AND valid_to IS NULL
		 ORDER BY valid_from ASC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: find unembedded: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var result []storage.UnembeddedDecision
	for rows.Next() {
		var (
			d      storage.UnembeddedDecision
			idStr  string
			orgStr string
		)
		if err := rows.Scan(&idStr, &orgStr, &d.DecisionType, &d.Outcome, &d.Reasoning); err != nil {
			return nil, fmt.Errorf("sqlite: scan unembedded: %w", err)
		}
		d.ID = parseUUID(idStr)
		d.OrgID = parseUUID(orgStr)
		result = append(result, d)
	}
	return result, rows.Err()
}

// BackfillEmbedding updates a decision's embedding.
func (l *LiteDB) BackfillEmbedding(ctx context.Context, id, orgID uuid.UUID, emb pgvector.Vector) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE decisions SET embedding = ? WHERE id = ? AND org_id = ? AND valid_to IS NULL`,
		vectorToBlob(&emb), uuidStr(id), uuidStr(orgID),
	)
	if err != nil {
		return fmt.Errorf("sqlite: backfill embedding: %w", err)
	}
	return nil
}

// FindDecisionsMissingOutcomeEmbedding returns decisions with embedding but no outcome_embedding.
func (l *LiteDB) FindDecisionsMissingOutcomeEmbedding(ctx context.Context, limit int) ([]storage.UnembeddedDecision, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, org_id, decision_type, outcome, reasoning
		 FROM decisions
		 WHERE embedding IS NOT NULL AND outcome_embedding IS NULL AND valid_to IS NULL
		 ORDER BY valid_from ASC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: find missing outcome embedding: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var result []storage.UnembeddedDecision
	for rows.Next() {
		var (
			d      storage.UnembeddedDecision
			idStr  string
			orgStr string
		)
		if err := rows.Scan(&idStr, &orgStr, &d.DecisionType, &d.Outcome, &d.Reasoning); err != nil {
			return nil, fmt.Errorf("sqlite: scan missing outcome: %w", err)
		}
		d.ID = parseUUID(idStr)
		d.OrgID = parseUUID(orgStr)
		result = append(result, d)
	}
	return result, rows.Err()
}

// BackfillOutcomeEmbedding updates a decision's outcome_embedding.
func (l *LiteDB) BackfillOutcomeEmbedding(ctx context.Context, id, orgID uuid.UUID, emb pgvector.Vector) error {
	_, err := l.db.ExecContext(ctx,
		`UPDATE decisions SET outcome_embedding = ? WHERE id = ? AND org_id = ? AND valid_to IS NULL`,
		vectorToBlob(&emb), uuidStr(id), uuidStr(orgID),
	)
	if err != nil {
		return fmt.Errorf("sqlite: backfill outcome embedding: %w", err)
	}
	return nil
}
