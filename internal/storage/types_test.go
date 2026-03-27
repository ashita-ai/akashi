//go:build !lite

package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClampPagination(t *testing.T) {
	t.Run("uses defaults for zero limit", func(t *testing.T) {
		limit, offset := clampPagination(0, 0, 50, 1000)
		assert.Equal(t, 50, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("uses defaults for negative limit", func(t *testing.T) {
		limit, offset := clampPagination(-1, 0, 50, 1000)
		assert.Equal(t, 50, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("caps limit at max", func(t *testing.T) {
		limit, offset := clampPagination(5000, 0, 50, 1000)
		assert.Equal(t, 1000, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("passes through valid limit", func(t *testing.T) {
		limit, offset := clampPagination(200, 10, 50, 1000)
		assert.Equal(t, 200, limit)
		assert.Equal(t, 10, offset)
	})

	t.Run("clamps negative offset to zero", func(t *testing.T) {
		limit, offset := clampPagination(50, -5, 50, 1000)
		assert.Equal(t, 50, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("limit exactly at max is kept", func(t *testing.T) {
		limit, _ := clampPagination(1000, 0, 50, 1000)
		assert.Equal(t, 1000, limit)
	})

	t.Run("limit of 1 is valid", func(t *testing.T) {
		limit, offset := clampPagination(1, 0, 50, 1000)
		assert.Equal(t, 1, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("respects different defaults", func(t *testing.T) {
		limit, offset := clampPagination(0, 0, 200, 500)
		assert.Equal(t, 200, limit)
		assert.Equal(t, 0, offset)

		limit, offset = clampPagination(600, 0, 200, 500)
		assert.Equal(t, 500, limit)
		assert.Equal(t, 0, offset)
	})
}
