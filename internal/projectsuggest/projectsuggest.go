// Package projectsuggest produces "did you mean" suggestions for unknown
// project names rejected by the trace endpoints. The matcher is biased
// toward substring containment because the dominant failure mode is agents
// passing a path-style or org-prefixed name (e.g. "ArdentAILabs/mono",
// "ardent-mono") when the canonical project is the basename ("mono").
package projectsuggest

import (
	"sort"
	"strings"
)

// MaxSuggestions caps the number of "did you mean" candidates surfaced in
// a rejection message. Keeps the error focused on the most likely retry.
const MaxSuggestions = 3

// Threshold is the minimum similarity (0..1) required for a candidate to
// be surfaced. 0.5 admits substring-style matches like
// "ardent-mono" → "mono", while filtering unrelated names.
const Threshold = 0.5

// Suggest returns up to MaxSuggestions known project names that resemble
// the submitted string, ordered by descending similarity. Returns nil
// when no candidate clears Threshold or the inputs are empty.
func Suggest(submitted string, known []string) []string {
	submitted = strings.TrimSpace(submitted)
	if submitted == "" || len(known) == 0 {
		return nil
	}

	type scored struct {
		name  string
		score float64
	}

	subLower := strings.ToLower(submitted)
	scoredList := make([]scored, 0, len(known))
	for _, k := range known {
		if k == submitted {
			continue
		}
		score := similarityScore(subLower, strings.ToLower(k))
		if score >= Threshold {
			scoredList = append(scoredList, scored{name: k, score: score})
		}
	}

	sort.Slice(scoredList, func(i, j int) bool {
		if scoredList[i].score != scoredList[j].score {
			return scoredList[i].score > scoredList[j].score
		}
		return scoredList[i].name < scoredList[j].name
	})

	limit := MaxSuggestions
	if len(scoredList) < limit {
		limit = len(scoredList)
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, scoredList[i].name)
	}
	return out
}

// FormatRejectionSuffix builds the human-readable suffix appended to
// "unknown project" rejection errors. Returns "" when there are no known
// projects to enumerate. The result begins with a space so callers can
// concatenate directly onto an existing message.
func FormatRejectionSuffix(submitted string, known []string) string {
	if len(known) == 0 {
		return ""
	}
	suggestions := Suggest(submitted, known)

	var b strings.Builder
	if len(suggestions) > 0 {
		b.WriteString(" Did you mean: ")
		b.WriteString(joinQuoted(suggestions))
		b.WriteString("?")
	}

	preview := known
	const maxPreview = 10
	truncated := false
	if len(preview) > maxPreview {
		preview = preview[:maxPreview]
		truncated = true
	}
	b.WriteString(" Known projects: ")
	b.WriteString(joinQuoted(preview))
	if truncated {
		b.WriteString(", …")
	}
	b.WriteString(".")
	return b.String()
}

// similarityScore returns a 0..1 score for the closeness of a and b. Both
// inputs are expected to already be lowercased. Substring containment scores
// 0.9; otherwise the score is 1 - normalizedLevenshtein(a, b).
func similarityScore(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return 0.9
	}
	dist := levenshtein(a, b)
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 0
	}
	return 1 - float64(dist)/float64(maxLen)
}

// levenshtein returns the edit distance between a and b using a single-row
// dynamic programming table. Inputs are treated as byte slices, which is
// adequate for project names (ASCII identifiers in practice).
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = minInt(
				prev[j]+1,      // deletion
				curr[j-1]+1,    // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func minInt(values ...int) int {
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func joinQuoted(items []string) string {
	if len(items) == 0 {
		return ""
	}
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = `"` + s + `"`
	}
	return strings.Join(parts, ", ")
}
