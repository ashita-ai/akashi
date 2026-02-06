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
	EmbeddingProvider   string // "auto", "openai", "ollama", or "noop"
	OpenAIAPIKey        string
	EmbeddingModel      string
	EmbeddingDimensions int // Vector dimensions; must match the chosen model's output.
	OllamaURL           string
	OllamaModel         string

	// OTEL settings.
	OTELEndpoint string
	ServiceName  string

	// Stripe billing settings.
	StripeSecretKey     string
	StripeWebhookSecret string
	StripePriceIDPro    string // Stripe Price ID for the Pro plan.

	// SMTP settings for email verification.
	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string
	BaseURL      string // e.g., "https://akashi.example.com" for verification links.

	// Operational settings.
	LogLevel                string
	ConflictRefreshInterval time.Duration
	EventBufferSize         int
	EventFlushTimeout       time.Duration
	MaxRequestBodyBytes     int64 // Maximum request body size in bytes.
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (Config, error) {
	cfg := Config{
		Port:                    envInt("AKASHI_PORT", 8080),
		ReadTimeout:             envDuration("AKASHI_READ_TIMEOUT", 30*time.Second),
		WriteTimeout:            envDuration("AKASHI_WRITE_TIMEOUT", 30*time.Second),
		DatabaseURL:             envStr("DATABASE_URL", "postgres://akashi:akashi@localhost:6432/akashi?sslmode=verify-full"),
		NotifyURL:               envStr("NOTIFY_URL", "postgres://akashi:akashi@localhost:5432/akashi?sslmode=verify-full"),
		RedisURL:                envStr("REDIS_URL", "redis://localhost:6379/0"),
		JWTPrivateKeyPath:       envStr("AKASHI_JWT_PRIVATE_KEY", ""),
		JWTPublicKeyPath:        envStr("AKASHI_JWT_PUBLIC_KEY", ""),
		JWTExpiration:           envDuration("AKASHI_JWT_EXPIRATION", 24*time.Hour),
		AdminAPIKey:             envStr("AKASHI_ADMIN_API_KEY", ""),
		EmbeddingProvider:       envStr("AKASHI_EMBEDDING_PROVIDER", "auto"),
		OpenAIAPIKey:            envStr("OPENAI_API_KEY", ""),
		EmbeddingModel:          envStr("AKASHI_EMBEDDING_MODEL", "text-embedding-3-small"),
		EmbeddingDimensions:     envInt("AKASHI_EMBEDDING_DIMENSIONS", 1024),
		OllamaURL:               envStr("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:             envStr("OLLAMA_MODEL", "mxbai-embed-large"),
		OTELEndpoint:            envStr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:             envStr("OTEL_SERVICE_NAME", "akashi"),
		StripeSecretKey:         envStr("STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret:     envStr("STRIPE_WEBHOOK_SECRET", ""),
		StripePriceIDPro:        envStr("STRIPE_PRO_PRICE_ID", ""),
		SMTPHost:                envStr("AKASHI_SMTP_HOST", ""),
		SMTPPort:                envInt("AKASHI_SMTP_PORT", 587),
		SMTPUser:                envStr("AKASHI_SMTP_USER", ""),
		SMTPPassword:            envStr("AKASHI_SMTP_PASSWORD", ""),
		SMTPFrom:                envStr("AKASHI_SMTP_FROM", "noreply@akashi.dev"),
		BaseURL:                 envStr("AKASHI_BASE_URL", "http://localhost:8080"),
		LogLevel:                envStr("AKASHI_LOG_LEVEL", "info"),
		ConflictRefreshInterval: envDuration("AKASHI_CONFLICT_REFRESH_INTERVAL", 30*time.Second),
		EventBufferSize:         envInt("AKASHI_EVENT_BUFFER_SIZE", 1000),
		EventFlushTimeout:       envDuration("AKASHI_EVENT_FLUSH_TIMEOUT", 100*time.Millisecond),
		MaxRequestBodyBytes:     int64(envInt("AKASHI_MAX_REQUEST_BODY_BYTES", 1*1024*1024)), // 1 MB default
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
	if c.EmbeddingDimensions <= 0 {
		return fmt.Errorf("config: AKASHI_EMBEDDING_DIMENSIONS must be positive")
	}
	if c.MaxRequestBodyBytes <= 0 {
		return fmt.Errorf("config: AKASHI_MAX_REQUEST_BODY_BYTES must be positive")
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
