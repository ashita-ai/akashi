package config

import (
	"testing"
	"time"
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
	t.Setenv("AKASHI_EMBEDDING_DIMENSIONS", "xyz")
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail with multiple invalid vars")
	}
	got := err.Error()
	if !contains(got, "AKASHI_PORT") {
		t.Fatalf("error should mention AKASHI_PORT, got: %s", got)
	}
	if !contains(got, "AKASHI_EMBEDDING_DIMENSIONS") {
		t.Fatalf("error should mention AKASHI_EMBEDDING_DIMENSIONS, got: %s", got)
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
	if cfg.EnableDestructiveDelete {
		t.Fatal("expected destructive delete to be disabled by default")
	}
	// WAL should be enabled by default.
	if cfg.WALDir != "./data/wal" {
		t.Fatalf("expected default WALDir %q, got %q", "./data/wal", cfg.WALDir)
	}
	if cfg.WALDisable {
		t.Fatal("expected WALDisable to be false by default")
	}
}

func TestLoad_WALDisableOverridesDir(t *testing.T) {
	t.Setenv("AKASHI_WAL_DISABLE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed, got: %v", err)
	}
	if cfg.WALDir != "" {
		t.Fatalf("expected WALDir to be empty when AKASHI_WAL_DISABLE=true, got %q", cfg.WALDir)
	}
	if !cfg.WALDisable {
		t.Fatal("expected WALDisable to be true")
	}
}

func TestLoad_ExplicitWALDir(t *testing.T) {
	t.Setenv("AKASHI_WAL_DIR", "/custom/wal/path")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed, got: %v", err)
	}
	if cfg.WALDir != "/custom/wal/path" {
		t.Fatalf("expected WALDir %q, got %q", "/custom/wal/path", cfg.WALDir)
	}
}

func TestLoad_WALDisableOverridesExplicitDir(t *testing.T) {
	// AKASHI_WAL_DISABLE=true should override even an explicit AKASHI_WAL_DIR.
	t.Setenv("AKASHI_WAL_DIR", "/custom/wal/path")
	t.Setenv("AKASHI_WAL_DISABLE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed, got: %v", err)
	}
	if cfg.WALDir != "" {
		t.Fatalf("expected WALDir to be empty when AKASHI_WAL_DISABLE=true, got %q", cfg.WALDir)
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

func TestLoad_JWTKeyPathValidation(t *testing.T) {
	bogusPath := "/tmp/akashi-test-nonexistent-key-file.pem"
	t.Setenv("AKASHI_JWT_PRIVATE_KEY", bogusPath)
	t.Setenv("AKASHI_JWT_PUBLIC_KEY", bogusPath)

	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail when AKASHI_JWT_PRIVATE_KEY points to a nonexistent file")
	}
	got := err.Error()
	if !contains(got, bogusPath) {
		t.Fatalf("error should mention the path %q, got: %s", bogusPath, got)
	}
	if !contains(got, "AKASHI_JWT_PRIVATE_KEY") {
		t.Fatalf("error should mention AKASHI_JWT_PRIVATE_KEY, got: %s", got)
	}
}

func TestLoad_JWTKeyBothOrNeither(t *testing.T) {
	t.Run("private only fails", func(t *testing.T) {
		t.Setenv("AKASHI_JWT_PRIVATE_KEY", "/some/path")
		t.Setenv("AKASHI_JWT_PUBLIC_KEY", "")

		_, err := Load()
		if err == nil {
			t.Fatal("expected Load() to fail when only private key is set")
		}
		if !contains(err.Error(), "both be set or both be empty") {
			t.Fatalf("error should mention both-or-neither, got: %s", err.Error())
		}
	})

	t.Run("public only fails", func(t *testing.T) {
		t.Setenv("AKASHI_JWT_PRIVATE_KEY", "")
		t.Setenv("AKASHI_JWT_PUBLIC_KEY", "/some/path")

		_, err := Load()
		if err == nil {
			t.Fatal("expected Load() to fail when only public key is set")
		}
		if !contains(err.Error(), "both be set or both be empty") {
			t.Fatalf("error should mention both-or-neither, got: %s", err.Error())
		}
	})

	t.Run("both empty succeeds (ephemeral)", func(t *testing.T) {
		t.Setenv("AKASHI_JWT_PRIVATE_KEY", "")
		t.Setenv("AKASHI_JWT_PUBLIC_KEY", "")

		_, err := Load()
		if err != nil {
			t.Fatalf("expected Load() to succeed with both keys empty (ephemeral mode), got: %v", err)
		}
	})
}

