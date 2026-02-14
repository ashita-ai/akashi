package decisions

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/service/embedding"
)

func TestIsZeroVector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		vec      pgvector.Vector
		expected bool
	}{
		{
			name:     "all zeros",
			vec:      pgvector.NewVector([]float32{0, 0, 0, 0}),
			expected: true,
		},
		{
			name:     "first element nonzero",
			vec:      pgvector.NewVector([]float32{0.1, 0, 0, 0}),
			expected: false,
		},
		{
			name:     "last element nonzero",
			vec:      pgvector.NewVector([]float32{0, 0, 0, 0.01}),
			expected: false,
		},
		{
			name:     "all nonzero",
			vec:      pgvector.NewVector([]float32{0.5, 0.3, 0.2, 0.1}),
			expected: false,
		},
		{
			name:     "empty slice",
			vec:      pgvector.NewVector([]float32{}),
			expected: true,
		},
		{
			name:     "single zero",
			vec:      pgvector.NewVector([]float32{0}),
			expected: true,
		},
		{
			name:     "single nonzero",
			vec:      pgvector.NewVector([]float32{1.0}),
			expected: false,
		},
		{
			name:     "negative value",
			vec:      pgvector.NewVector([]float32{0, -0.5, 0}),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isZeroVector(tt.vec)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestValidateEmbeddingDims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dims      int
		vecLen    int
		expectErr bool
	}{
		{
			name:      "matching dimensions",
			dims:      1024,
			vecLen:    1024,
			expectErr: false,
		},
		{
			name:      "vector too short",
			dims:      1024,
			vecLen:    512,
			expectErr: true,
		},
		{
			name:      "vector too long",
			dims:      1024,
			vecLen:    2048,
			expectErr: true,
		},
		{
			name:      "zero-length vector with nonzero expected dims",
			dims:      1024,
			vecLen:    0,
			expectErr: true,
		},
		{
			name:      "single dimension match",
			dims:      1,
			vecLen:    1,
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := embedding.NewNoopProvider(tt.dims)
			svc := &Service{embedder: provider}

			vec := pgvector.NewVector(make([]float32, tt.vecLen))
			err := svc.validateEmbeddingDims(vec)

			if tt.expectErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "embedding dimension mismatch")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestIsDuplicateKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name: "duplicate key violation 23505",
			err: &pgconn.PgError{
				Code:    "23505",
				Message: "duplicate key value violates unique constraint",
			},
			expected: true,
		},
		{
			name: "foreign key violation 23503",
			err: &pgconn.PgError{
				Code:    "23503",
				Message: "insert or update on table violates foreign key constraint",
			},
			expected: false,
		},
		{
			name: "check constraint violation 23514",
			err: &pgconn.PgError{
				Code:    "23514",
				Message: "new row violates check constraint",
			},
			expected: false,
		},
		{
			name:     "generic non-pg error",
			err:      assert.AnError,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isDuplicateKey(tt.err)
			assert.Equal(t, tt.expected, got)
		})
	}
}
