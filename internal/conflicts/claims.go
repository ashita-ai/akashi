package conflicts

import (
	"strings"
	"unicode"
)

// SplitClaims breaks an outcome text into individual claims suitable for
// sentence-level embedding. It splits on sentence boundaries, numbered items,
// markdown lists, and semicolons, then filters out claims that are too short
// to be meaningful.
//
// The minimum useful claim length is 20 characters — shorter fragments
// (e.g. "Ratings:", "Three findings:") lack enough semantic content for
// meaningful embedding comparison.
func SplitClaims(outcome string) []string {
	if len(outcome) == 0 {
		return nil
	}

	// First pass: split on markdown list items (- or *) at line boundaries.
	raw := splitMarkdownLists(outcome)

	// Second pass: split on sentence-ending punctuation followed by whitespace.
	var sentences []string
	for _, s := range raw {
		sentences = append(sentences, splitSentences(s)...)
	}

	// Third pass: split sentences that contain numbered lists like "(1) ... (2) ...".
	var numbered []string
	for _, s := range sentences {
		numbered = append(numbered, splitNumberedItems(s)...)
	}

	// Fourth pass: split on semicolons when the resulting parts are substantial.
	var expanded []string
	for _, s := range numbered {
		expanded = append(expanded, splitSemicolons(s)...)
	}

	// Filter: drop fragments too short for meaningful embedding.
	var claims []string
	for _, s := range expanded {
		s = strings.TrimSpace(s)
		if len(s) >= 20 {
			claims = append(claims, s)
		}
	}
	return claims
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

// splitMarkdownLists splits text on markdown-style list items (- or *) that
// appear at the start of a line. Non-list text before the first item is
// preserved as a separate element.
func splitMarkdownLists(text string) []string {
	lines := strings.Split(text, "\n")
	hasList := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if (strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ")) && len(trimmed) > 2 {
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
		if (strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ")) && len(trimmed) > 2 {
			// Flush any accumulated non-list text.
			if before := strings.TrimSpace(current.String()); before != "" {
				parts = append(parts, before)
			}
			current.Reset()
			// Strip the list marker.
			parts = append(parts, strings.TrimSpace(trimmed[2:]))
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
