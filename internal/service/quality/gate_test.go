package quality_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/service/quality"
)

func TestParseGateMode(t *testing.T) {
	cases := []struct {
		input string
		want  quality.GateMode
		err   bool
	}{
		{"", quality.GateModeOff, false},
		{"off", quality.GateModeOff, false},
		{"OFF", quality.GateModeOff, false},
		{"disabled", quality.GateModeOff, false},
		{"  warn  ", quality.GateModeWarn, false},
		{"warning", quality.GateModeWarn, false},
		{"reject", quality.GateModeReject, false},
		{"block", quality.GateModeReject, false},
		{"strict", "", true},
		{"true", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := quality.ParseGateMode(tc.input)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCompletenessGate_OffMode_AlwaysPasses(t *testing.T) {
	g := quality.CompletenessGate{Mode: quality.GateModeOff, Threshold: 0.9}
	r := g.Evaluate(0.05, "code_review")
	assert.False(t, r.Below, "off mode must never trip the gate even on very low scores")
	assert.Empty(t, r.WarningMessage("code_review"))
}

func TestCompletenessGate_ZeroValue_AlwaysPasses(t *testing.T) {
	// The zero value of CompletenessGate is the safe default — service code
	// not yet wired to operator config must behave exactly as pre-#715.
	var g quality.CompletenessGate
	r := g.Evaluate(0.01, "architecture")
	assert.False(t, r.Below)
	assert.Equal(t, quality.GateMode(""), r.Mode)
}

func TestCompletenessGate_RejectMode_BelowThresholdFires(t *testing.T) {
	g := quality.CompletenessGate{Mode: quality.GateModeReject, Threshold: 0.30}
	r := g.Evaluate(0.20, "code_review")
	assert.True(t, r.Below)
	assert.InDelta(t, 0.30, r.Threshold, 0.0001)
	assert.InDelta(t, 0.20, r.Score, 0.0001)
}

func TestCompletenessGate_ExactThresholdPasses(t *testing.T) {
	// Exact equality must pass. Strict-below semantics avoid surprising
	// rejections caused by floating-point round-off at threshold boundaries.
	g := quality.CompletenessGate{Mode: quality.GateModeReject, Threshold: 0.30}
	r := g.Evaluate(0.30, "code_review")
	assert.False(t, r.Below)
}

func TestCompletenessGate_PerTypeOverridesGlobal(t *testing.T) {
	g := quality.CompletenessGate{
		Mode:      quality.GateModeReject,
		Threshold: 0.30,
		ByType: map[string]float32{
			"security":     0.60,
			"architecture": 0.55,
		},
	}
	// Score 0.45 passes the global 0.30 but fails the per-type override for
	// security and architecture.
	assert.True(t, g.Evaluate(0.45, "security").Below)
	assert.True(t, g.Evaluate(0.45, "architecture").Below)
	assert.False(t, g.Evaluate(0.45, "code_review").Below)
	assert.False(t, g.Evaluate(0.45, "investigation").Below)
}

func TestCompletenessGate_PerTypeZeroDisablesGate(t *testing.T) {
	// Mapping a type to 0 in ByType exempts it from the global floor —
	// useful when operators want strict security/architecture bars but
	// want to allow brief investigation traces unconditionally.
	g := quality.CompletenessGate{
		Mode:      quality.GateModeReject,
		Threshold: 0.40,
		ByType:    map[string]float32{"investigation": 0},
	}
	assert.False(t, g.Evaluate(0.01, "investigation").Below)
	assert.True(t, g.Evaluate(0.01, "code_review").Below)
}

func TestCompletenessGate_GlobalZero_OnlyTypedGateFires(t *testing.T) {
	// Threshold 0 with ByType set means "no global floor, but enforce these
	// specific types." Used to opt only the high-stakes types into a bar.
	g := quality.CompletenessGate{
		Mode:      quality.GateModeReject,
		Threshold: 0,
		ByType:    map[string]float32{"security": 0.60},
	}
	assert.True(t, g.Evaluate(0.30, "security").Below)
	assert.False(t, g.Evaluate(0.30, "code_review").Below)
	assert.False(t, g.Evaluate(0.30, "investigation").Below)
}

func TestCompletenessGate_WarningMessageStable(t *testing.T) {
	g := quality.CompletenessGate{Mode: quality.GateModeWarn, Threshold: 0.50}
	r := g.Evaluate(0.25, "code_review")
	msg := r.WarningMessage("code_review")
	require.NotEmpty(t, msg)
	// The message must contain the score, threshold, and decision type so
	// operators can act on it without correlating against other logs.
	assert.Contains(t, msg, "0.25")
	assert.Contains(t, msg, "0.50")
	assert.Contains(t, msg, "code_review")
}

func TestCompletenessGate_WarningMessageEmptyWhenPassing(t *testing.T) {
	g := quality.CompletenessGate{Mode: quality.GateModeWarn, Threshold: 0.50}
	r := g.Evaluate(0.80, "code_review")
	assert.Empty(t, r.WarningMessage("code_review"))
}

func TestParseGateByType_EmptyReturnsNil(t *testing.T) {
	m, err := quality.ParseGateByType("")
	require.NoError(t, err)
	assert.Nil(t, m)

	m, err = quality.ParseGateByType("   ")
	require.NoError(t, err)
	assert.Nil(t, m)
}

func TestParseGateByType_NormalizesKeysToLowercase(t *testing.T) {
	m, err := quality.ParseGateByType(`{"Security": 0.6, "ARCHITECTURE": 0.55}`)
	require.NoError(t, err)
	assert.InDelta(t, 0.60, m["security"], 0.0001)
	assert.InDelta(t, 0.55, m["architecture"], 0.0001)
	// Original-case keys must NOT be present — that would cause silent
	// misses when the trace pipeline normalizes types to lowercase first.
	_, hasUpper := m["Security"]
	assert.False(t, hasUpper)
}

func TestParseGateByType_RejectsOutOfRange(t *testing.T) {
	_, err := quality.ParseGateByType(`{"security": 1.5}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")

	_, err = quality.ParseGateByType(`{"security": -0.1}`)
	require.Error(t, err)
}

func TestParseGateByType_RejectsEmptyKey(t *testing.T) {
	_, err := quality.ParseGateByType(`{"": 0.5}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestParseGateByType_RejectsInvalidJSON(t *testing.T) {
	_, err := quality.ParseGateByType(`{not json}`)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "invalid"))
}
