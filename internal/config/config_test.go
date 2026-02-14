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
	t.Setenv("AKASHI_IDEMPOTENCY_IN_PROGRESS_TTL", "6m")
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
	if cfg.IdempotencyInProgressTTL != 6*time.Minute {
		t.Fatalf("expected IdempotencyInProgressTTL 6m, got %s", cfg.IdempotencyInProgressTTL)
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
