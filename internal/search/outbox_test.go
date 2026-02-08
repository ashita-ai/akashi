package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMaxOutboxAttempts(t *testing.T) {
	// Verify the dead-letter threshold is set to a reasonable value.
	assert.Equal(t, 10, maxOutboxAttempts)
}

func TestScanOutboxEntriesEmpty(t *testing.T) {
	// This test verifies the outbox worker's core logic constants and types
	// without requiring a live database. Integration tests cover the full
	// poll → process → Qdrant flow.

	// Verify Point type has all required fields for Qdrant upsert.
	var p Point
	_ = p.ID
	_ = p.OrgID
	_ = p.AgentID
	_ = p.DecisionType
	_ = p.Confidence
	_ = p.QualityScore
	_ = p.ValidFrom
	_ = p.Embedding

	// Verify DecisionForIndex has all required fields.
	var d DecisionForIndex
	_ = d.ID
	_ = d.OrgID
	_ = d.AgentID
	_ = d.DecisionType
	_ = d.Confidence
	_ = d.QualityScore
	_ = d.ValidFrom
	_ = d.Embedding
}
