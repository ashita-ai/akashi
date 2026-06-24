package conflicts

import (
	"regexp"
	"strings"

	"github.com/ashita-ai/akashi/internal/model"
)

// ticketRefPattern matches ticket references like ARD-958, ENG-42, RFC-7234.
// The 3-letter prefix floor avoids accidental matches on HTTP-2, IPv4-4, and
// other version-style strings; this still admits the conventional 3+ letter
// project keys used by Linear, Jira, GitHub, and most issue trackers.
var ticketRefPattern = regexp.MustCompile(`(?i)\b([A-Z]{3,10})-(\d+)\b`)

// extractTicketRef returns the canonical ticket reference for a decision, or
// "" if none can be inferred. Checks three sources in priority order:
//
//  1. agent_context.task   — agent-supplied free text (most explicit)
//  2. agent_context.git_branch — e.g. "evanvolgas/ard-958-snapshot-errors"
//  3. outcome text         — e.g. "ARD-958 S2 implemented..."
//
// Returns the canonical uppercase form ("ARD-958"). When multiple references
// appear in the same source, returns the first match — agents conventionally
// place the primary ticket first.
//
// Used by isSameAgentSameTicketRefinement and isCrossAgentPrecedentRefinement
// to suppress false-positive contradictions when an agent refines a prior
// decision on the same ticket — either implicitly (same-agent, no
// supersedes_id link) or explicitly (cross-agent, precedent_ref set).
func extractTicketRef(d model.Decision) string {
	refs := extractTicketRefs(d)
	if len(refs) == 0 {
		return ""
	}
	return refs[0]
}

// extractTicketRefs returns every canonical ticket reference visible in a
// decision, deduplicated and ordered by source priority then occurrence.
// Sources are walked in the same priority order as extractTicketRef
// (task → branch → outcome); within a single source, references are returned
// in the order they appear.
//
// Used by structural pre-filters that need to compare *sets* of tickets
// across two decisions — e.g. the disjoint-ticket filter and the PR-series
// layer-marker filter both need to recognise that a single outcome can
// legitimately mention several tickets and we should not throw away the
// non-primary ones.
//
// Returns nil when nothing is extractable from any source.
func extractTicketRefs(d model.Decision) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(s string) {
		for _, m := range allTicketRefs(s) {
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	add(nestedContextString(d.AgentContext, "task"))
	add(nestedContextString(d.AgentContext, "git_branch"))
	add(d.Outcome)
	return out
}

// allTicketRefs returns every canonical ticket reference found in s, in the
// order they appear. Duplicates within a single string are preserved here —
// callers that want set semantics should dedupe across sources.
func allTicketRefs(s string) []string {
	if s == "" {
		return nil
	}
	matches := ticketRefPattern.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.ToUpper(m[1])+"-"+m[2])
	}
	return out
}

// matchTicketRef applies ticketRefPattern to s and returns the canonical
// uppercase ticket reference (e.g. "ARD-958"), or "" if none found.
// Returns only the first match; callers that need every match should use
// allTicketRefs.
func matchTicketRef(s string) string {
	if s == "" {
		return ""
	}
	m := ticketRefPattern.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1]) + "-" + m[2]
}

// prRefPattern matches pull-request / issue references embedded in free text,
// e.g. "#1539", "PR #1539", "(#1543)". The leading "#" is required so the
// pattern does not swallow unrelated numbers that saturate decision outcomes
// (migration timestamps "20260623000001", counts like "573 unit tests", LSNs).
// The 2-digit floor skips single-digit enumerations ("(1)", "finding #3") and
// the trailing \b prevents partial matches inside alphanumeric runs (hex
// colors "#1a2b", ids). Numbers are canonicalized to "PR-<n>" so PR refs share
// the disjoint-set namespace with ticket refs ("ARD-958") without colliding.
var prRefPattern = regexp.MustCompile(`#(\d{2,7})\b`)

