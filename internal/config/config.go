// Package config loads and validates application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
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

	// Redis settings.
	RedisURL string

	// JWT settings.
	JWTPrivateKeyPath string // Path to Ed25519 private key PEM file.
	JWTPublicKeyPath  string // Path to Ed25519 public key PEM file.
	JWTExpiration     time.Duration

	// Admin bootstrap.
	AdminAPIKey string // API key for the initial admin agent.

	// Embedding provider settings.
	EmbeddingProvider string // "auto", "openai", "ollama", or "noop"
	OpenAIAPIKey      string
	EmbeddingModel    string
	OllamaURL         string
	OllamaModel       string

	// OTEL settings.
	OTELEndpoint string
	ServiceName  string

	// Operational settings.
	LogLevel              string
	ConflictRefreshInterval time.Duration
	EventBufferSize       int
	EventFlushTimeout     time.Duration
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (Config, error) {
	cfg := Config{
		Port:                    envInt("KYOYU_PORT", 8080),
		ReadTimeout:             envDuration("KYOYU_READ_TIMEOUT", 30*time.Second),
		WriteTimeout:            envDuration("KYOYU_WRITE_TIMEOUT", 30*time.Second),
		DatabaseURL:             envStr("DATABASE_URL", "postgres://kyoyu:kyoyu@localhost:6432/kyoyu?sslmode=disable"),
		NotifyURL:               envStr("NOTIFY_URL", "postgres://kyoyu:kyoyu@localhost:5432/kyoyu?sslmode=disable"),
		RedisURL:                envStr("REDIS_URL", "redis://localhost:6379/0"),
		JWTPrivateKeyPath:       envStr("KYOYU_JWT_PRIVATE_KEY", ""),
		JWTPublicKeyPath:        envStr("KYOYU_JWT_PUBLIC_KEY", ""),
		JWTExpiration:           envDuration("KYOYU_JWT_EXPIRATION", 24*time.Hour),
		AdminAPIKey:             envStr("KYOYU_ADMIN_API_KEY", ""),
		EmbeddingProvider:       envStr("KYOYU_EMBEDDING_PROVIDER", "auto"),
		OpenAIAPIKey:            envStr("OPENAI_API_KEY", ""),
		EmbeddingModel:          envStr("KYOYU_EMBEDDING_MODEL", "text-embedding-3-small"),
		OllamaURL:               envStr("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:             envStr("OLLAMA_MODEL", "mxbai-embed-large"),
		OTELEndpoint:            envStr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:             envStr("OTEL_SERVICE_NAME", "kyoyu"),
		LogLevel:                envStr("KYOYU_LOG_LEVEL", "info"),
		ConflictRefreshInterval: envDuration("KYOYU_CONFLICT_REFRESH_INTERVAL", 30*time.Second),
		EventBufferSize:         envInt("KYOYU_EVENT_BUFFER_SIZE", 1000),
		EventFlushTimeout:       envDuration("KYOYU_EVENT_FLUSH_TIMEOUT", 100*time.Millisecond),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that required configuration is present.
func (c Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("config: DATABASE_URL is required")
	}
	return nil
}

func envStr(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

func envDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}
