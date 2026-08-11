package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ashita-ai/akashi/internal/model"
)

// The enrichment structs must not carry any pre-access-filter quantity.
// GET /v1/runs/{run_id} is a read-role route and enrichments span agents the
// caller may hold no grant for, so a "total" field is an oracle for hidden
// records. This test fails if anyone reintroduces one.
func TestEnrichmentResponses_PublishNoTotals(t *testing.T) {
	// Reflect over the struct definition rather than marshalling one instance.
	// The instance-based form this replaces was blind to the reintroduction it
	// existed to catch: a re-added `Total int \`json:"total,omitempty"\`` sits at
	// its zero value in a literal, omitempty elides the key, and the assertion
	// passes while the field ships to every caller that populates it.
	for name, typ := range map[string]reflect.Type{
		"revisions": reflect.TypeOf(enrichmentRevisions{}),
		"conflicts": reflect.TypeOf(enrichmentConflicts{}),
	} {
		for i := range typ.NumField() {
			f := typ.Field(i)
			tag := f.Tag.Get("json")
			assert.NotContains(t, strings.ToLower(f.Name), "total",
				"%s must not carry a total-derived field: %s", name, f.Name)
			assert.NotContains(t, strings.ToLower(tag), "total",
				"%s must not publish a total-derived field: json tag %q", name, tag)
		}
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
		// Exactly at the cap nothing was withheld, so has_more must be false.
		// Storage over-fetches cap+1, so a slice of exactly cap proves the
		// accessible set is exactly cap. The earlier version of this case
		// asserted true and pinned an off-by-one that would have rendered a
		// misleading "50+" for every caller, admins included.
		{"at cap, nothing withheld", maxEnrichmentConflicts, maxEnrichmentConflicts, false},
		{"over cap (storage +1 probe)", maxEnrichmentConflicts + 1, maxEnrichmentConflicts, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, hasMore := capEnrichmentConflicts(mk(tc.in))
			assert.Len(t, items, tc.wantLen)
			assert.Equal(t, tc.wantHasMore, hasMore)
		})
	}
}