func TestLoad_OTELEndpointParsing(t *testing.T) {
	endpoint := "https://otel.example.com:4317"
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed, got: %v", err)
	}
	if cfg.OTELEndpoint != endpoint {
		t.Fatalf("expected OTELEndpoint %q, got %q", endpoint, cfg.OTELEndpoint)
	}
}

func TestLoad_EmbeddingProviderSelection(t *testing.T) {
	t.Setenv("AKASHI_EMBEDDING_PROVIDER", "ollama")
	t.Setenv("OLLAMA_URL", "http://localhost:11434")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed, got: %v", err)
	}
	if cfg.EmbeddingProvider != "ollama" {
		t.Fatalf("expected EmbeddingProvider %q, got %q", "ollama", cfg.EmbeddingProvider)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Fatalf("expected OllamaURL %q, got %q", "http://localhost:11434", cfg.OllamaURL)
	}
}

func TestLoad_QdrantURLValidation(t *testing.T) {
	t.Run("explicit URL", func(t *testing.T) {
		qdrantURL := "https://qdrant.example.com:6334"
		t.Setenv("QDRANT_URL", qdrantURL)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("expected Load() to succeed, got: %v", err)
		}
		if cfg.QdrantURL != qdrantURL {
			t.Fatalf("expected QdrantURL %q, got %q", qdrantURL, cfg.QdrantURL)
		}
	})

	t.Run("empty default", func(t *testing.T) {
		// QDRANT_URL is not set; default should be empty.
		cfg, err := Load()
		if err != nil {
			t.Fatalf("expected Load() to succeed, got: %v", err)
		}
		if cfg.QdrantURL != "" {
			t.Fatalf("expected empty QdrantURL by default, got %q", cfg.QdrantURL)
		}
	})
}

func TestLoad_ConflictScoringThresholdDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed, got: %v", err)
	}
	if cfg.ConflictClaimTopicSimFloor != 0.60 {
		t.Fatalf("expected ConflictClaimTopicSimFloor 0.60, got %f", cfg.ConflictClaimTopicSimFloor)
	}
	if cfg.ConflictClaimDivFloor != 0.15 {
		t.Fatalf("expected ConflictClaimDivFloor 0.15, got %f", cfg.ConflictClaimDivFloor)
	}
	if cfg.ConflictDecisionTopicSimFloor != 0.70 {
		t.Fatalf("expected ConflictDecisionTopicSimFloor 0.70, got %f", cfg.ConflictDecisionTopicSimFloor)
	}
}

func TestLoad_ConflictScoringThresholdOverrides(t *testing.T) {
	t.Setenv("AKASHI_CONFLICT_CLAIM_TOPIC_SIM_FLOOR", "0.55")
	t.Setenv("AKASHI_CONFLICT_CLAIM_DIV_FLOOR", "0.20")
	t.Setenv("AKASHI_CONFLICT_DECISION_TOPIC_SIM_FLOOR", "0.65")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed, got: %v", err)
	}
	if cfg.ConflictClaimTopicSimFloor != 0.55 {
		t.Fatalf("expected ConflictClaimTopicSimFloor 0.55, got %f", cfg.ConflictClaimTopicSimFloor)
	}
	if cfg.ConflictClaimDivFloor != 0.20 {
		t.Fatalf("expected ConflictClaimDivFloor 0.20, got %f", cfg.ConflictClaimDivFloor)
	}
	if cfg.ConflictDecisionTopicSimFloor != 0.65 {
		t.Fatalf("expected ConflictDecisionTopicSimFloor 0.65, got %f", cfg.ConflictDecisionTopicSimFloor)
	}
}

