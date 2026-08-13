//go:build !lite

package conflicts

import "github.com/ashita-ai/akashi/internal/model"

// isCrossAgentPrecedentRefinement returns true when two different agents
// trace decisions on the same ticket, one explicitly cites the other via
// precedent_ref, and the later outcome contains no supersession keywords.
//
// Cross-agent analogue of isSameAgentSameTicketRefinement: the same-agent
// filter targets forgotten-supersedes within one agent's work; this filter
// targets the case where agent B explicitly declares "I am building on A"
// (by setting precedent_ref to A on the same ticket) yet the LLM validator
// still classifies the pair as contradiction because the two outcomes
// mention competing implementation details.
//
// Conservative by construction. Required signals:
//   - different agents
//   - precedent_ref explicit between the two decisions (either direction)
//   - identical extracted ticket reference on both sides
//   - same project (defense against pathological cross-project precedent_refs)
//   - later outcome free of supersession keywords (so declared reversals
//     and partial supersessions still reach the validator)
//
// Does NOT catch (deliberately):
//   - same-ticket cross-agent pairs without precedent_ref — those may be
//     genuine disagreements (e.g. ARD-1168 broad vs narrow shared-env
//     fallback, which resolved with codex winning)
//   - pairs sharing only a common-ancestor precedent_ref but not citing
//     each other — shared ancestry is too weak a signal to suppress, in
//     line with the 8f5124c8 reversal of bare same-branch suppression
//
// See sibling isSameAgentSameTicketRefinement for the same-agent variant.
func isCrossAgentPrecedentRefinement(d, cand model.Decision) bool {
	if d.AgentID == cand.AgentID {
		return false
	}
	if !isPrecedentLinked(d, cand) {
		return false
	}
	if derefString(d.Project) != derefString(cand.Project) {
		return false
	}
	later := d
	if cand.ValidFrom.After(d.ValidFrom) {
		later = cand
	}
	if containsSupersessionKeyword(later.Outcome) {
		return false
	}
	refA := extractTicketRef(d)
	if refA == "" {
		return false
	}
	return refA == extractTicketRef(cand)
}
