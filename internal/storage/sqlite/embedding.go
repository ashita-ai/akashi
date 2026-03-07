package sqlite

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// StoreEmbedding persists an embedding alongside a decision. The embedding is
// serialized as a little-endian []float32 BLOB. This is called asynchronously
// after trace — the same pattern as the full server's Qdrant outbox worker.
//
// If the decision does not exist, the UPDATE affects zero rows and no error is
// returned. This is intentional: a race between delete and async embedding is
// harmless — the embedding simply has nowhere to go.
func (s *Store) StoreEmbedding(ctx context.Context, decisionID uuid.UUID, embedding []float32) error {
	blob := marshalFloat32s(embedding)
	_, err := s.db.ExecContext(ctx,
		`UPDATE decisions SET embedding = ? WHERE id = ?`,
		blob, decisionID.String(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: store embedding for %s: %w", decisionID, err)
	}
	return nil
}

// GetEmbedding retrieves the stored embedding for a decision. Returns nil, nil
// if the decision has no embedding or does not exist.
func (s *Store) GetEmbedding(ctx context.Context, decisionID uuid.UUID) ([]float32, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT embedding FROM decisions WHERE id = ? AND embedding IS NOT NULL`,
		decisionID.String(),
	).Scan(&blob)
	if err != nil {
		return nil, nil //nolint:nilerr // No row or NULL embedding is not an error.
	}
	return unmarshalFloat32s(blob)
}