// prNumberDigits extracts the numeric run from the structured
// agent_context.pr_number field, which agents may send as "1539", "#1539", or
// "PR-1539" — none of which the "#"-anchored prRefPattern reliably matches.
// The structured field is already known to be a PR/issue identifier, so a bare
// digit run is a safe signal there (unlike free text).
var prNumberDigits = regexp.MustCompile(`\d{2,7}`)

// canonicalPRRef normalizes a structured pr_number value to the canonical
// "PR-<digits>" form, or "" when it carries no digit run of the expected length.
func canonicalPRRef(s string) string {
	digits := prNumberDigits.FindString(s)
	if digits == "" {
		return ""
	}
	return "PR-" + digits
}

// extractWorkItemRefs returns every work-item identifier visible in a decision:
// ticket references (canonical "ARD-958") AND pull-request / issue references
// (canonical "PR-1539"). PR refs are sourced first from the structured
// agent_context.pr_number (most reliable), then parsed from the task and outcome
// free text as a fallback — the live trail carries them almost entirely in
// outcome prose ("recommended not merging PR #1539 ...").
//
// Used only by isDisjointWorkItem (cloud scorer), which treats two
// work-item-scoped decisions about entirely different work items as
// non-contradictory. Deliberately separate from extractTicketRefs so the
// ticket-only refinement filters — which join on an *identical* ticket key —
// are unaffected by PR-number parsing. A missed reference only costs a
// suppression (the safe, under-suppressing direction), never a false one.
//
// Returns nil when nothing is extractable from any source.
func extractWorkItemRefs(d model.Decision) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, t := range extractTicketRefs(d) {
		add(t)
	}
	add(canonicalPRRef(nestedContextString(d.AgentContext, "pr_number")))
	for _, src := range []string{
		nestedContextString(d.AgentContext, "task"),
		d.Outcome,
	} {
		for _, m := range prRefPattern.FindAllStringSubmatch(src, -1) {
			add("PR-" + m[1])
		}
	}
	return out
}

// resourceRefPattern matches the structured infrastructure-resource identifiers
// that dominate live incident-recovery traces — "connector_57a42328",
// "org_54b2e846". These are regular tokens (a fixed prefix + a hex run), not
// the free-text customer names ("Trellis", "Akkari") that an earlier note in
// isOperationalStateProgression deemed too fragile to parse: the prefix anchors
// the match and the 6+ hex-char floor rejects method names ("connector_reset" —
// "reset" is not a hex run) and short fields ("org_id"). Hex is canonicalized to
// lowercase so the two casings collide in the disjoint-set namespace.
var resourceRefPattern = regexp.MustCompile(`(?i)\b(connector|org)_([0-9a-f]{6,})\b`)

// extractResourceRefs returns every infrastructure-resource identifier visible
// in a decision: connector references (canonical "CONNECTOR-57a42328") and org
// references (canonical "ORG-54b2e846"), parsed from the task and outcome free
// text where live operational traces name them.
//
// Used only by isDisjointResource (cloud scorer), which treats two
// operational/investigation decisions about entirely different resources as
// non-contradictory. Kept separate from extractWorkItemRefs because the two
// signals key different filters and must not cross-contaminate: a PR number and
// a connector id are different identity namespaces. A missed reference only
// costs a suppression (the safe, under-suppressing direction), never a false
// one. Customer names are deliberately NOT parsed — an unbounded, drifting
// free-text vocabulary is exactly the fragile signal to avoid.
//
// Returns nil when nothing is extractable from any source.
func extractResourceRefs(d model.Decision) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(prefix, hex string) {
		ref := strings.ToUpper(prefix) + "-" + strings.ToLower(hex)
		if _, dup := seen[ref]; dup {
			return
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	for _, src := range []string{
		nestedContextString(d.AgentContext, "task"),
		d.Outcome,
	} {
		for _, m := range resourceRefPattern.FindAllStringSubmatch(src, -1) {
			add(m[1], m[2])
		}
	}
	return out
}
