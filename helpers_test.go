package akashi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDerefOr(t *testing.T) {
	t.Run("returns value when pointer is non-nil", func(t *testing.T) {
		s := "hello"
		assert.Equal(t, "hello", derefOr(&s, "fallback"))
	})

	t.Run("returns fallback when pointer is nil", func(t *testing.T) {
		var s *string
		assert.Equal(t, "fallback", derefOr(s, "fallback"))
	})

	t.Run("works with float64", func(t *testing.T) {
		f := 0.95
		assert.Equal(t, 0.95, derefOr(&f, 0.0))
	})

	t.Run("returns fallback for nil float64", func(t *testing.T) {
		var f *float64
		assert.Equal(t, 0.0, derefOr(f, 0.0))
	})

	t.Run("returns zero-value pointer content", func(t *testing.T) {
		s := ""
		assert.Equal(t, "", derefOr(&s, "fallback"))
	})
}
