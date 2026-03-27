package compact

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestSetIfPresent(t *testing.T) {
	t.Run("sets value when pointer is non-nil", func(t *testing.T) {
		m := make(map[string]any)
		s := "value"
		setIfPresent(m, "key", &s)
		assert.Equal(t, "value", m["key"])
	})

	t.Run("does not set when pointer is nil", func(t *testing.T) {
		m := make(map[string]any)
		var s *string
		setIfPresent(m, "key", s)
		_, exists := m["key"]
		assert.False(t, exists)
	})

	t.Run("stores dereferenced value not pointer", func(t *testing.T) {
		m := make(map[string]any)
		id := uuid.New()
		setIfPresent(m, "id", &id)
		// The stored value should be uuid.UUID, not *uuid.UUID.
		_, ok := m["id"].(uuid.UUID)
		assert.True(t, ok, "expected uuid.UUID, got %T", m["id"])
	})

	t.Run("overwrites existing key", func(t *testing.T) {
		m := map[string]any{"key": "old"}
		s := "new"
		setIfPresent(m, "key", &s)
		assert.Equal(t, "new", m["key"])
	})

	t.Run("works with int", func(t *testing.T) {
		m := make(map[string]any)
		n := 42
		setIfPresent(m, "count", &n)
		assert.Equal(t, 42, m["count"])
	})
}
