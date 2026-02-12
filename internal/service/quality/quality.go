// Package quality provides decision trace quality scoring.
// Quality scores (0.0-1.0) measure trace completeness and are used
// to rank precedent lookup results.
package quality

import (
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

// StandardDecisionTypes are the canonical types from prompt templates.
// Using standard types improves discoverability and consistency.
var StandardDecisionTypes = map[string]bool{
	"model_selection": true,
	"architecture":    true,
	"data_source":     true,
	"error_handling":  true,
	"feature_scope":   true,
	"trade_off":       true,
	"deployment":      true,
	"security":        true,
}

// Score computes a quality score (0.0-1.0) for a trace decision.
// Higher scores indicate more complete, useful traces.
//
// Scoring factors:
//   - Confidence present and reasonable (0.05-0.95): 0.15
//   - Reasoning substantive (>50 chars): up to 0.25
//   - Alternatives provided (>=2): up to 0.20
//   - Rejection reason on any alternative: 0.10
//   - Evidence provided: up to 0.15
//   - Standard decision type: 0.10
//   - Outcome substantive (>20 chars): 0.05
func Score(d model.TraceDecision) float32 {
	var score float32

	// Factor 1: Confidence is present and reasonable.
	// Extreme values (exactly 0 or 1) are often defaults, so we reward mid-range.
	// Strict inequality: exactly 0.05 and 0.95 fall to edge tier (0.10).
	if d.Confidence > 0.05 && d.Confidence < 0.95 {
		score += 0.15
	} else if d.Confidence > 0 && d.Confidence < 1 {
		score += 0.10
	}

	// Factor 2: Reasoning is substantive.
	if d.Reasoning != nil {
		reasoningLen := len(strings.TrimSpace(*d.Reasoning))
		switch {
		case reasoningLen > 100:
			score += 0.25
		case reasoningLen > 50:
			score += 0.20
		case reasoningLen > 20:
			score += 0.10
		}
	}

	// Factor 3: Alternatives provided.
	switch {
	case len(d.Alternatives) >= 3:
		score += 0.20
	case len(d.Alternatives) >= 2:
		score += 0.15
	case len(d.Alternatives) >= 1:
		score += 0.05
	}

	// Factor 4: At least one alternative has a rejection reason.
	for _, alt := range d.Alternatives {
		if alt.RejectionReason != nil && len(strings.TrimSpace(*alt.RejectionReason)) > 10 {
			score += 0.10
			break
		}
	}

	// Factor 5: Evidence provided.
	if len(d.Evidence) >= 2 {
		score += 0.15
	} else if len(d.Evidence) >= 1 {
		score += 0.10
	}

	// Factor 6: Decision type is from standard taxonomy.
	if StandardDecisionTypes[d.DecisionType] {
		score += 0.10
	}

	// Factor 7: Outcome is substantive.
	if len(strings.TrimSpace(d.Outcome)) > 20 {
		score += 0.05
	}

	return score
}