func TestEnvFloat64Valid(t *testing.T) {
	t.Setenv("TEST_FLOAT", "3.14")
	v, err := envFloat64("TEST_FLOAT", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 3.14 {
		t.Fatalf("expected 3.14, got %f", v)
	}
}

func TestEnvFloat64Fallback(t *testing.T) {
	v, err := envFloat64("TEST_FLOAT_MISSING", 2.71)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 2.71 {
		t.Fatalf("expected fallback 2.71, got %f", v)
	}
}

func TestEnvFloat64Invalid(t *testing.T) {
	t.Setenv("TEST_FLOAT_BAD", "notanumber")
	_, err := envFloat64("TEST_FLOAT_BAD", 0)
	if err == nil {
		t.Fatal("expected error for non-float value, got nil")
	}
	if !contains(err.Error(), "TEST_FLOAT_BAD") {
		t.Fatalf("error should mention the key, got: %s", err.Error())
	}
}

func TestEnvBoolFallback(t *testing.T) {
	v, err := envBool("TEST_BOOL_MISSING", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v {
		t.Fatal("expected fallback true")
	}
}

func TestEnvDurationFallback(t *testing.T) {
	v, err := envDuration("TEST_DUR_MISSING", 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 10*time.Second {
		t.Fatalf("expected fallback 10s, got %s", v)
	}
}

func TestEnvStr(t *testing.T) {
	t.Run("with value", func(t *testing.T) {
		t.Setenv("TEST_STR", "hello")
		v := envStr("TEST_STR", "default")
		if v != "hello" {
			t.Fatalf("expected 'hello', got %q", v)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		v := envStr("TEST_STR_MISSING", "default")
		if v != "default" {
			t.Fatalf("expected 'default', got %q", v)
		}
	})
}

func TestEnvStrSlice(t *testing.T) {
	t.Run("with values", func(t *testing.T) {
		t.Setenv("TEST_SLICE", "a, b, c")
		v := envStrSlice("TEST_SLICE", nil)
		if len(v) != 3 {
			t.Fatalf("expected 3 items, got %d", len(v))
		}
		if v[0] != "a" || v[1] != "b" || v[2] != "c" {
			t.Fatalf("unexpected values: %v", v)
		}
	})

	t.Run("fallback on empty", func(t *testing.T) {
		fallback := []string{"x"}
		v := envStrSlice("TEST_SLICE_MISSING", fallback)
		if len(v) != 1 || v[0] != "x" {
			t.Fatalf("expected fallback, got %v", v)
		}
	})

	t.Run("whitespace-only entries filtered", func(t *testing.T) {
		t.Setenv("TEST_SLICE_EMPTY", "  , , ")
		v := envStrSlice("TEST_SLICE_EMPTY", []string{"fallback"})
		if len(v) != 1 || v[0] != "fallback" {
			t.Fatalf("expected fallback for whitespace-only entries, got %v", v)
		}
	})

	t.Run("single value", func(t *testing.T) {
		t.Setenv("TEST_SLICE_SINGLE", "only-one")
		v := envStrSlice("TEST_SLICE_SINGLE", nil)
		if len(v) != 1 || v[0] != "only-one" {
			t.Fatalf("expected single value, got %v", v)
		}
	})
}

func TestValidate_NegativeEmbeddingDimensions(t *testing.T) {
	cfg := validBaseConfig()
	cfg.EmbeddingDimensions = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for negative embedding dimensions")
	}
	if !contains(err.Error(), "AKASHI_EMBEDDING_DIMENSIONS") {
		t.Fatalf("error should mention AKASHI_EMBEDDING_DIMENSIONS, got: %s", err.Error())
	}
}

func TestValidate_ZeroMaxRequestBodyBytes(t *testing.T) {
	cfg := validBaseConfig()
	cfg.MaxRequestBodyBytes = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for zero MaxRequestBodyBytes")
	}
	if !contains(err.Error(), "AKASHI_MAX_REQUEST_BODY_BYTES") {
		t.Fatalf("error should mention AKASHI_MAX_REQUEST_BODY_BYTES, got: %s", err.Error())
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	tests := []struct {
		name string
		port int
	}{
		{"zero", 0},
		{"negative", -1},
		{"too large", 70000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig()
			cfg.Port = tt.port
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error for invalid port")
			}
			if !contains(err.Error(), "AKASHI_PORT") {
				t.Fatalf("error should mention AKASHI_PORT, got: %s", err.Error())
			}
		})
	}
}

func TestValidate_NegativeTimeouts(t *testing.T) {
	tests := []struct {
		name   string
		setter func(*Config)
		errStr string
	}{
		{
			name:   "negative read timeout",
			setter: func(c *Config) { c.ReadTimeout = -1 * time.Second },
			errStr: "AKASHI_READ_TIMEOUT",
		},
		{
			name:   "negative write timeout",
			setter: func(c *Config) { c.WriteTimeout = -1 * time.Second },
			errStr: "AKASHI_WRITE_TIMEOUT",
		},
		{
			name:   "negative event flush timeout",
			setter: func(c *Config) { c.EventFlushTimeout = -1 * time.Millisecond },
			errStr: "AKASHI_EVENT_FLUSH_TIMEOUT",
		},
		{
			name:   "negative shutdown HTTP timeout",
			setter: func(c *Config) { c.ShutdownHTTPTimeout = -1 * time.Second },
			errStr: "AKASHI_SHUTDOWN_HTTP_TIMEOUT",
		},
		{
			name:   "negative shutdown buffer drain timeout",
			setter: func(c *Config) { c.ShutdownBufferDrainTimeout = -1 * time.Second },
			errStr: "AKASHI_SHUTDOWN_BUFFER_DRAIN_TIMEOUT",
		},
		{
			name:   "negative shutdown outbox drain timeout",
			setter: func(c *Config) { c.ShutdownOutboxDrainTimeout = -1 * time.Second },
			errStr: "AKASHI_SHUTDOWN_OUTBOX_DRAIN_TIMEOUT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig()
			tt.setter(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !contains(err.Error(), tt.errStr) {
				t.Fatalf("error should mention %s, got: %s", tt.errStr, err.Error())
			}
		})
	}
}

func TestValidate_ZeroEventBufferSize(t *testing.T) {
	cfg := validBaseConfig()
	cfg.EventBufferSize = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for zero EventBufferSize")
	}
	if !contains(err.Error(), "AKASHI_EVENT_BUFFER_SIZE") {
		t.Fatalf("error should mention AKASHI_EVENT_BUFFER_SIZE, got: %s", err.Error())
	}
}

