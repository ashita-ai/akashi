package config

import (
	"testing"
)

func TestEnvIntValid(t *testing.T) {
	t.Setenv("TEST_INT", "42")
	v, err := envInt("TEST_INT", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}
}

func TestEnvIntFallback(t *testing.T) {
	// TEST_INT_MISSING is not set.
	v, err := envInt("TEST_INT_MISSING", 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 99 {
		t.Fatalf("expected fallback 99, got %d", v)
	}
}

func TestEnvIntInvalid(t *testing.T) {
	t.Setenv("TEST_INT_BAD", "abc")
	_, err := envInt("TEST_INT_BAD", 0)
	if err == nil {
		t.Fatal("expected error for non-integer value, got nil")
	}
	if got := err.Error(); got != `TEST_INT_BAD="abc" is not a valid integer` {
		t.Fatalf("unexpected error message: %s", got)
	}
}

func TestEnvBoolValid(t *testing.T) {
	t.Setenv("TEST_BOOL", "true")
	v, err := envBool("TEST_BOOL", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v {
		t.Fatal("expected true")
	}
}

func TestEnvBoolInvalid(t *testing.T) {
	t.Setenv("TEST_BOOL_BAD", "maybe")
	_, err := envBool("TEST_BOOL_BAD", false)
	if err == nil {
		t.Fatal("expected error for non-boolean value, got nil")
	}
	if got := err.Error(); got != `TEST_BOOL_BAD="maybe" is not a valid boolean` {
		t.Fatalf("unexpected error message: %s", got)
	}
}

func TestEnvDurationValid(t *testing.T) {
	t.Setenv("TEST_DUR", "5s")
	v, err := envDuration("TEST_DUR", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Seconds() != 5 {
		t.Fatalf("expected 5s, got %s", v)
	}
}

func TestEnvDurationInvalid(t *testing.T) {
	t.Setenv("TEST_DUR_BAD", "five-seconds")
	_, err := envDuration("TEST_DUR_BAD", 0)
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
	if got := err.Error(); got != `TEST_DUR_BAD="five-seconds" is not a valid duration` {
		t.Fatalf("unexpected error message: %s", got)
	}
}

func TestLoadFailsOnInvalidPort(t *testing.T) {
	t.Setenv("AKASHI_PORT", "abc")
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail with invalid AKASHI_PORT")
	}
	// Error should mention the variable name and value.
	if got := err.Error(); !contains(got, "AKASHI_PORT") || !contains(got, "abc") {
		t.Fatalf("error should mention AKASHI_PORT and value 'abc', got: %s", got)
	}
}

func TestLoadFailsOnMultipleInvalid(t *testing.T) {
	t.Setenv("AKASHI_PORT", "abc")
	t.Setenv("AKASHI_SMTP_PORT", "xyz")
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail with multiple invalid vars")
	}
	got := err.Error()
	if !contains(got, "AKASHI_PORT") {
		t.Fatalf("error should mention AKASHI_PORT, got: %s", got)
	}
	if !contains(got, "AKASHI_SMTP_PORT") {
		t.Fatalf("error should mention AKASHI_SMTP_PORT, got: %s", got)
	}
}

func TestLoadSucceedsWithDefaults(t *testing.T) {
	// With no env vars set, Load should succeed using all defaults.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed with defaults, got: %v", err)
	}
	if cfg.Port != 8080 {
		t.Fatalf("expected default port 8080, got %d", cfg.Port)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
