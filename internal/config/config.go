// Package config loads and validates application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	// Server settings.
	Port         int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// Database settings.
	DatabaseURL string // PgBouncer or direct Postgres URL for queries.
	NotifyURL   string // Direct Postgres URL for LISTEN/NOTIFY.

	// JWT settings.
	JWTPrivateKeyPath string // Path to Ed25519 private key PEM file.
	JWTPublicKeyPath  string // Path to Ed25519 public key PEM file.
	JWTExpiration     time.Duration

	// Admin bootstrap.
	AdminAPIKey string // API key for the initial admin agent.

	// Embedding provider settings.
	EmbeddingProvider   string // "auto", "openai", "ollama", or "noop"
	OpenAIAPIKey        string
	EmbeddingModel      string
	EmbeddingDimensions int // Vector dimensions; must match the chosen model's output.
	OllamaURL           string
	OllamaModel         string

	// OTEL settings.
	OTELEndpoint string
	OTELInsecure bool // Use HTTP instead of HTTPS for OTEL exporter (default: false).
	ServiceName  string

	// Qdrant vector search settings.
	QdrantURL          string // gRPC-compatible URL (e.g. "https://xyz.cloud.qdrant.io:6334")
	QdrantAPIKey       string
	QdrantCollection   string
	OutboxPollInterval time.Duration
	OutboxBatchSize    int

	// CORS settings.
	CORSAllowedOrigins []string // Allowed origins for CORS; ["*"] permits all.

	// Operational settings.
	LogLevel                string
	ConflictRefreshInterval time.Duration
	IntegrityProofInterval  time.Duration // How often to build Merkle tree proofs.
	EventBufferSize         int
	EventFlushTimeout       time.Duration
	MaxRequestBodyBytes     int64 // Maximum request body size in bytes.
}