func TestValidate_EmptyDatabaseURL(t *testing.T) {
	cfg := validBaseConfig()
	cfg.DatabaseURL = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty DatabaseURL")
	}
	if !contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("error should mention DATABASE_URL, got: %s", err.Error())
	}
}

func TestValidate_RateLimitEnabledRequiresPositiveValues(t *testing.T) {
	t.Run("zero RPS", func(t *testing.T) {
		cfg := validBaseConfig()
		cfg.RateLimitEnabled = true
		cfg.RateLimitRPS = 0

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected validation error for zero RateLimitRPS")
		}
		if !contains(err.Error(), "AKASHI_RATE_LIMIT_RPS") {
			t.Fatalf("error should mention AKASHI_RATE_LIMIT_RPS, got: %s", err.Error())
		}
	})

	t.Run("zero burst", func(t *testing.T) {
		cfg := validBaseConfig()
		cfg.RateLimitEnabled = true
		cfg.RateLimitBurst = 0

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected validation error for zero RateLimitBurst")
		}
		if !contains(err.Error(), "AKASHI_RATE_LIMIT_BURST") {
			t.Fatalf("error should mention AKASHI_RATE_LIMIT_BURST, got: %s", err.Error())
		}
	})

	t.Run("disabled rate limit does not validate RPS/burst", func(t *testing.T) {
		cfg := validBaseConfig()
		cfg.RateLimitEnabled = false
		cfg.RateLimitRPS = 0
		cfg.RateLimitBurst = 0

		err := cfg.Validate()
		if err != nil {
			t.Fatalf("expected no validation error when rate limiting is disabled, got: %v", err)
		}
	})
}

