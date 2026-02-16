package conflicts

import (
	"strings"
	"unicode"
)

// SplitClaims breaks an outcome text into individual claims suitable for
// sentence-level embedding. It splits on sentence boundaries, numbered items,
// markdown lists, colon-introduced lists, and semicolons, then filters out
// claims that are too short, boilerplate, or near-duplicates.
//
// The minimum useful claim length is 20 characters — shorter fragments
// (e.g. "Ratings:", "Three findings:") lack enough semantic content for
// meaningful embedding comparison.
func SplitClaims(outcome string) []string {
	if len(outcome) == 0 {
		return nil
	}

	// First pass: split on markdown list items (-, *, or numbered) at line boundaries.
	raw := splitMarkdownLists(outcome)

	// Second pass: split colon-introduced lists ("Preamble: item1, item2").
	var colonExpanded []string
	for _, s := range raw {
		colonExpanded = append(colonExpanded, splitColonLists(s)...)
	}

	// Third pass: split on sentence-ending punctuation followed by whitespace.
	var sentences []string
	for _, s := range colonExpanded {
		sentences = append(sentences, splitSentences(s)...)
	}

	// Fourth pass: split sentences that contain numbered lists like "(1) ... (2) ...".
	var numbered []string
	for _, s := range sentences {
		numbered = append(numbered, splitNumberedItems(s)...)
	}

	// Fifth pass: split on semicolons when the resulting parts are substantial.
	var expanded []string
	for _, s := range numbered {
		expanded = append(expanded, splitSemicolons(s)...)
	}

	// Filter: drop fragments too short or boilerplate.
	var claims []string
	for _, s := range expanded {
		s = strings.TrimSpace(s)
		if len(s) >= 20 && !isBoilerplate(s) {
			claims = append(claims, s)
		}
	}

	// Deduplicate: remove claims that are substrings of other claims.
	return deduplicateClaims(claims)
}

// splitSentences splits text on sentence boundaries (. ! ?) followed by
// whitespace or end-of-string. Preserves abbreviation-like patterns by
// requiring the character after the period to be uppercase or a digit.
func splitSentences(text string) []string {
	var sentences []string
	runes := []rune(text)
	start := 0

	for i := 0; i < len(runes); i++ {
		if runes[i] != '.' && runes[i] != '!' && runes[i] != '?' {
			continue
		}
		// Look ahead: is the next non-space character uppercase or a digit?
		// If so, this is likely a sentence boundary.
		j := i + 1
		for j < len(runes) && runes[j] == ' ' {
			j++
		}
		if j >= len(runes) {
			// End of text — this is a sentence boundary.
			s := strings.TrimSpace(string(runes[start : i+1]))
			if s != "" {
				sentences = append(sentences, s)
			}
			start = j
			continue
		}
		if j == i+1 {
			// No space after punctuation — not a sentence boundary (e.g., "6/10.5").
			continue
		}
		next := runes[j]
		if unicode.IsUpper(next) || unicode.IsDigit(next) || next == '(' || next == '"' || next == '\'' {
			s := strings.TrimSpace(string(runes[start : i+1]))
			if s != "" {
				sentences = append(sentences, s)
			}
			start = j
		}
	}
	// Remainder.
	if start < len(runes) {
		s := strings.TrimSpace(string(runes[start:]))
		if s != "" {
			sentences = append(sentences, s)
		}
	}
	return sentences
}

// splitNumberedItems splits a sentence containing "(1) ... (2) ..." patterns
// into individual items. Returns the original string if no pattern is found.
func splitNumberedItems(s string) []string {
	// Look for patterns like "(1)", "(2)", etc.
	var parts []string
	var current strings.Builder
	runes := []rune(s)

	for i := 0; i < len(runes); i++ {
		if runes[i] == '(' && i+2 < len(runes) && unicode.IsDigit(runes[i+1]) {
			// Check if it's a numbered item: (N) where N is one or more digits.
			j := i + 1
			for j < len(runes) && unicode.IsDigit(runes[j]) {
				j++
			}
			if j < len(runes) && runes[j] == ')' {
				// Found a numbered item boundary.
				before := strings.TrimSpace(current.String())
				if before != "" {
					parts = append(parts, before)
				}
				current.Reset()
				// Include the numbered prefix in the new part.
				current.WriteString(string(runes[i : j+1]))
				i = j
				continue
			}
		}
		current.WriteRune(runes[i])
	}
	remainder := strings.TrimSpace(current.String())
	if remainder != "" {
		parts = append(parts, remainder)
	}
	if len(parts) <= 1 {
		return []string{s}
	}
	return parts
}

// isListItem returns true if the trimmed line starts with a markdown list
// marker: "- ", "* ", or a numbered prefix like "1. ", "2. ".
func isListItem(trimmed string) bool {
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		return len(trimmed) > 2
	}
	// Check for "N. " where N is one or more digits.
	i := 0
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i > 0 && i < len(trimmed)-1 && trimmed[i] == '.' && trimmed[i+1] == ' ' {
		return len(trimmed) > i+2
	}
	return false
}