// Load reads configuration from environment variables with sensible defaults.
// Returns an error if any environment variable contains an unparseable value.
// Missing variables use sensible defaults; only malformed values are rejected.
func Load() (Config, error) {
	var errs []error
	cfg := Config{
		DatabaseURL:        envStr("DATABASE_URL", "postgres://akashi:akashi@localhost:6432/akashi?sslmode=verify-full"),
		NotifyURL:          envStr("NOTIFY_URL", "postgres://akashi:akashi@localhost:5432/akashi?sslmode=verify-full"),
		JWTPrivateKeyPath:  envStr("AKASHI_JWT_PRIVATE_KEY", ""),
		JWTPublicKeyPath:   envStr("AKASHI_JWT_PUBLIC_KEY", ""),
		AdminAPIKey:        envStr("AKASHI_ADMIN_API_KEY", ""),
		EmbeddingProvider:  envStr("AKASHI_EMBEDDING_PROVIDER", "auto"),
		OpenAIAPIKey:       envStr("OPENAI_API_KEY", ""),
		EmbeddingModel:     envStr("AKASHI_EMBEDDING_MODEL", "text-embedding-3-small"),
		OllamaURL:          envStr("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:        envStr("OLLAMA_MODEL", "mxbai-embed-large"),
		OTELEndpoint:       envStr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:        envStr("OTEL_SERVICE_NAME", "akashi"),
		QdrantURL:          envStr("QDRANT_URL", ""),
		QdrantAPIKey:       envStr("QDRANT_API_KEY", ""),
		QdrantCollection:   envStr("QDRANT_COLLECTION", "akashi_decisions"),
		LogLevel:           envStr("AKASHI_LOG_LEVEL", "info"),
		CORSAllowedOrigins: envStrSlice("AKASHI_CORS_ALLOWED_ORIGINS", nil),
	}

	// Integer fields.
	cfg.Port, errs = collectInt(errs, "AKASHI_PORT", 8080)
	cfg.EmbeddingDimensions, errs = collectInt(errs, "AKASHI_EMBEDDING_DIMENSIONS", 1024)
	cfg.OutboxBatchSize, errs = collectInt(errs, "AKASHI_OUTBOX_BATCH_SIZE", 100)
	cfg.EventBufferSize, errs = collectInt(errs, "AKASHI_EVENT_BUFFER_SIZE", 1000)

	var maxReqBody int
	maxReqBody, errs = collectInt(errs, "AKASHI_MAX_REQUEST_BODY_BYTES", 1*1024*1024)
	cfg.MaxRequestBodyBytes = int64(maxReqBody)

	// Boolean fields.
	cfg.OTELInsecure, errs = collectBool(errs, "OTEL_EXPORTER_OTLP_INSECURE", false)

	// Duration fields.
	cfg.ReadTimeout, errs = collectDuration(errs, "AKASHI_READ_TIMEOUT", 30*time.Second)
	cfg.WriteTimeout, errs = collectDuration(errs, "AKASHI_WRITE_TIMEOUT", 30*time.Second)
	cfg.JWTExpiration, errs = collectDuration(errs, "AKASHI_JWT_EXPIRATION", 24*time.Hour)
	cfg.OutboxPollInterval, errs = collectDuration(errs, "AKASHI_OUTBOX_POLL_INTERVAL", 1*time.Second)
	cfg.ConflictRefreshInterval, errs = collectDuration(errs, "AKASHI_CONFLICT_REFRESH_INTERVAL", 30*time.Second)
	cfg.IntegrityProofInterval, errs = collectDuration(errs, "AKASHI_INTEGRITY_PROOF_INTERVAL", 5*time.Minute)
	cfg.EventFlushTimeout, errs = collectDuration(errs, "AKASHI_EVENT_FLUSH_TIMEOUT", 100*time.Millisecond)

	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return Config{}, fmt.Errorf("config: invalid environment variables:\n  %s", strings.Join(msgs, "\n  "))
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// collectInt parses an int env var, appending any error to the accumulator.
func collectInt(errs []error, key string, fallback int) (int, []error) {
	v, err := envInt(key, fallback)
	if err != nil {
		errs = append(errs, err)
	}
	return v, errs
}

// collectBool parses a bool env var, appending any error to the accumulator.
func collectBool(errs []error, key string, fallback bool) (bool, []error) {
	v, err := envBool(key, fallback)
	if err != nil {
		errs = append(errs, err)
	}
	return v, errs
}

// collectDuration parses a duration env var, appending any error to the accumulator.
func collectDuration(errs []error, key string, fallback time.Duration) (time.Duration, []error) {
	v, err := envDuration(key, fallback)
	if err != nil {
		errs = append(errs, err)
	}
	return v, errs
}

// Validate checks that required configuration is present and sane.
func (c Config) Validate() error {
	var errs []error

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("config: DATABASE_URL is required"))
	}
	if c.EmbeddingDimensions <= 0 {
		errs = append(errs, errors.New("config: AKASHI_EMBEDDING_DIMENSIONS must be positive"))
	}
	if c.MaxRequestBodyBytes <= 0 {
		errs = append(errs, errors.New("config: AKASHI_MAX_REQUEST_BODY_BYTES must be positive"))
	}
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, errors.New("config: AKASHI_PORT must be between 1 and 65535"))
	}
	if c.ReadTimeout <= 0 {
		errs = append(errs, errors.New("config: AKASHI_READ_TIMEOUT must be positive"))
	}
	if c.WriteTimeout <= 0 {
		errs = append(errs, errors.New("config: AKASHI_WRITE_TIMEOUT must be positive"))
	}
	if c.EventFlushTimeout <= 0 {
		errs = append(errs, errors.New("config: AKASHI_EVENT_FLUSH_TIMEOUT must be positive"))
	}
	if c.EventBufferSize <= 0 {
		errs = append(errs, errors.New("config: AKASHI_EVENT_BUFFER_SIZE must be positive"))
	}
	if c.OutboxPollInterval <= 0 {
		errs = append(errs, errors.New("config: AKASHI_OUTBOX_POLL_INTERVAL must be positive"))
	}
	if c.ConflictRefreshInterval <= 0 {
		errs = append(errs, errors.New("config: AKASHI_CONFLICT_REFRESH_INTERVAL must be positive"))
	}
	if c.IntegrityProofInterval <= 0 {
		errs = append(errs, errors.New("config: AKASHI_INTEGRITY_PROOF_INTERVAL must be positive"))
	}
	if c.JWTPrivateKeyPath != "" {
		if err := validateKeyFile(c.JWTPrivateKeyPath, "AKASHI_JWT_PRIVATE_KEY"); err != nil {
			errs = append(errs, err)
		}
	}
	if c.JWTPublicKeyPath != "" {
		if err := validateKeyFile(c.JWTPublicKeyPath, "AKASHI_JWT_PUBLIC_KEY"); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// validateKeyFile checks that a key file exists, is readable, is non-empty,
// and has restrictive permissions (owner-only on Unix).
func validateKeyFile(path, envVar string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config: %s %q: %w", envVar, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("config: %s %q is a directory, expected a file", envVar, path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("config: %s %q is empty", envVar, path)
	}
	// Check that the file is not world-readable (Unix permissions only).
	// info.Mode().Perm() returns the Unix permission bits.
	perm := info.Mode().Perm()
	if perm&0o077 != 0 {
		return fmt.Errorf("config: %s %q has overly permissive mode %04o (expected 0600 or stricter)", envVar, path, perm)
	}
	return nil
}

func envStr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid integer", key, v)
	}
	return n, nil
}

func envBool(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a valid boolean", key, v)
	}
	return b, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid duration", key, v)
	}
	return d, nil
}

// envStrSlice reads a comma-separated env var into a string slice.
// Returns fallback if the env var is empty or unset.
func envStrSlice(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