func TestValidate_KeyFileValidation(t *testing.T) {
	t.Run("directory instead of file", func(t *testing.T) {
		dir := t.TempDir()
		err := validateKeyFile(dir, "TEST_KEY")
		if err == nil {
			t.Fatal("expected error for directory")
		}
		if !contains(err.Error(), "is a directory") {
			t.Fatalf("error should mention 'is a directory', got: %s", err.Error())
		}
	})
}

func TestValidate_ZeroIdempotencyIntervals(t *testing.T) {
	tests := []struct {
		name   string
		setter func(*Config)
		errStr string
	}{
		{
			name:   "zero cleanup interval",
			setter: func(c *Config) { c.IdempotencyCleanupInterval = 0 },
			errStr: "AKASHI_IDEMPOTENCY_CLEANUP_INTERVAL",
		},
		{
			name:   "zero completed TTL",
			setter: func(c *Config) { c.IdempotencyCompletedTTL = 0 },
			errStr: "AKASHI_IDEMPOTENCY_COMPLETED_TTL",
		},
		{
			name:   "zero abandoned TTL",
			setter: func(c *Config) { c.IdempotencyAbandonedTTL = 0 },
			errStr: "AKASHI_IDEMPOTENCY_ABANDONED_TTL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig()
			tt.setter(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !contains(err.Error(), tt.errStr) {
				t.Fatalf("error should mention %s, got: %s", tt.errStr, err.Error())
			}
		})
	}
}

func TestValidate_ZeroIntervals(t *testing.T) {
	tests := []struct {
		name   string
		setter func(*Config)
		errStr string
	}{
		{
			name:   "zero outbox poll interval",
			setter: func(c *Config) { c.OutboxPollInterval = 0 },
			errStr: "AKASHI_OUTBOX_POLL_INTERVAL",
		},
		{
			name:   "zero conflict refresh interval",
			setter: func(c *Config) { c.ConflictRefreshInterval = 0 },
			errStr: "AKASHI_CONFLICT_REFRESH_INTERVAL",
		},
		{
			name:   "zero integrity proof interval",
			setter: func(c *Config) { c.IntegrityProofInterval = 0 },
			errStr: "AKASHI_INTEGRITY_PROOF_INTERVAL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig()
			tt.setter(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !contains(err.Error(), tt.errStr) {
				t.Fatalf("error should mention %s, got: %s", tt.errStr, err.Error())
			}
		})
	}
}

func TestLoad_InvalidBooleanEnvVar(t *testing.T) {
	t.Setenv("AKASHI_RATE_LIMIT_ENABLED", "notabool")
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail with invalid boolean")
	}
	if !contains(err.Error(), "AKASHI_RATE_LIMIT_ENABLED") {
		t.Fatalf("error should mention the variable, got: %s", err.Error())
	}
}

func TestLoad_InvalidDurationEnvVar(t *testing.T) {
	t.Setenv("AKASHI_READ_TIMEOUT", "notaduration")
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail with invalid duration")
	}
	if !contains(err.Error(), "AKASHI_READ_TIMEOUT") {
		t.Fatalf("error should mention the variable, got: %s", err.Error())
	}
}

func TestLoad_InvalidFloatEnvVar(t *testing.T) {
	t.Setenv("AKASHI_RATE_LIMIT_RPS", "notafloat")
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail with invalid float")
	}
	if !contains(err.Error(), "AKASHI_RATE_LIMIT_RPS") {
		t.Fatalf("error should mention the variable, got: %s", err.Error())
	}
}