// stripListMarker removes the leading list marker from a trimmed line.
// Handles "- ", "* ", and "N. " prefixes.
func stripListMarker(trimmed string) string {
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		return strings.TrimSpace(trimmed[2:])
	}
	// Strip "N. " prefix.
	i := 0
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i > 0 && i < len(trimmed)-1 && trimmed[i] == '.' && trimmed[i+1] == ' ' {
		return strings.TrimSpace(trimmed[i+2:])
	}
	return trimmed
}

// splitMarkdownLists splits text on markdown-style list items (-, *, or
// numbered "1. ", "2. ") that appear at the start of a line. Non-list text
// before the first item is preserved as a separate element.
func splitMarkdownLists(text string) []string {
	lines := strings.Split(text, "\n")
	hasList := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isListItem(trimmed) {
			hasList = true
			break
		}
	}
	if !hasList {
		return []string{text}
	}

	var parts []string
	var current strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isListItem(trimmed) {
			// Flush any accumulated non-list text.
			if before := strings.TrimSpace(current.String()); before != "" {
				parts = append(parts, before)
			}
			current.Reset()
			parts = append(parts, stripListMarker(trimmed))
		} else {
			if current.Len() > 0 {
				current.WriteRune(' ')
			}
			current.WriteString(trimmed)
		}
	}
	if remainder := strings.TrimSpace(current.String()); remainder != "" {
		parts = append(parts, remainder)
	}
	return parts
}

// splitColonLists splits text like "Preamble: item1, item2, item3" when
// the items after the colon are each at least 20 characters. This catches
// a common pattern in code review outcomes where findings are comma-separated
// after a descriptive prefix.
func splitColonLists(text string) []string {
	idx := strings.Index(text, ": ")
	if idx < 0 || idx > len(text)-3 {
		return []string{text}
	}
	after := text[idx+2:]
	parts := strings.Split(after, ", ")
	if len(parts) < 2 {
		return []string{text}
	}
	// Only split if all parts are substantial.
	for _, p := range parts {
		if len(strings.TrimSpace(p)) < 20 {
			return []string{text}
		}
	}
	var result []string
	// First item includes the preamble for context.
	result = append(result, text[:idx+2]+strings.TrimSpace(parts[0]))
	for _, p := range parts[1:] {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// deduplicateClaims removes claims that are substrings of other claims in
// the same set (case-insensitive). This eliminates redundant fragments that
// would produce false positive conflict pairs.
func deduplicateClaims(claims []string) []string {
	if len(claims) <= 1 {
		return claims
	}
	// Build lowercase versions once.
	lower := make([]string, len(claims))
	for i, c := range claims {
		lower[i] = strings.ToLower(c)
	}

	keep := make([]bool, len(claims))
	for i := range keep {
		keep[i] = true
	}

	for i := 0; i < len(claims); i++ {
		if !keep[i] {
			continue
		}
		for j := 0; j < len(claims); j++ {
			if i == j || !keep[j] {
				continue
			}
			// If claim[j] is a substring of claim[i] and shorter, drop j.
			if len(lower[j]) < len(lower[i]) && strings.Contains(lower[i], lower[j]) {
				keep[j] = false
			}
		}
	}

	var result []string
	for i, c := range claims {
		if keep[i] {
			result = append(result, c)
		}
	}
	return result
}

// boilerplatePrefixes are common low-signal phrases that appear in code review
// outcomes but carry no semantic content for conflict detection.
var boilerplatePrefixes = []string{
	"all tests pass",
	"no issues found",
	"lgtm",
	"looks good to me",
	"ci passes",
	"build succeeds",
	"no regressions",
	"approved",
	"ship it",
	"no changes needed",
	"tests are green",
	"all checks pass",
}

// isBoilerplate returns true if the claim is a known low-signal phrase
// that should be excluded from conflict analysis.
func isBoilerplate(claim string) bool {
	lower := strings.ToLower(strings.TrimRight(claim, ".!? "))
	for _, prefix := range boilerplatePrefixes {
		if lower == prefix || strings.HasPrefix(lower, prefix+" ") {
			return true
		}
	}
	return false
}

// splitSemicolons splits a text on semicolons when both resulting parts
// are at least 20 characters long. This handles enumerated lists like
// "issue A; issue B; issue C" without splitting on incidental semicolons
// in short text.
func splitSemicolons(text string) []string {
	if !strings.Contains(text, ";") {
		return []string{text}
	}
	raw := strings.Split(text, ";")
	// Only split if all parts are substantial (>= 20 chars after trimming).
	allSubstantial := true
	for _, part := range raw {
		if len(strings.TrimSpace(part)) < 20 {
			allSubstantial = false
			break
		}
	}
	if !allSubstantial {
		return []string{text}
	}
	var parts []string
	for _, part := range raw {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}
