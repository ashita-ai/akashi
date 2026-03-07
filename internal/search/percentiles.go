package search

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OrgPercentiles holds precomputed signal percentile breakpoints for a single org.
// Breakpoints are the values at the 25th, 50th, 75th, and 90th percentiles of the
// org's decision corpus. Used by ReScore to normalize raw signal counts into
// distribution-aware [0, 1] scores (issue #264).
type OrgPercentiles struct {
	// CitationBreakpoints holds percentile breakpoints [p25, p50, p75, p90] for
	// precedent citation counts within this org. When empty, ReScore falls back
	// to the logarithmic cap formula.
	CitationBreakpoints []float64
	RefreshedAt         time.Time
}

// PercentileCache holds per-org signal breakpoints, refreshed periodically by a background loop.
// Safe for concurrent use.
type PercentileCache struct {
	mu   sync.RWMutex
	orgs map[uuid.UUID]OrgPercentiles
}

// NewPercentileCache creates an empty cache.
func NewPercentileCache() *PercentileCache {
	return &PercentileCache{
		orgs: make(map[uuid.UUID]OrgPercentiles),
	}
}

// Get returns the percentile data for an org. Returns nil if not cached.
func (c *PercentileCache) Get(orgID uuid.UUID) *OrgPercentiles {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.orgs[orgID]
	if !ok {
		return nil
	}
	return &p
}

// Set stores percentile data for an org.
func (c *PercentileCache) Set(orgID uuid.UUID, p OrgPercentiles) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.orgs[orgID] = p
}

// PercentileScore maps a raw value to [0, 1] based on its position relative to the
// given breakpoints [p25, p50, p75, p90]. The mapping is:
//
//	value <= 0        → 0.0
//	value <= p25      → 0.25 * (value / p25)
//	value <= p50      → 0.25 + 0.25 * ((value - p25) / (p50 - p25))
//	value <= p75      → 0.50 + 0.25 * ((value - p50) / (p75 - p50))
//	value <= p90      → 0.75 + 0.15 * ((value - p75) / (p90 - p75))
//	value > p90       → 0.90 + 0.10 * min((value - p90) / p90, 1.0)
//
// If breakpoints is empty or all zeros, returns 0.0 (cold-start fallback).
// Breakpoints must be sorted ascending and len >= 4.
func PercentileScore(value float64, breakpoints []float64) float64 {
	if len(breakpoints) < 4 || value <= 0 {
		return 0.0
	}

	p25, p50, p75, p90 := breakpoints[0], breakpoints[1], breakpoints[2], breakpoints[3]

	// All breakpoints at zero means no meaningful distribution data.
	if p90 == 0 {
		return 0.0
	}

	// Ensure breakpoints are usable — degenerate cases where adjacent breakpoints collapse.
	switch {
	case value <= p25:
		if p25 == 0 {
			return 0.0
		}
		return 0.25 * (value / p25)
	case value <= p50:
		span := p50 - p25
		if span == 0 {
			return 0.50
		}
		return 0.25 + 0.25*((value-p25)/span)
	case value <= p75:
		span := p75 - p50
		if span == 0 {
			return 0.75
		}
		return 0.50 + 0.25*((value-p50)/span)
	case value <= p90:
		span := p90 - p75
		if span == 0 {
			return 0.90
		}
		return 0.75 + 0.15*((value-p75)/span)
	default:
		// Beyond p90: allow scores up to 1.0 for extreme outliers.
		if p90 == 0 {
			return 1.0
		}
		extra := (value - p90) / p90
		if extra > 1.0 {
			extra = 1.0
		}
		return 0.90 + 0.10*extra
	}
}

// BreakpointsFromValues computes [p25, p50, p75, p90] breakpoints from a slice of values.
// Used in tests and by the percentile refresh loop to convert raw query results to breakpoints.
// Returns nil if values is empty.
func BreakpointsFromValues(values []float64) []float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	return []float64{
		interpolatedPercentile(sorted, 0.25),
		interpolatedPercentile(sorted, 0.50),
		interpolatedPercentile(sorted, 0.75),
		interpolatedPercentile(sorted, 0.90),
	}
}

// interpolatedPercentile computes the p-th percentile (0.0-1.0) from sorted values
// using linear interpolation (matching PostgreSQL's percentile_cont behavior).
func interpolatedPercentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := p * float64(n-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