// validBaseConfig returns a Config with all required fields set to valid values.
// Use this as a starting point for Validate() tests that want to test one field at a time.
func validBaseConfig() Config {
	return Config{
		Port:                       8080,
		ReadTimeout:                30 * time.Second,
		WriteTimeout:               30 * time.Second,
		DatabaseURL:                "postgres://localhost/test",
		EmbeddingDimensions:        1024,
		MaxRequestBodyBytes:        1024 * 1024,
		EventBufferSize:            1000,
		EventFlushTimeout:          100 * time.Millisecond,
		ShutdownHTTPTimeout:        10 * time.Second,
		ShutdownBufferDrainTimeout: 0,
		ShutdownOutboxDrainTimeout: 0,
		OutboxPollInterval:         1 * time.Second,
		ConflictRefreshInterval:    30 * time.Second,
		IntegrityProofInterval:     5 * time.Minute,
		IdempotencyCleanupInterval: 1 * time.Hour,
		IdempotencyCompletedTTL:    7 * 24 * time.Hour,
		IdempotencyAbandonedTTL:    24 * time.Hour,
		RateLimitEnabled:           true,
		RateLimitRPS:               100,
		RateLimitBurst:             200,
	}
}

func TestLoad_ConflictScoringThresholdInvalid(t *testing.T) {
	t.Setenv("AKASHI_CONFLICT_CLAIM_TOPIC_SIM_FLOOR", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail with invalid float")
	}
	if !contains(err.Error(), "AKASHI_CONFLICT_CLAIM_TOPIC_SIM_FLOOR") {
		t.Fatalf("error should mention the variable, got: %s", err.Error())
	}
}

func TestLoad_EarlyExitFloorExceedsThreshold(t *testing.T) {
	t.Setenv("AKASHI_CONFLICT_EARLY_EXIT_FLOOR", "0.50")
	t.Setenv("AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD", "0.30")

	_, err := Load()
	if err == nil {
		t.Fatal("expected Load() to fail when early exit floor exceeds significance threshold")
	}
	got := err.Error()
	if !contains(got, "AKASHI_CONFLICT_EARLY_EXIT_FLOOR") {
		t.Fatalf("error should mention AKASHI_CONFLICT_EARLY_EXIT_FLOOR, got: %s", got)
	}
	if !contains(got, "AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD") {
		t.Fatalf("error should mention AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD, got: %s", got)
	}
}

func TestLoad_EarlyExitFloorEqualsThreshold(t *testing.T) {
	t.Setenv("AKASHI_CONFLICT_EARLY_EXIT_FLOOR", "0.30")
	t.Setenv("AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD", "0.30")

	_, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed when floor equals threshold, got: %v", err)
	}
}

