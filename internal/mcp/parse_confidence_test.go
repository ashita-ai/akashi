package mcp

import (
	"encoding/json"
	"math"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeRequest builds a CallToolRequest carrying the given arguments map.
func makeRequest(args map[string]any) mcplib.CallToolRequest {
	return mcplib.CallToolRequest{
		Params: mcplib.CallToolParams{
			Name:      "akashi_trace",
			Arguments: args,
		},
	}
}

// TestParseTraceConfidence_Accepts covers shapes a real MCP client may
// produce — JSON numbers (decoded as float64), ints, json.Number, and
// stringified floats — all of which should yield the expected float32.
func TestParseTraceConfidence_Accepts(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want float32
	}{
		{"float64 mid", float64(0.85), 0.85},
		{"float64 zero", float64(0), 0},
		{"float64 one", float64(1), 1},
		{"float32", float32(0.42), 0.42},
		{"int", int(1), 1},
		{"int64", int64(0), 0},
		{"json.Number", json.Number("0.75"), 0.75},
		{"stringified float", "0.6", 0.6},
		{"stringified zero", "0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTraceConfidence(makeRequest(map[string]any{"confidence": tc.raw}))
			require.NoError(t, err)
			assert.InDelta(t, tc.want, got, 1e-6)
		})
	}
}

// TestParseTraceConfidence_Rejects covers everything the old GetFloat path
// collapsed onto the silent 0.4 default. Each case must surface a
// descriptive error rather than fall through to a fabricated value.
func TestParseTraceConfidence_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantSub string
	}{
		{
			name:    "missing key",
			args:    map[string]any{},
			wantSub: "confidence is required",
		},
		{
			name:    "explicit nil",
			args:    map[string]any{"confidence": nil},
			wantSub: "confidence is required",
		},
		{
			name:    "unparseable string",
			args:    map[string]any{"confidence": "high"},
			wantSub: "not a valid number",
		},
		{
			name:    "bool",
			args:    map[string]any{"confidence": true},
			wantSub: "must be a number",
		},
		{
			name:    "map",
			args:    map[string]any{"confidence": map[string]any{"value": 0.5}},
			wantSub: "must be a number",
		},
		{
			name:    "slice",
			args:    map[string]any{"confidence": []any{0.5}},
			wantSub: "must be a number",
		},
		{
			name:    "NaN",
			args:    map[string]any{"confidence": math.NaN()},
			wantSub: "finite number",
		},
		{
			name:    "positive infinity",
			args:    map[string]any{"confidence": math.Inf(1)},
			wantSub: "finite number",
		},
		{
			name:    "negative infinity",
			args:    map[string]any{"confidence": math.Inf(-1)},
			wantSub: "finite number",
		},
		{
			name:    "below zero",
			args:    map[string]any{"confidence": -0.01},
			wantSub: "between 0 and 1",
		},
		{
			name:    "above one",
			args:    map[string]any{"confidence": 1.01},
			wantSub: "between 0 and 1",
		},
		{
			name:    "string above one",
			args:    map[string]any{"confidence": "1.5"},
			wantSub: "between 0 and 1",
		},
		{
			name:    "bad json.Number",
			args:    map[string]any{"confidence": json.Number("not-a-number")},
			wantSub: "not a valid number",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTraceConfidence(makeRequest(tc.args))
			require.Error(t, err, "case %s must reject the input", tc.name)
			assert.Equal(t, float32(0), got, "must not leak a fabricated value")
			assert.Contains(t, err.Error(), tc.wantSub,
				"error message should describe why the input is invalid")
		})
	}
}
