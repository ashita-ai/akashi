package conflicts

import (
	"strings"
	"unicode"
)

// SplitClaims breaks an outcome text into individual claims suitable for
// sentence-level embedding. It splits on sentence boundaries and numbered
// items, then filters out claims that are too short to be meaningful.
//
// The minimum useful claim length is 20 characters — shorter fragments
// (e.g. "Ratings:", "Three findings:") lack enough semantic content for
// meaningful embedding comparison.
func SplitClaims(outcome string) []string {
	if len(outcome) == 0 {
		return nil
	}

	// First pass: split on sentence-ending punctuation followed by whitespace.
	raw := splitSentences(outcome)

	// Second pass: split sentences that contain numbered lists like "(1) ... (2) ...".
	var expanded []string
	for _, s := range raw {
		expanded = append(expanded, splitNumberedItems(s)...)
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
