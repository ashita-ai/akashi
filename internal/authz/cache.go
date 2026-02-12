package authz

import (
	"sync"
	"time"
)

// GrantCache is a short-TTL in-memory cache for LoadGrantedSet results.
// It eliminates 2-3 DB queries per request for non-admin users by caching
// the set of agent_ids each caller can access.
//
// Key: "org_id:subject_uuid" (from JWT claims).
// Value: map[string]bool (set of accessible agent_ids) + expiry time.
// A nil map value means unrestricted (admin).
type GrantCache struct {
	mu      sync.RWMutex
	entries map[string]cachedEntry
	ttl     time.Duration
	done    chan struct{}
}

type cachedEntry struct {
	granted   map[string]bool
	expiresAt time.Time
}

// NewGrantCache creates a new cache with the given TTL.
// Call Close to stop the background eviction goroutine.
func NewGrantCache(ttl time.Duration) *GrantCache {
	c := &GrantCache{
		entries: make(map[string]cachedEntry),
		ttl:     ttl,
		done:    make(chan struct{}),
	}
	go c.evictLoop()
	return c
}

// Get returns the cached granted set and true if a valid entry exists.
// Returns nil, false on miss or expiry.
func (c *GrantCache) Get(key string) (map[string]bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.granted, true
}

// Set stores a granted set with the configured TTL.
func (c *GrantCache) Set(key string, granted map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cachedEntry{
		granted:   granted,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Close stops the background eviction goroutine.
func (c *GrantCache) Close() {
	close(c.done)
}

// evictLoop removes expired entries every minute.
func (c *GrantCache) evictLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.evictExpired()
		}
	}
}

func (c *GrantCache) evictExpired() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			delete(c.entries, k)
		}
	}
}
