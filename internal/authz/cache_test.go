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
