//go:build !lite

package conflicts

import (
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

// isDisjointWorkItem returns true when two work-item-scoped decisions (reviews,
// plans) on the same project reference entirely different work items — different
// PRs or different tickets — and therefore cannot be contradicting each other.
// A review of PR #1539 and a review of PR #1543 examine different code; a plan
// for ARD-1606 and a plan for ARD-1662 scope different tickets. They routinely
// share dense subsystem vocabulary (cutover, pgstream, freeze, replica-identity,
// shadow-apply) that places them close in embedding space, and the LLM validator
// then manufactures a CONTRADICTION between them.
//
// This is the hard-rule escalation of the disjoint-ticket signal that
// eef7eb2c shipped as a *validator prompt hint* ("DIFFERENT TICKETS ... classify
// as UNRELATED or COMPLEMENTARY unless ..."). The live 2026-06-23 open-conflict
// audit showed the hint is ignored once shared vocabulary is dense enough: 9 of
// 19 open false positives were disjoint-work-item review/planning pairs the
// prompt failed to deflect. Promoting it to a structural pre-LLM suppressor is
// the membership-parity follow-through, and — unlike the ticket-only hint — this
// one is PR-aware via extractWorkItemRefs, which is what closes the dominant
// fan-out hole: the hub decision in that audit (a review of "PR #1539") carries
// NO ARD-style ticket ref at all, so a ticket-only rule could never have matched
// the nine pairs it spawned.
//
// Deliberately narrow, to keep genuine conflicts detectable:
//   - both decisions must be workItemScopedTypes. architecture / trade_off /
//     design forks are never eligible — a cross-cutting design disagreement
//     (e.g. the real Debezium-vs-pgstream fork) can legitimately span work
//     items and must reach the validator. Same exclusion rationale as
//     isOperationalStateProgression.
//   - same non-empty project (PR/ticket numbers are repo-scoped; an untagged
//     pair, or a cross-repo pair sharing a PR number, is never suppressed —
//     mirrors isCoordinatedChange's same-project requirement for pr_number)
//   - both decisions must expose at least one extractable work-item ref, and
//     the two ref sets must be fully disjoint. Any shared PR/ticket (including
//     one outcome naming the other's work item, e.g. "closed #1543 in favor of
//     #1545") makes the sets overlap and the pair reaches the validator.
//   - not precedent-linked — an explicit lineage cite is left to the LLM
//   - neither side's task or outcome reports a data-loss / corruption /
//     quarantine event — a data-safety finding is categorically high-stakes and
//     must never be dropped pre-LLM (both sides, both text fields — the same
//     task+outcome extractWorkItemRefs mines for the work-item refs that make
//     the pair eligible, so a keyword cannot hide in a field the ref scan reads
//     but the guard does not). This is NOT a redundant copy of the operational
//     filter's guard: cross-ticket data-safety contradictions are a real,
//     resolved class in this trail (e.g. two investigations root-causing the
//     same DATALOSS incident to different causes under different tickets), and
//     disjoint work items do not make that pair safe to drop. Same guard, same
//     reasoning as isOperationalStateProgression; see
//     decisionContainsDataLossKeyword.
//
// Deliberately NOT guarded on supersession keywords, unlike the operational and
// refinement siblings. Those siblings have no work-item identity signal, so a
// "replaced X" / "superseded by Y" phrase is their only cue that a pair might be
// a declared reversal that should reach the validator. This filter HAS that
// signal: a genuine cross-work-item supersession names the other work item, so
// the ref sets overlap and the pair already reaches the validator (then the
// post-LLM supersession→suggestion path). Adding a keyword guard here would only
// re-admit the false positives this filter exists to kill — review prose is full
// of code-diff verbs ("replaced the boolean", "dropped the dead reader") that
// describe a diff, not a decision reversal. A vague reversal that names no work
// item is the sole edge this forgoes; it stays suppressed, which is correct (an
// unnamed antecedent cannot be linked anyway). The difference from the siblings
// is intentional, not drift.
//
// Failure mode is under-suppression: a missed PR/ticket extraction leaves the
// ref sets looking smaller (or one side empty), which disables the filter and
// sends the pair to the validator — never a wrongful suppression.
//
// Scope: lives only in the cloud scorer (this file is !lite), alongside the rest
// of the structural suppression family.
func isDisjointWorkItem(d, cand model.Decision) bool {
	if !workItemScopedTypes[strings.ToLower(d.DecisionType)] {
		return false
	}
	if !workItemScopedTypes[strings.ToLower(cand.DecisionType)] {
		return false
	}
	projA, projB := derefString(d.Project), derefString(cand.Project)
	if projA != projB || (projA == "" && projB == "") {
		return false
	}
	if isPrecedentLinked(d, cand) {
		return false
	}
	if decisionContainsDataLossKeyword(d) || decisionContainsDataLossKeyword(cand) {
		return false
	}
	refsA := extractWorkItemRefs(d)
	refsB := extractWorkItemRefs(cand)
	if len(refsA) == 0 || len(refsB) == 0 {
		return false
	}
	return !ticketRefsOverlap(refsA, refsB)
}
