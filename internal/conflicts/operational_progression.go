//go:build !lite

package conflicts

import (
	"strings"
	"time"

	"github.com/ashita-ai/akashi/internal/model"
)

// operationalProgressionWindow is the minimum time delta between two
// operational decisions on the same project below which the pair is never
// treated as sequential progression — near-simultaneous competing directives
// are the genuine operational-clash shape and must stay detectable. Beyond the
// window, a large gap is a *weak* signal that the system state has moved on and
// the earlier action no longer describes what the later one is changing.
//
// The signal is only weak, and deliberately not relied on alone: a single
// resource can stay in one incident longer than the window (the live Acme
// connector_redacted01 incident ran 2026-05-26 → 06-18), in which case directives
// spanning >7 days are still about the same live system, not unrelated steps.
// Time-apart cannot distinguish that case from genuine progression, so
// isOperationalStateProgression layers other guards on top: the data-loss guard
// exempts high-stakes pairs outright regardless of gap, and the
// precedent/session/supersession guards exempt declared lineage. Matches
// temporalReassessmentWindow (7 days = the SessionStart "recent" window): the
// same "state moved on" rationale applied to operational state instead of
// measured metrics.
const operationalProgressionWindow = 7 * 24 * time.Hour

// isOperationalStateProgression returns true when two decisions are both
// operational-execution types on the same project, separated by at least
// operationalProgressionWindow, with neither framed as a reversal of a prior
// approach and no explicit precedent link between them.
//
// Motivation: the dominant live false-positive class (2026-06 open-conflict
// audit) was cross-ticket operational steps on one hot subsystem
// (pgstream / Debezium / Acme CDC) clustered by shared vocabulary —
// e.g. "rolled kafka2pg back to its pre-rollout digest" eight days after
// "verified the connector ONLINE". Embeddings place them close (topic_sim
// 0.70-0.85) and the LLM validator returns CONTRADICTION, but a rollback or
// pause executed a week after a prior deploy is the next step in an incident
// timeline, not a disagreement with it.
//
// Operational sibling of isTemporalReassessment: that filter covers
// review-type re-measurements whose numbers drift over time; this one covers
// operational state that mutates over time. Both rest on the same principle —
// an action taken long ago does not contradict a later one just because they
// touch the same system.
//
// Deliberately narrow, to keep genuine conflicts detectable:
//   - both decisions must be operationalTypes. architecture / trade_off /
//     design forks (e.g. the real Debezium-vs-pgstream disagreement) are
//     never eligible, so they still reach the validator. This is why
//     trade_off and bug_fix are excluded even though some operational FPs
//     carry those types — widening would risk suppressing real design forks.
//   - same non-empty project (untagged-vs-untagged pairs are not suppressed)
//   - >= operationalProgressionWindow apart, so near-simultaneous competing
//     directives (the genuine operational-clash shape) stay detectable
//   - neither outcome contains a supersession keyword — an action that
//     explicitly reverts/abandons a prior approach is a declared reversal and
//     reaches the LLM (checked on both sides: the safe, under-suppressing
//     direction)
//   - neither side's task or outcome reports a data-loss / corruption /
//     quarantine event — a data-safety event is categorically high-stakes and
//     must reach the validator regardless of the time gap (checked on both
//     sides, both text fields). This is the guard that keeps same-resource
//     incident disagreements like the live Acme DATALOSS pause from
//     being silently dropped pre-LLM. See decisionContainsDataLossKeyword.
//   - not precedent-linked and not the same session — explicit links and
//     intra-session pairs are left to the LLM, mirroring isTemporalReassessment
//
// Known limitation: suppression keys on (type, project, gap, keywords) with no
// resource-identity signal, so a same-resource contradiction that mentions no
// data-safety event can still be suppressed once it is past the window. The
// data-loss guard closes the high-stakes slice of that gap — the only slice
// with verified instances in the trail. The complementary different-resource
// slice (and operational pairs inside the same incident window, which this
// filter's gap requirement skips) is now handled pre-LLM by isDisjointResource,
// which keys on the structured connector_/org_ tokens this filter declined to
// parse — those tokens are regular identifiers, not the fragile free-text
// customer names that motivated the original deferral.
//
// Scope: lives only in the cloud scorer (this file is !lite). The lite scorer
// has none of the structural suppression family; porting it is out of scope.
func isOperationalStateProgression(d, cand model.Decision) bool {
	if !operationalTypes[strings.ToLower(d.DecisionType)] {
		return false
	}
	if !operationalTypes[strings.ToLower(cand.DecisionType)] {
		return false
	}
	projA, projB := derefString(d.Project), derefString(cand.Project)
	if projA != projB || (projA == "" && projB == "") {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	if d.SessionID != nil && cand.SessionID != nil && *d.SessionID == *cand.SessionID {
		return false
	}
	if containsSupersessionKeyword(d.Outcome) || containsSupersessionKeyword(cand.Outcome) {
		return false
	}
	// Data-safety guard: never auto-suppress a pair where either side reports a
	// data-loss / corruption / quarantine event. Such a decision is not a routine
	// sequential step, and a same-system pair that includes one is precisely the
	// disagreement that must reach the validator and the conflict queue rather
	// than be dropped pre-LLM with only a Debug log. Both sides checked, across
	// task and outcome (under-suppressing direction), mirroring the supersession
	// guard above. (The supersession guard stays outcome-only on purpose: a
	// reversal verb in a task title — "migrate connector from kafka to pg" — is
	// routine phrasing, and scanning the task for it would re-admit the very FPs
	// this filter kills. The data-safety guard has no such cost: firing it only
	// ever routes a pair to the validator, never suppresses one.)
	if decisionContainsDataLossKeyword(d) || decisionContainsDataLossKeyword(cand) {
		return false
	}
	delta := d.ValidFrom.Sub(cand.ValidFrom)
	if delta < 0 {
		delta = -delta
	}
	return delta >= operationalProgressionWindow
}
