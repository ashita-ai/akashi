//go:build !lite

package conflicts

import (
	"strings"
	"time"

	"github.com/ashita-ai/akashi/internal/model"
)

// isComplementaryWorkflowPair returns true if the decision pair matches a
// structural pattern where two decisions are about the same topic but are
// complementary rather than contradictory. These patterns are invisible to
// embedding-based scoring because topic similarity is high and outcome text
// diverges (review language vs implementation language).
//
// Three heuristics, any of which is sufficient:
//
//  1. Temporal workflow: one decision is a review/assessment/investigation type
//     and the other is an implementation/fix type, with the implementation
//     recorded after the review. This covers review→fix and assessment→implementation.
//
//  2. Same-agent refinement: both decisions are from the same agent and the
//     newer one's outcome contains keywords indicating it built on the older
//     one ("implemented", "fixed", "resolved", "completed", "addressed").
//
//  3. Precedent chain: one decision cites the other via precedent_ref,
//     meaning the agent explicitly linked them. However, precedent links can
//     represent supersession (reversal) rather than refinement. When the later
//     decision's outcome contains supersession keywords, the pair is passed
//     through to LLM validation instead of being suppressed. See issue #452.
func isComplementaryWorkflowPair(d, cand model.Decision) bool {
	// Heuristic 3: Precedent chain. If either decision cites the other,
	// they are linked by design. But linked decisions can still conflict
	// (supersession). Check the later decision's outcome for reversal signals;
	// if found, let the LLM decide instead of suppressing.
	if isPrecedentLinked(d, cand) {
		later := cand
		if cand.ValidFrom.Before(d.ValidFrom) {
			later = d
		}
		if containsSupersessionKeyword(later.Outcome) {
			return false // probable supersession — let LLM validate
		}
		return true // probable refinement — suppress
	}

	// Determine temporal order: earlier and later decision.
	earlier, later := d, cand
	if cand.ValidFrom.Before(d.ValidFrom) {
		earlier, later = cand, d
	}

	// Heuristic 1: Temporal workflow — review/assessment type followed by
	// implementation/fix type.
	if isDirectionalWorkflowPair(earlier.DecisionType, later.DecisionType) {
		return true
	}

	// Heuristic 2: Same-agent refinement with outcome keywords.
	if d.AgentID == cand.AgentID {
		lowerOutcome := strings.ToLower(later.Outcome)
		for _, kw := range refinementKeywords {
			if strings.Contains(lowerOutcome, kw) {
				return true
			}
		}
	}

	return false
}

// isCoordinatedChange returns true when two decisions share provenance metadata
// that proves they are part of the same coordinated change — same commit, same
// PR, or same branch within a short temporal window. This is a binary signal
// (no thresholds) that is semantically correct: decisions from the same PR are
// coordinated by definition, even when they implement different layers (model,
// handler, storage, UI, docs) whose outcome text diverges.
//
// Checked fields (via nestedContextString, which searches client.*, server.*,
// and flat namespaces):
//   - commit_sha: exact match → always coordinated (globally unique)
//   - pr_number: exact match + same project → always coordinated
//   - branch: exact match + same project + decisions within 24h → likely coordinated
//
// PR numbers and branch names are scoped to a repository, not globally unique.
// Two decisions in the same org but different repos sharing pr_number "42" are
// not coordinated. commit_sha is exempt because SHA-256 hashes are globally unique.
//
// Branch alone is a weaker signal (branches are reused across PRs), so it
// requires temporal proximity as a secondary qualifier.
func isCoordinatedChange(d, cand model.Decision) bool {
	// Same commit SHA → definitively coordinated.
	// Commit SHAs are globally unique, no project scoping needed.
	commitA := nestedContextString(d.AgentContext, "commit_sha")
	commitB := nestedContextString(cand.AgentContext, "commit_sha")
	if commitA != "" && commitA == commitB {
		return true
	}

	// For pr_number and branch checks, require same project. PR numbers and
	// branch names are repo-scoped, not globally unique — pr_number "42" in
	// repo-alpha is unrelated to pr_number "42" in repo-beta. When both
	// projects are nil/empty we can't distinguish repos, so we require at
	// least one non-empty project to avoid false suppression in multi-repo orgs.
	projA := derefString(d.Project)
	projB := derefString(cand.Project)
	sameProject := projA == projB && (projA != "" || projB != "")

	// Same PR number + same project → definitively coordinated.
	prA := nestedContextString(d.AgentContext, "pr_number")
	prB := nestedContextString(cand.AgentContext, "pr_number")
	if prA != "" && prA == prB && sameProject {
		return true
	}

	// Same branch + same project + temporal proximity → likely coordinated.
	// 24h window accommodates multi-session work on a single branch
	// while excluding branch reuse across separate PRs (which typically
	// spans days/weeks with a merge in between).
	branchA := nestedContextString(d.AgentContext, "branch")
	branchB := nestedContextString(cand.AgentContext, "branch")
	if branchA != "" && branchA == branchB && sameProject {
		const coordinationWindow = 24 * time.Hour
		timeDelta := d.ValidFrom.Sub(cand.ValidFrom).Abs()
		if timeDelta <= coordinationWindow {
			return true
		}
	}

	return false
}

// isSameAgentSameTicketRefinement returns true when the same agent traces
// what looks like a clean refinement of their own prior decision on the same
// ticket without setting supersedes_id. The newer decision likely intends
// to supersede the earlier one but the agent forgot to set the link.
//
// Sibling of isSameBranchSelfCorrection: that filter requires identical
// git_branch metadata. This one is broader — refinements can span branches
// (e.g. layer-2 PR followed by layer-3 PR on the same ticket) so the join key
// is an extracted ticket reference (see extractTicketRef in ticket_extract.go).
//
// Excluded:
//   - precedent-linked pairs (the agent already linked them — let LLM judge)
//   - outcomes containing supersession keywords (probable explicit reversal,
//     not a forgotten link — let LLM judge)
//   - different agents
//   - missing or differing ticket references
//
// Tracks issue #709 (PR-1 of #708). PR-2 (#710) will surface a supersedes_id
// suggestion to the agent via akashi_check.
func isSameAgentSameTicketRefinement(d, cand model.Decision) bool {
	if d.AgentID != cand.AgentID {
		return false
	}
	if isPrecedentLinked(d, cand) {
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

// isPrecedentLinked returns true if either decision cites the other via
// precedent_ref, meaning there is an explicit lineage link between them.
func isPrecedentLinked(d, cand model.Decision) bool {
	if cand.PrecedentRef != nil && *cand.PrecedentRef == d.ID {
		return true
	}
	if d.PrecedentRef != nil && *d.PrecedentRef == cand.ID {
		return true
	}
	return false
}

// isDirectionalWorkflowPair returns true if earlierType is a review/assessment
// type and laterType is an implementation/fix type. Unlike isWorkflowPair in
// validator.go (which checks both directions for LLM prompt hints), this check
// is directional: the review must come first temporally.
func isDirectionalWorkflowPair(earlierType, laterType string) bool {
	return reviewTypes[strings.ToLower(earlierType)] && implementationTypes[strings.ToLower(laterType)]
}
