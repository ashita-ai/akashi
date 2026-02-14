package authz

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGrantCache_GetSet(t *testing.T) {
	c := NewGrantCache(time.Second)
	defer c.Close()

	// Miss on empty cache.
	got, ok := c.Get("org:sub")
	assert.False(t, ok)
	assert.Nil(t, got)

	// Set and hit.
	granted := map[string]bool{"agent-a": true, "agent-b": true}
	c.Set("org:sub", granted)

	got, ok = c.Get("org:sub")
	require.True(t, ok)
	assert.Equal(t, granted, got)
}

func TestGrantCache_DefensiveCopies(t *testing.T) {
	c := NewGrantCache(time.Second)
	defer c.Close()

	original := map[string]bool{"agent-a": true}
	c.Set("org:sub", original)
	original["agent-b"] = true // mutate caller map after Set

	got, ok := c.Get("org:sub")
	require.True(t, ok)
	assert.True(t, got["agent-a"])
	assert.False(t, got["agent-b"], "cache should not retain caller-side mutations")

	// Mutating returned map must not affect subsequent reads.
	got["agent-c"] = true
	got2, ok := c.Get("org:sub")
	require.True(t, ok)
	assert.False(t, got2["agent-c"], "cache Get should return a defensive copy")
}

func TestGrantCache_NilValueDistinguishedFromMiss(t *testing.T) {
	c := NewGrantCache(time.Second)
	defer c.Close()

	// Nil map is a valid cached value (means unrestricted / admin).
	// It should be distinguishable from a cache miss.
	c.Set("admin:key", nil)

	got, ok := c.Get("admin:key")
	assert.True(t, ok, "nil value should be a cache hit")
	assert.Nil(t, got)
}

func TestGrantCache_Expiry(t *testing.T) {
	c := NewGrantCache(50 * time.Millisecond)
	defer c.Close()

	c.Set("org:sub", map[string]bool{"a": true})

	// Should be present immediately.
	_, ok := c.Get("org:sub")
	require.True(t, ok)

	// Wait for expiry.
	time.Sleep(60 * time.Millisecond)

	_, ok = c.Get("org:sub")
	assert.False(t, ok, "entry should have expired")
}

func TestGrantCache_EvictExpired(t *testing.T) {
	c := NewGrantCache(10 * time.Millisecond)
	defer c.Close()

	c.Set("key1", map[string]bool{"a": true})
	c.Set("key2", map[string]bool{"b": true})

	time.Sleep(20 * time.Millisecond)

	c.evictExpired()

	c.mu.RLock()
	assert.Empty(t, c.entries, "evictExpired should have removed all expired entries")
	c.mu.RUnlock()
}

func TestGrantCache_DifferentKeys(t *testing.T) {
	c := NewGrantCache(time.Second)
	defer c.Close()

	c.Set("org1:sub1", map[string]bool{"a": true})
	c.Set("org2:sub2", map[string]bool{"b": true})

	got1, ok := c.Get("org1:sub1")
	require.True(t, ok)
	assert.True(t, got1["a"])
	assert.False(t, got1["b"])

	got2, ok := c.Get("org2:sub2")
	require.True(t, ok)
	assert.True(t, got2["b"])
	assert.False(t, got2["a"])
}

func TestGrantCache_Invalidate(t *testing.T) {
	c := NewGrantCache(time.Minute) // long TTL so entries don't expire during test
	defer c.Close()

	// Populate cache with two entries.
	c.Set("org:user-a", map[string]bool{"agent-1": true, "agent-2": true})
	c.Set("org:user-b", map[string]bool{"agent-3": true})

	// Verify both are present.
	got, ok := c.Get("org:user-a")
	require.True(t, ok)
	assert.Len(t, got, 2)

	got, ok = c.Get("org:user-b")
	require.True(t, ok)
	assert.Len(t, got, 1)

	// Invalidate one entry.
	c.Invalidate("org:user-a")

	// The invalidated key should miss.
	got, ok = c.Get("org:user-a")
	assert.False(t, ok, "invalidated entry should be a cache miss")
	assert.Nil(t, got)

	// The other key should still be present.
	got, ok = c.Get("org:user-b")
	require.True(t, ok, "non-invalidated entry should still be cached")
	assert.True(t, got["agent-3"])
}

func TestGrantCache_Invalidate_NonExistentKey(t *testing.T) {
	c := NewGrantCache(time.Minute)
	defer c.Close()

	// Invalidating a key that was never set should not panic.
	c.Invalidate("does-not-exist")

	// Cache should still be functional.
	c.Set("key", map[string]bool{"x": true})
	got, ok := c.Get("key")
	require.True(t, ok)
	assert.True(t, got["x"])
}

func TestGrantCache_Invalidate_ThenReSet(t *testing.T) {
	c := NewGrantCache(time.Minute)
	defer c.Close()

	// Set, invalidate, then re-set with different data.
	c.Set("org:user", map[string]bool{"old-agent": true})
	c.Invalidate("org:user")

	_, ok := c.Get("org:user")
	require.False(t, ok, "should miss after invalidation")

	// Re-set with new data.
	c.Set("org:user", map[string]bool{"new-agent": true})

	got, ok := c.Get("org:user")
	require.True(t, ok, "should hit after re-set")
	assert.True(t, got["new-agent"])
	assert.False(t, got["old-agent"], "old data should not reappear")
}

func TestTagsOverlap(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want bool
	}{
		{
			name: "both empty",
			a:    []string{},
			b:    []string{},
			want: false,
		},
		{
			name: "a empty",
			a:    []string{},
			b:    []string{"backend"},
			want: false,
		},
		{
			name: "b empty",
			a:    []string{"backend"},
			b:    []string{},
			want: false,
		},
		{
			name: "a nil",
			a:    nil,
			b:    []string{"backend"},
			want: false,
		},
		{
			name: "b nil",
			a:    []string{"backend"},
			b:    nil,
			want: false,
		},
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: false,
		},
		{
			name: "no overlap",
			a:    []string{"backend", "infra"},
			b:    []string{"frontend", "design"},
			want: false,
		},
		{
			name: "single overlap",
			a:    []string{"backend", "infra"},
			b:    []string{"infra", "security"},
			want: true,
		},
		{
			name: "multiple overlaps",
			a:    []string{"backend", "infra", "security"},
			b:    []string{"infra", "security", "devops"},
			want: true,
		},
		{
			name: "identical sets",
			a:    []string{"a", "b", "c"},
			b:    []string{"a", "b", "c"},
			want: true,
		},
		{
			name: "single element match",
			a:    []string{"team-alpha"},
			b:    []string{"team-alpha"},
			want: true,
		},
		{
			name: "single element no match",
			a:    []string{"team-alpha"},
			b:    []string{"team-beta"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tagsOverlap(tt.a, tt.b)
			assert.Equal(t, tt.want, got)
		})
	}
}