func TestLoad_AllEnvVarsHonored(t *testing.T) {
	t.Setenv("AKASHI_PORT", "9090")
	t.Setenv("DATABASE_URL", "postgres://test:test@db:5432/testdb")
	t.Setenv("NOTIFY_URL", "postgres://test:test@db:5432/testdb_notify")
	t.Setenv("AKASHI_JWT_EXPIRATION", "12h")
	t.Setenv("AKASHI_EMBEDDING_DIMENSIONS", "768")
	t.Setenv("OTEL_SERVICE_NAME", "akashi-test")
	t.Setenv("AKASHI_LOG_LEVEL", "debug")
	t.Setenv("AKASHI_RATE_LIMIT_RPS", "50.5")
	t.Setenv("AKASHI_RATE_LIMIT_BURST", "100")
	t.Setenv("AKASHI_CORS_ALLOWED_ORIGINS", "https://a.example.com, https://b.example.com")
	t.Setenv("AKASHI_SKIP_EMBEDDED_MIGRATIONS", "true")
	t.Setenv("AKASHI_ENABLE_DESTRUCTIVE_DELETE", "true")
	t.Setenv("AKASHI_SHUTDOWN_HTTP_TIMEOUT", "15s")
	t.Setenv("AKASHI_SHUTDOWN_BUFFER_DRAIN_TIMEOUT", "0")
	t.Setenv("AKASHI_SHUTDOWN_OUTBOX_DRAIN_TIMEOUT", "20s")
	t.Setenv("AKASHI_IDEMPOTENCY_CLEANUP_INTERVAL", "2h")
	t.Setenv("AKASHI_IDEMPOTENCY_COMPLETED_TTL", "72h")
	t.Setenv("AKASHI_IDEMPOTENCY_ABANDONED_TTL", "36h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed, got: %v", err)
	}

	if cfg.Port != 9090 {
		t.Fatalf("expected Port 9090, got %d", cfg.Port)
	}
	if cfg.DatabaseURL != "postgres://test:test@db:5432/testdb" {
		t.Fatalf("expected DatabaseURL %q, got %q", "postgres://test:test@db:5432/testdb", cfg.DatabaseURL)
	}
	if cfg.NotifyURL != "postgres://test:test@db:5432/testdb_notify" {
		t.Fatalf("expected NotifyURL %q, got %q", "postgres://test:test@db:5432/testdb_notify", cfg.NotifyURL)
	}
	if cfg.JWTExpiration != 12*time.Hour {
		t.Fatalf("expected JWTExpiration 12h, got %s", cfg.JWTExpiration)
	}
	if cfg.EmbeddingDimensions != 768 {
		t.Fatalf("expected EmbeddingDimensions 768, got %d", cfg.EmbeddingDimensions)
	}
	if cfg.ServiceName != "akashi-test" {
		t.Fatalf("expected ServiceName %q, got %q", "akashi-test", cfg.ServiceName)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected LogLevel %q, got %q", "debug", cfg.LogLevel)
	}
	if cfg.RateLimitRPS != 50.5 {
		t.Fatalf("expected RateLimitRPS 50.5, got %f", cfg.RateLimitRPS)
	}
	if cfg.RateLimitBurst != 100 {
		t.Fatalf("expected RateLimitBurst 100, got %d", cfg.RateLimitBurst)
	}
	if len(cfg.CORSAllowedOrigins) != 2 {
		t.Fatalf("expected 2 CORS origins, got %d", len(cfg.CORSAllowedOrigins))
	}
	if cfg.CORSAllowedOrigins[0] != "https://a.example.com" {
		t.Fatalf("expected first CORS origin %q, got %q", "https://a.example.com", cfg.CORSAllowedOrigins[0])
	}
	if cfg.CORSAllowedOrigins[1] != "https://b.example.com" {
		t.Fatalf("expected second CORS origin %q, got %q", "https://b.example.com", cfg.CORSAllowedOrigins[1])
	}
	if !cfg.SkipEmbeddedMigrations {
		t.Fatal("expected SkipEmbeddedMigrations true")
	}
	if !cfg.EnableDestructiveDelete {
		t.Fatal("expected EnableDestructiveDelete true")
	}
	if cfg.ShutdownHTTPTimeout != 15*time.Second {
		t.Fatalf("expected ShutdownHTTPTimeout 15s, got %s", cfg.ShutdownHTTPTimeout)
	}
	if cfg.ShutdownBufferDrainTimeout != 0 {
		t.Fatalf("expected ShutdownBufferDrainTimeout 0, got %s", cfg.ShutdownBufferDrainTimeout)
	}
	if cfg.ShutdownOutboxDrainTimeout != 20*time.Second {
		t.Fatalf("expected ShutdownOutboxDrainTimeout 20s, got %s", cfg.ShutdownOutboxDrainTimeout)
	}
	if cfg.IdempotencyCleanupInterval != 2*time.Hour {
		t.Fatalf("expected IdempotencyCleanupInterval 2h, got %s", cfg.IdempotencyCleanupInterval)
	}
	if cfg.IdempotencyCompletedTTL != 72*time.Hour {
		t.Fatalf("expected IdempotencyCompletedTTL 72h, got %s", cfg.IdempotencyCompletedTTL)
	}
	if cfg.IdempotencyAbandonedTTL != 36*time.Hour {
		t.Fatalf("expected IdempotencyAbandonedTTL 36h, got %s", cfg.IdempotencyAbandonedTTL)
	}
}

