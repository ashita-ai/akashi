package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// The enrichment structs must not carry any pre-access-filter quantity.
// GET /v1/runs/{run_id} is a read-role route and enrichments span agents the
// caller may hold no grant for, so a "total" field is an oracle for hidden
// records. This test fails if anyone reintroduces one.
func TestEnrichmentResponses_PublishNoTotals(t *testing.T) {
	for name, v := range map[string]any{
		"revisions": enrichmentRevisions{Items: []model.Decision{}, Count: 3},
		"conflicts": enrichmentConflicts{Items: []model.DecisionConflict{}, Count: 3},
	} {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(b, &m))
		_, hasTotal := m["total"]
		assert.False(t, hasTotal, "%s must not publish a total field: %s", name, b)
	}
}

func TestCapEnrichmentConflicts(t *testing.T) {
	mk := func(n int) []model.DecisionConflict { return make([]model.DecisionConflict, n) }
	for _, tc := range []struct {
		name        string
		in          int
		wantLen     int
		wantHasMore bool
	}{
		{"empty", 0, 0, false},
		{"under cap", 49, 49, false},
		{"at cap", maxEnrichmentConflicts, maxEnrichmentConflicts, true},
		{"over cap (storage +1 probe)", maxEnrichmentConflicts + 1, maxEnrichmentConflicts, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, hasMore := capEnrichmentConflicts(mk(tc.in))
			assert.Len(t, items, tc.wantLen)
			assert.Equal(t, tc.wantHasMore, hasMore)
		})
	}
}
