package signup

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateEmail(t *testing.T) {
	tests := []struct {
		email string
		valid bool
	}{
		{"user@example.com", true},
		{"user+tag@example.com", true},
		{"user@sub.domain.com", true},
		{"user.name@example.co.uk", true},
		{"", false},
		{"not-an-email", false},
		{"@example.com", false},
		{"user@", false},
		{"user@.com", false},
		{"user@example", false},
	}
	for _, tt := range tests {
		err := validateEmail(tt.email)
		if tt.valid {
			assert.NoError(t, err, "expected %q to be valid", tt.email)
		} else {
			assert.ErrorIs(t, err, ErrInvalidEmail, "expected %q to be invalid", tt.email)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		password string
		valid    bool
	}{
		{"StrongP@ss123", true},
		{"Abcdefghij1x", true},
		{"short1A", false},       // too short
		{"alllowercase1", false}, // no uppercase
		{"ALLUPPERCASE1", false}, // no lowercase
		{"AllLettersNoDigit", false},
		{"", false},
	}
	for _, tt := range tests {
		err := validatePassword(tt.password)
		if tt.valid {
			assert.NoError(t, err, "expected %q to be valid", tt.password)
		} else {
			assert.ErrorIs(t, err, ErrWeakPassword, "expected %q to be rejected", tt.password)
		}
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"My Org", "my-org"},
		{"ACME Corp", "acme-corp"},
		{"hello_world", "hello-world"},
		{"  spaces  everywhere  ", "spaces-everywhere"},
		{"Special!@#Chars", "specialchars"},
		{"multiple---hyphens", "multiple-hyphens"},
		{"Already-Good", "already-good"},
		{"123 Numbers", "123-numbers"},
	}
	for _, tt := range tests {
		result := slugify(tt.input)
		assert.Equal(t, tt.expected, result, "slugify(%q)", tt.input)
	}
}