func TestLoad_EmbeddingModelProfile_AutoDetect(t *testing.T) {
	// Default provider is auto/ollama with OLLAMA_MODEL defaulting to mxbai-embed-large.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EmbeddingModelProfile != "mxbai-embed-large" {
		t.Fatalf("expected EmbeddingModelProfile 'mxbai-embed-large', got %q", cfg.EmbeddingModelProfile)
	}
	// With default model, thresholds should match original hardcoded values.
	if cfg.ConflictClaimTopicSimFloor != 0.60 {
		t.Fatalf("expected 0.60, got %f", cfg.ConflictClaimTopicSimFloor)
	}
}

func TestLoad_EmbeddingModelProfile_ExplicitOverride(t *testing.T) {
	t.Setenv("AKASHI_EMBEDDING_MODEL_PROFILE", "nomic-embed-text")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EmbeddingModelProfile != "nomic-embed-text" {
		t.Fatalf("expected 'nomic-embed-text', got %q", cfg.EmbeddingModelProfile)
	}
	// nomic-embed-text has lower claim topic sim floor than mxbai-embed-large.
	if cfg.ConflictClaimTopicSimFloor != 0.55 {
		t.Fatalf("expected 0.55 for nomic-embed-text, got %f", cfg.ConflictClaimTopicSimFloor)
	}
}

func TestLoad_EmbeddingModelProfile_WithDetectionProfile(t *testing.T) {
	t.Setenv("AKASHI_EMBEDDING_MODEL_PROFILE", "nomic-embed-text")
	t.Setenv("AKASHI_CONFLICT_PROFILE", "high_precision")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	// nomic-embed-text base (0.55) + high_precision delta (+0.05) = 0.60
	diff := cfg.ConflictClaimTopicSimFloor - 0.60
	if diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("expected ~0.60 (nomic 0.55 + high_precision +0.05), got %f", cfg.ConflictClaimTopicSimFloor)
	}
}

func TestLoad_EmbeddingModelProfile_EnvVarOverridesProfile(t *testing.T) {
	t.Setenv("AKASHI_EMBEDDING_MODEL_PROFILE", "nomic-embed-text")
	t.Setenv("AKASHI_CONFLICT_CLAIM_TOPIC_SIM_FLOOR", "0.42")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	// Explicit env var should override the model+detection profile.
	if cfg.ConflictClaimTopicSimFloor != 0.42 {
		t.Fatalf("expected 0.42 (explicit override), got %f", cfg.ConflictClaimTopicSimFloor)
	}
}

func TestEmbeddingModelThresholds_Known(t *testing.T) {
	claimSim, claimDiv, decSim, known := EmbeddingModelThresholds("mxbai-embed-large")
	if !known {
		t.Fatal("expected mxbai-embed-large to be a known model")
	}
	if claimSim != 0.60 || claimDiv != 0.15 || decSim != 0.70 {
		t.Fatalf("unexpected thresholds: %f, %f, %f", claimSim, claimDiv, decSim)
	}
}

func TestEmbeddingModelThresholds_Unknown(t *testing.T) {
	claimSim, claimDiv, decSim, known := EmbeddingModelThresholds("some-unknown-model")
	if known {
		t.Fatal("expected unknown model to return known=false")
	}
	// Should fall back to mxbai-embed-large defaults.
	if claimSim != 0.60 || claimDiv != 0.15 || decSim != 0.70 {
		t.Fatalf("expected mxbai-embed-large defaults, got: %f, %f, %f", claimSim, claimDiv, decSim)
	}
}
