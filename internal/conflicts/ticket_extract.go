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
	if ref := matchTicketRef(nestedContextString(d.AgentContext, "task")); ref != "" {
		return ref
	}
	if ref := matchTicketRef(nestedContextString(d.AgentContext, "git_branch")); ref != "" {
		return ref
	}
	return matchTicketRef(d.Outcome)
}

// matchTicketRef applies ticketRefPattern to s and returns the canonical
// uppercase ticket reference (e.g. "ARD-958"), or "" if none found.
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
