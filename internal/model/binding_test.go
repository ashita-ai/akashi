package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Canonicalization decides which parameters join. Too little and real
// collisions are missed; too much and unrelated parameters are merged, which
// manufactures conflicts and destroys the one property this feature has —
// that a detection built on a binding is certain.
func TestBindingCanonicalize_SpellingVariantsJoin(t *testing.T) {
	variants := []string{
		"qdrant.create_field_index_on_startup",
		"Qdrant.CreateFieldIndex_on_startup",
		"qdrant create field index on startup",
		"qdrant-create-field-index-on-startup",
		"  QDRANT/CREATE/FIELD/INDEX/ON/STARTUP  ",
	}
	var key string
	for _, v := range variants {
		b := Binding{Parameter: v, Value: "true"}
		require.NoError(t, b.Canonicalize(), "variant %q", v)
		if key == "" {
			key = b.ParameterKey
			continue
		}
		assert.Equal(t, key, b.ParameterKey, "variant %q must canonicalize like the others", v)
	}
	assert.Equal(t, "qdrant.create.field.index.on.startup", key)
}

// Values are compared, never interpreted. Deciding "5m" and "300s" are the
// same requires the parameter's type, which is not recorded, so treating them
// as equal would be a guess — and a guess that suppresses a real conflict.
func TestBindingCanonicalize_ValuesAreNotInterpreted(t *testing.T) {
	for _, pair := range [][2]string{
		{"5m", "300s"},
		{"true", "yes"},
		{"1", "1.0"},
	} {
		a := Binding{Parameter: "p", Value: pair[0]}
		b := Binding{Parameter: "p", Value: pair[1]}
		require.NoError(t, a.Canonicalize())
		require.NoError(t, b.Canonicalize())
		assert.NotEqual(t, a.ValueKey, b.ValueKey,
			"%q and %q must stay distinct — equating them needs a type we do not have", pair[0], pair[1])
	}
}

// Case and separator differences in a VALUE should not read as disagreement.
func TestBindingCanonicalize_ValueSpellingIsNormalized(t *testing.T) {
	a := Binding{Parameter: "p", Value: "PostgreSQL Only"}
	b := Binding{Parameter: "p", Value: "postgresql-only"}
	require.NoError(t, a.Canonicalize())
	require.NoError(t, b.Canonicalize())
	assert.Equal(t, a.ValueKey, b.ValueKey)
}

func TestBindingCanonicalize_Rejects(t *testing.T) {
	cases := map[string]Binding{
		"empty parameter":      {Parameter: "", Value: "x"},
		"blank parameter":      {Parameter: "   ", Value: "x"},
		"empty value":          {Parameter: "p", Value: ""},
		"separator-only param": {Parameter: "...", Value: "x"},
		"separator-only value": {Parameter: "p", Value: "___"},
		"over-long parameter":  {Parameter: strings.Repeat("a", maxBindingParameterLen+1), Value: "x"},
		"over-long value":      {Parameter: "p", Value: strings.Repeat("a", maxBindingValueLen+1)},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, b.Canonicalize())
		})
	}
}

// A decision that binds the same parameter twice contradicts itself inside a
// single trace. That is a caller bug, and storing it would mean re-discovering
// it later as a conflict between a decision and itself.
func TestCanonicalizeBindings_RejectsIntraDecisionDuplicate(t *testing.T) {
	_, err := CanonicalizeBindings([]Binding{
		{Parameter: "cache.ttl", Value: "5m"},
		{Parameter: "Cache TTL", Value: "10m"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bound twice")
}

// Binding the same parameter to the same value twice is still a duplicate:
// the row is redundant and the unique constraint would reject it at insert.
func TestCanonicalizeBindings_RejectsDuplicateEvenWhenValuesAgree(t *testing.T) {
	_, err := CanonicalizeBindings([]Binding{
		{Parameter: "cache.ttl", Value: "5m"},
		{Parameter: "cache.ttl", Value: "5m"},
	})
	assert.Error(t, err)
}

func TestCanonicalizeBindings_EnforcesFanOutLimit(t *testing.T) {
	many := make([]Binding, MaxBindingsPerDecision+1)
	for i := range many {
		many[i] = Binding{Parameter: string(rune('a'+i%26)) + strings.Repeat("x", i), Value: "v"}
	}
	_, err := CanonicalizeBindings(many)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the limit")
}

func TestCanonicalizeBindings_EmptyIsNotAnError(t *testing.T) {
	out, err := CanonicalizeBindings(nil)
	require.NoError(t, err)
	assert.Nil(t, out, "bindings are optional; absent must stay absent rather than becoming an empty slice")
}

func TestCanonicalizeBindings_PreservesRawSpelling(t *testing.T) {
	out, err := CanonicalizeBindings([]Binding{{Parameter: "  Cache TTL  ", Value: "  5 Minutes "}})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "Cache TTL", out[0].Parameter, "the agent's spelling is what a human reads back")
	assert.Equal(t, "5 Minutes", out[0].Value)
	assert.Equal(t, "cache.ttl", out[0].ParameterKey)
}
