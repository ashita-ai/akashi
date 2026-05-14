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
// Used by isSameAgentSameTicketRefinement to suppress false-positive
// contradictions when the same agent refines their own prior decision on the
// same ticket without setting supersedes_id.
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
