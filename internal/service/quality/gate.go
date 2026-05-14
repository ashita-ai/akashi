package quality

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GateMode controls how the completeness gate reacts to a trace whose
// completeness score falls below the configured threshold.
type GateMode string

const (
	// GateModeOff disables the gate. Traces are accepted regardless of score.
	// This is the default and preserves pre-#715 behavior.
	GateModeOff GateMode = "off"

	// GateModeWarn accepts low-completeness traces but attaches a warning to
	// the response so the caller can surface it to the agent or operator.
	GateModeWarn GateMode = "warn"

	// GateModeReject refuses to store low-completeness traces; the trace
	// endpoint returns a 4xx error with a descriptive code.
	GateModeReject GateMode = "reject"
)

// ParseGateMode normalizes the input string to a known GateMode.
// The empty string maps to GateModeOff to keep zero-value behavior safe.
// Unrecognized values return an error so misconfiguration is surfaced loudly
// at startup rather than silently disabling the gate.
func ParseGateMode(s string) (GateMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "disabled":
		return GateModeOff, nil
	case "warn", "warning":
		return GateModeWarn, nil
	case "reject", "block":
		return GateModeReject, nil
	default:
		return "", fmt.Errorf("invalid completeness gate mode %q (want off|warn|reject)", s)
	}
}

// CompletenessGate is the operator-configurable structural-quality gate for
// trace ingest. A new trace is gated by the threshold for its decision_type
// (per-type override if set, otherwise the global Threshold).
//
// The zero value is safe: Mode defaults to GateModeOff which short-circuits
// every evaluation as a pass, so service code that hasn't been wired to the
// gate yet behaves exactly as before.
type CompletenessGate struct {
	// Mode selects between off (default), warn, and reject behavior.
	Mode GateMode

	// Threshold is the global minimum completeness score. Decisions whose
	// type does not appear in ByType are evaluated against this value.
	// Range: [0.0, 1.0]. Zero is treated as "no global floor" — the gate
	// then only fires for types listed in ByType.
	Threshold float32

	// ByType is an optional per-decision-type floor that overrides Threshold
	// for the named type. Keys must be lowercase canonical decision_type
	// values (e.g. "security", "architecture"). Use this to set a stricter
	// bar for high-stakes types without raising the global floor.
	ByType map[string]float32
}

// ThresholdFor returns the effective threshold for a decision_type.
// Per-type overrides take precedence over the global threshold. The lookup is
// case-sensitive against lowercase keys, since decision types are normalized
// to lowercase before reaching the gate.
func (g CompletenessGate) ThresholdFor(decisionType string) float32 {
	if t, ok := g.ByType[decisionType]; ok {
		return t
	}
	return g.Threshold
}

// GateResult is the outcome of evaluating a single trace against the gate.
// Below is true only when the gate is active (Mode != off), the threshold is
// positive, AND the score falls strictly below the threshold. A score that
// exactly equals the threshold passes — this matches the convention used for
// the rest of the codebase's threshold comparisons (e.g. confidence factor
// uses strict inequalities at boundaries).
type GateResult struct {
	// Mode is the gate mode at evaluation time. Carried on the result so the
	// caller can decide between rejecting, warning, or emitting telemetry
	// without re-reading service state.
	Mode GateMode

	// Threshold is the effective threshold the score was compared against.
	Threshold float32

	// Score is the score that was evaluated, returned for telemetry and
	// human-readable error/warning messages.
	Score float32

	// Below indicates the score fell strictly below the threshold AND the
	// gate is active. When false, no action should be taken regardless of mode.
	Below bool
}

// Evaluate compares score against the threshold for decisionType and returns
// a structured result. The function is pure; callers translate the result
// into errors (reject), warnings (warn), or no-ops (off, or above threshold).
//
// A non-positive threshold disables evaluation for that type regardless of
// mode. This lets operators turn the gate on globally while exempting a
// specific type by mapping it to 0 in ByType.
func (g CompletenessGate) Evaluate(score float32, decisionType string) GateResult {
	threshold := g.ThresholdFor(decisionType)
	result := GateResult{
		Mode:      g.Mode,
		Threshold: threshold,
		Score:     score,
	}
	if g.Mode == GateModeOff || g.Mode == "" {
		return result
	}
	if threshold <= 0 {
		return result
	}
	if score < threshold {
		result.Below = true
	}
	return result
}

// WarningMessage returns a stable, human-readable string for warn-mode
// responses. Format is deliberately consistent so downstream tooling can
// parse it. Returns empty string when the result did not trip the gate.
func (r GateResult) WarningMessage(decisionType string) string {
	if !r.Below {
		return ""
	}
	return fmt.Sprintf(
		"completeness score %.2f for decision_type=%q is below the configured minimum of %.2f; this decision may be filtered from quality-sensitive queries",
		r.Score, decisionType, r.Threshold,
	)
}

// ParseGateByType parses a JSON object mapping decision_type strings to
// float thresholds. Returns an empty (non-nil) map for empty input so
// downstream lookups never panic on nil. Keys are normalized to lowercase
// and trimmed; values must be in [0.0, 1.0]. Out-of-range values return an
// error rather than being silently clamped — silent clamping would mask
// operator typos like "0.85" written as "85".
func ParseGateByType(raw string) (map[string]float32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var parsed map[string]float64
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("invalid completeness-by-type JSON: %w", err)
	}
	out := make(map[string]float32, len(parsed))
	for k, v := range parsed {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" {
			return nil, fmt.Errorf("completeness-by-type contains empty decision_type key")
		}
		if v < 0 || v > 1 {
			return nil, fmt.Errorf("completeness-by-type[%q]=%.4f out of range [0.0, 1.0]", k, v)
		}
		out[key] = float32(v)
	}
	return out, nil
}
