//go:build !lite

package conflicts

import (
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

// mechanicalKeywords are outcome substrings that indicate a mechanical/routine
// operation (migration renumbering, rebase conflict resolution, version bumps)
// rather than a genuine design or architecture decision.
var mechanicalKeywords = []string{
	"renumber",
	"renumbering",
	"renumbered",
	"rebase",
	"rebasing",
	"rebased",
	"merge conflict",
	"merge conflicts",
	"merged origin/main",
	"merged main",
	"version bump",
	"bumped version",
}

// isMechanicalOperation returns true when the outcome text describes a routine
// mechanical operation like migration renumbering or rebase conflict resolution.
// These operations produce semantically similar outcomes across branches but
// represent parallel correct work, not disagreement.
func isMechanicalOperation(outcome string) bool {
	lower := strings.ToLower(outcome)
	for _, kw := range mechanicalKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// isCrossBranchMechanical returns true when two decisions are on different git
// branches and both describe mechanical operations (e.g. migration renumbering
// during a rebase). These are parallel correct work whose resolution is
// determined by merge order, not a genuine conflict.
//
// Requires both decisions to have branch metadata in agent_context and for the
// branches to differ. Same-branch or missing-branch pairs are not affected.
func isCrossBranchMechanical(d, cand model.Decision) bool {
	branchA := nestedContextString(d.AgentContext, "git_branch")
	branchB := nestedContextString(cand.AgentContext, "git_branch")

	if branchA == "" || branchB == "" || branchA == branchB {
		return false
	}

	return isMechanicalOperation(d.Outcome) && isMechanicalOperation(cand.Outcome)
}

// isSameBranchSelfCorrection returns true when the same agent made two
// sequential decisions on the same branch — a self-correction pattern, not a
// contradiction. The later decision supersedes the earlier one as part of
// iterative work on the same branch.
//
// Requires both decisions to have branch metadata, be from the same agent,
// and be on the same branch. The temporal order doesn't matter — the fact
// that the same agent revised their own decision on the same branch is
// sufficient to classify this as a self-correction.
func isSameBranchSelfCorrection(d, cand model.Decision) bool {
	if d.AgentID != cand.AgentID {
		return false
	}

	branchA := nestedContextString(d.AgentContext, "git_branch")
	branchB := nestedContextString(cand.AgentContext, "git_branch")

	return branchA != "" && branchB != "" && branchA == branchB
}
