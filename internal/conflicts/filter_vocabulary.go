//go:build !lite

package conflicts

import (
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

// supersessionKeywords are outcome substrings that indicate the decision
// reverses or replaces a prior decision rather than refining it.
//
// The "walk-back" group (shelved, dropped, pivoting, …) was added after the
// #717 FP audit observed agents stating their own reversal in plain English
// — "Shelved the draft; pivoting to a different topic" — without the
// existing same-agent same-ticket filter recognising the reversal because
// the keyword list missed the verb.
var supersessionKeywords = []string{
	"switched",
	"superseding",
	"superseded",
	"replaced",
	"replacing",
	"reversed",
	"reversing",
	"reverted",
	"reverting",
	"migrated from",
	"migrating from",
	"instead of",
	"no longer",
	"abandoned",
	"abandoning",
	// Walk-back / pivot vocabulary surfaced by issue #717 FP audit.
	"shelved",
	"shelving",
	"dropped",
	"dropping",
	"pivoting",
	"pivoted",
	"walked back",
	"walking back",
	"rewrote",
	"rewriting",
	"rewritten",
	"scrapped",
	"scrapping",
	"discarded",
	"discarding",
	"retracted",
	"retracting",
	"withdrew",
	"withdrawing",
	"tabled",
	"parked",
	"deprecated this",
	"deprecating",
}

// containsSupersessionKeyword returns true if the outcome text contains any
// keyword indicating the decision reverses or replaces a prior one.
func containsSupersessionKeyword(outcome string) bool {
	lower := strings.ToLower(outcome)
	for _, kw := range supersessionKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// dataLossKeywords are outcome substrings that mark an operational decision as
// describing a data-safety event (loss, corruption, or quarantined/skipped
// writes) rather than a routine state change. isOperationalStateProgression
// refuses to suppress any pair where either side carries one: a data-safety
// event is categorically high-stakes, and silently dropping a same-system pair
// that reports one — before the LLM validator or a human ever sees it — would
// trade the system's core guarantee (a complete paper trail of disagreements)
// for noise reduction. Surfaced by the 2026-06 Acme connector_redacted01
// incident, where "paused … active target-side DATALOSS quarantine growth" sat
// >7 days from "scaled … resume loses no data" on the same writer.
//
// Deliberately broad and matched on both sides (the under-suppressing
// direction): even a reassuring "verified no data loss" disables suppression
// for the pair, which is correct — if data safety is in question at all, the
// pair belongs in front of the validator.
var dataLossKeywords = []string{
	"dataloss",
	"data loss",
	"data-loss",
	"lost data",
	"data was lost",
	"quarantine", // pgstream parks unappliable rows in a quarantine table
	"corrupt",    // corrupted / corruption
}

// containsDataLossKeyword returns true if the given text references a
// data-safety event (loss, corruption, or quarantined writes).
func containsDataLossKeyword(text string) bool {
	lower := strings.ToLower(text)
	for _, kw := range dataLossKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// decisionContainsDataLossKeyword reports whether a decision references a
// data-safety event in any free-text field that a pre-LLM suppressor reads to
// judge eligibility — the agent-supplied agent_context.task and the outcome.
//
// The pre-LLM suppressors mine BOTH fields for their identity signal:
// extractResourceRefs and extractWorkItemRefs both scan task and outcome, so a
// pair can become eligible for suppression on the strength of a ref that appears
// only in the task. The data-safety guard must therefore scan exactly those same
// sources — otherwise a "DATALOSS recovery connector_x" task whose outcome omits
// the word is silently dropped pre-LLM while its connector ref still drives the
// disjoint-resource match. Guarding only the outcome (the original behavior) left
// that hole. This is the membership-parity fix for the guard: every suppressor
// that mines the task for identity now guards the task for data safety, so the
// guard and the eligibility signal never read different text.
//
// isOperationalStateProgression shares this guard too, though it keys on a time
// gap rather than task-extracted refs: a data-safety event anywhere in a
// decision's salient text is categorically high-stakes and must reach the
// validator, and a single uniform guard is one fewer place for the data-loss
// check to drift between the sibling filters.
func decisionContainsDataLossKeyword(d model.Decision) bool {
	return containsDataLossKeyword(nestedContextString(d.AgentContext, "task")) ||
		containsDataLossKeyword(d.Outcome)
}

// implementationTypes are decision types that represent fix/implementation work.
var implementationTypes = map[string]bool{
	"architecture":   true,
	"bug_fix":        true,
	"fix":            true,
	"implementation": true,
	"refactor":       true,
}

// operationalTypes are decision types that represent execution actions against
// running infrastructure (deploys, rollbacks, scaling, restarts, image
// promotions) rather than design or evaluation decisions. These produce
// time-bound state mutations, which is what isOperationalStateProgression keys
// on. Deliberately excludes architecture/trade_off/design — those carry
// genuine direction-setting forks that must remain detectable.
var operationalTypes = map[string]bool{
	"operations":  true,
	"operational": true,
	"deployment":  true,
}

// workItemScopedTypes are decision types whose conflict scope is a single work
// item — a PR under review, a ticket being planned or investigated. Two such
// decisions about DIFFERENT work items examine different code/scope and cannot
// contradict each other, which is what isDisjointWorkItem keys on. Deliberately
// excludes the direction-setting types (architecture / trade_off / design):
// a cross-cutting design fork can legitimately span work items and must remain
// detectable, the same reason operationalTypes excludes them.
var workItemScopedTypes = map[string]bool{
	"code_review":   true,
	"review":        true,
	"assessment":    true,
	"investigation": true,
	"analysis":      true,
	"audit":         true,
	"planning":      true,
}

// resourceScopedTypes are decision types whose conflict scope is a single
// infrastructure resource — a connector being recovered, an org/customer
// incident being investigated or deployed against. Two such decisions about
// DIFFERENT resources act on different data planes and cannot contradict, which
// is what isDisjointResource keys on (via extractResourceRefs). investigation
// is deliberately a member of BOTH this set and workItemScopedTypes: an
// investigation can be disjoint by ticket OR by connector, and either filter may
// suppress it. Direction-setting types (architecture / trade_off / design) are
// excluded for the same reason as the sibling type sets — a cross-cutting design
// fork can legitimately span resources and must remain detectable.
var resourceScopedTypes = map[string]bool{
	"investigation":        true,
	"deployment":           true,
	"operational_recovery": true,
	"operations":           true,
	"operational":          true,
	"incident":             true,
}

// refinementKeywords are outcome substrings that indicate the decision
// built on a prior decision by the same agent.
var refinementKeywords = []string{
	"implemented",
	"fixed",
	"resolved",
	"completed",
	"addressed",
}
