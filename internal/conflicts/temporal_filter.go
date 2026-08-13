//go:build !lite

package conflicts

import (
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

// isTemporalReassessment returns true when two decisions are both
// review/assessment types on the same project, separated by at least
// temporalReassessmentWindow, with neither citing the other and not in
// the same session. This is the re-measurement pattern: the newer
// assessment updates a prior measurement of the same system and naturally
// has different numbers without contradicting the earlier conclusions.
//
// Pairs caught by this filter would otherwise embed close together (same
// domain) and produce a CONTRADICTION verdict from the LLM whenever quoted
// metrics differ — see issue #705. Linked or same-session re-assessments
// are deliberately excluded so the LLM can still classify intentional
// supersessions.
func isTemporalReassessment(d, cand model.Decision) bool {
	if !reviewTypes[strings.ToLower(d.DecisionType)] {
		return false
	}
	if !reviewTypes[strings.ToLower(cand.DecisionType)] {
		return false
	}
	if derefString(d.Project) != derefString(cand.Project) {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	if d.SessionID != nil && cand.SessionID != nil && *d.SessionID == *cand.SessionID {
		return false
	}
	delta := d.ValidFrom.Sub(cand.ValidFrom)
	if delta < 0 {
		delta = -delta
	}
	return delta >= temporalReassessmentWindow
}
