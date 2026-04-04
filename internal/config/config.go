// Package config loads and validates application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
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
	DBMaxConns  int32  // Max connections in the pool. 0 = pgxpool default (max(4, NumCPU)).
	DBMinConns  int32  // Min idle connections kept open. 0 = pgxpool default (0).

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

	// Rate limiting.
	RateLimitEnabled bool    // Enable rate limiting middleware (default: true).
	RateLimitRPS     float64 // Sustained requests per second per key (default: 100).
	RateLimitBurst   int     // Token bucket capacity per key (default: 200).
	TrustProxy       bool    // When true, use X-Forwarded-For for rate limit keys (default: false).

	// Conflict LLM validation.
	ConflictLLMModel              string  // Text generation model for conflict validation (e.g. "qwen3.5:9b" for Ollama).
	ConflictLLMThreads            int     // CPU threads Ollama may use per inference call (default: floor(NumCPU/3), min 1). 0 = let Ollama decide.
	ConflictCandidateLimit        int     // Max candidates retrieved from Qdrant per decision for conflict scoring (default: 20).
	ConflictBackfillWorkers       int     // Parallel workers for conflict scoring backfill (default: 4).
	ConflictDecayLambda           float64 // Temporal decay rate for conflict significance (default: 0.01, 0 disables).
	ConflictClaimTopicSimFloor    float64 // Min cosine similarity for two claims to be "about the same thing" (default: 0.60).
	ConflictClaimDivFloor         float64 // Min outcome divergence for claims to count as disagreeing (default: 0.15).
	ConflictDecisionTopicSimFloor float64 // Min decision-level topic similarity to activate claim-level scoring (default: 0.70).
	ConflictEarlyExitFloor        float64 // Min pre-LLM significance for early exit pruning (default: 0.25, 0 disables).
	ConflictOutcomeSimFloor       float64 // Min outcome cosine similarity to suppress as agreeing (default: 0.85, 0 disables).
	CrossEncoderURL               string  // URL of the cross-encoder reranking service (empty = disabled).
	CrossEncoderThreshold         float64 // Min cross-encoder score to proceed to LLM validation (default: 0.50).
	NLIURL                        string  // URL of NLI sidecar for stance-aware pre-filtering (empty = disabled). Takes precedence over CrossEncoderURL.
	ClaimExtractionLLM            bool    // Use the conflict LLM model for structured claim extraction (default: false).
	ForceConflictRescore          bool    // When true (and LLM validator configured), clear all conflicts and re-score at startup.
	ConflictProfile               string  // Named profile: "balanced" (default), "high_precision", "high_recall". Individual env vars override.
	EmbeddingModelProfile         string  // Embedding model name for threshold profile selection (auto-detected from provider config).

	// Event WAL (write-ahead log) for crash-durable event buffering.
	WALDir            string        // Directory for WAL files. Default: "./data/wal". Set AKASHI_WAL_DISABLE=true to disable.
	WALDisable        bool          // Explicitly disable WAL (for dev/testing). Default: false.
	WALSyncMode       string        // "full", "batch", "none". Default: "batch".
	WALSyncInterval   time.Duration // Sync interval for batch mode. Default: 10ms.
	WALSegmentSize    int           // Max segment size in bytes before rotation. Default: 64 MB.
	WALSegmentRecords int           // Max records per segment before rotation. Default: 100K.

	// Operational settings.
	LogLevel                      string
	SkipEmbeddedMigrations        bool // Skip startup embedded migrations; for external migration orchestration.
	EnableDestructiveDelete       bool // Enables irreversible DELETE /v1/agents/{agent_id}; default false.
	ConflictRefreshInterval       time.Duration
	ConflictSignificanceThreshold float64       // Minimum significance to store (default 0.30).
	IntegrityProofInterval        time.Duration // How often to build Merkle tree proofs.
	IntegrityAuditInterval        time.Duration // How often to verify stored Merkle proofs.
	IntegrityAuditTimeout         time.Duration // Timeout for each integrity audit tick (default 5m).
	IntegrityFullAuditInterval    time.Duration // How often to run exhaustive integrity audit across all orgs (default 24h, 0 disables).
	IntegrityFullAuditProofs      int           // Number of proofs to check per org in full sweep (default 50).
	EventBufferSize               int
	EventFlushTimeout             time.Duration
	ShutdownHTTPTimeout           time.Duration // 0 disables timeout (wait indefinitely).
	ShutdownAsyncDrainTimeout     time.Duration // 0 disables timeout (wait indefinitely).
	ShutdownBufferDrainTimeout    time.Duration // 0 disables timeout (wait indefinitely).
	ShutdownOutboxDrainTimeout    time.Duration // 0 disables timeout (wait indefinitely).
	ShutdownLoopDrainTimeout      time.Duration // 0 disables timeout (wait indefinitely).
	IdempotencyCleanupInterval    time.Duration // Background cleanup cadence for idempotency keys.
	IdempotencyCompletedTTL       time.Duration // Retention for completed idempotency records.
	IdempotencyAbandonedTTL       time.Duration // Hard TTL for abandoned in-progress idempotency records.
	MaxRequestBodyBytes           int64         // Maximum request body size in bytes.
	RetentionInterval             time.Duration // How often the background retention worker runs (default 24h).
	ClaimRetryInterval            time.Duration // How often to retry failed claim embeddings (default 2m).
	PercentileRefreshInterval     time.Duration // How often to refresh signal percentile caches (default 1h).
	AutoResolveInterval           time.Duration // How often the auto-resolution worker runs (default 1h, 0 disables).

	// Trace quality warnings.
	HighConfidenceWarnThreshold float32 // Confidence above this with zero evidence triggers a response warning (default: 0.85).

	// Self-serve signup.
	SignupEnabled bool // Enable POST /auth/signup for self-serve org creation (default: false).

	// IDE hook endpoint settings.
	HooksEnabled bool   // Enable /hooks/* IDE integration endpoints (default: true).
	HooksAPIKey  string // Optional API key for non-localhost hook access (default: "" = localhost only).
	AutoTrace    bool   // Auto-trace git commits from PostToolUse hooks (default: true).

	// Completeness profile overrides (tip filtering, not scoring).
	// JSON map of decision_type → profile overrides. Merges with built-in defaults
	// in internal/service/quality. Controls which completeness tips are surfaced
	// to agents for each decision type. Scoring is always uniform. Example:
	//   {"security":{"min_evidence":3,"alternatives_expected":true,"max_confidence_no_evidence":0.70}}
	CompletenessProfilesJSON string

	// Standard decision types override.
	// Comma-separated list of types considered "standard" for suggestion tips.
	// When set, replaces the built-in 12 defaults. Example:
	//   "architecture,security,data_pipeline,access_control"
	StandardDecisionTypes []string
}

// Load reads configuration from environment variables with sensible defaults.
// Returns an error if any environment variable contains an unparseable value.
// Missing variables use sensible defaults; only malformed values are rejected.
func Load() (Config, error) {
	var errs []error
	cfg := Config{
		DatabaseURL:              envStr("DATABASE_URL", "postgres://akashi:akashi@localhost:6432/akashi?sslmode=disable"),
		NotifyURL:                envStr("NOTIFY_URL", "postgres://akashi:akashi@localhost:5432/akashi?sslmode=disable"),
		JWTPrivateKeyPath:        envStr("AKASHI_JWT_PRIVATE_KEY", ""),
		JWTPublicKeyPath:         envStr("AKASHI_JWT_PUBLIC_KEY", ""),
		AdminAPIKey:              envStr("AKASHI_ADMIN_API_KEY", ""),
		EmbeddingProvider:        envStr("AKASHI_EMBEDDING_PROVIDER", "auto"),
		OpenAIAPIKey:             envStr("OPENAI_API_KEY", ""),
		EmbeddingModel:           envStr("AKASHI_EMBEDDING_MODEL", "text-embedding-3-small"),
		OllamaURL:                envStr("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:              envStr("OLLAMA_MODEL", "mxbai-embed-large"),
		OTELEndpoint:             envStr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:              envStr("OTEL_SERVICE_NAME", "akashi"),
		QdrantURL:                envStr("QDRANT_URL", ""),
		QdrantAPIKey:             envStr("QDRANT_API_KEY", ""),
		QdrantCollection:         envStr("QDRANT_COLLECTION", "akashi_decisions"),
		ConflictLLMModel:         envStr("AKASHI_CONFLICT_LLM_MODEL", ""),
		CrossEncoderURL:          envStr("AKASHI_CONFLICT_CROSS_ENCODER_URL", ""),
		NLIURL:                   envStr("AKASHI_CONFLICT_NLI_URL", ""),
		WALDir:                   envStr("AKASHI_WAL_DIR", "./data/wal"),
		WALSyncMode:              envStr("AKASHI_WAL_SYNC_MODE", "batch"),
		LogLevel:                 envStr("AKASHI_LOG_LEVEL", "info"),
		CORSAllowedOrigins:       envStrSlice("AKASHI_CORS_ALLOWED_ORIGINS", nil),
		HooksAPIKey:              envStr("AKASHI_HOOKS_API_KEY", ""),
		CompletenessProfilesJSON: envStr("AKASHI_COMPLETENESS_PROFILES", ""),
		StandardDecisionTypes:    envStrSlice("AKASHI_STANDARD_DECISION_TYPES", nil),
	}

	// Integer fields.
	cfg.Port, errs = collectInt(errs, "AKASHI_PORT", 8080)
	cfg.EmbeddingDimensions, errs = collectInt(errs, "AKASHI_EMBEDDING_DIMENSIONS", 1024)
	cfg.OutboxBatchSize, errs = collectInt(errs, "AKASHI_OUTBOX_BATCH_SIZE", 100)
	cfg.EventBufferSize, errs = collectInt(errs, "AKASHI_EVENT_BUFFER_SIZE", 1000)
	cfg.RateLimitBurst, errs = collectInt(errs, "AKASHI_RATE_LIMIT_BURST", 200)
	cfg.ConflictCandidateLimit, errs = collectInt(errs, "AKASHI_CONFLICT_CANDIDATE_LIMIT", 20)
	cfg.ConflictBackfillWorkers, errs = collectInt(errs, "AKASHI_CONFLICT_BACKFILL_WORKERS", 4)
	defaultLLMThreads := max(1, runtime.NumCPU()/3)
	cfg.ConflictLLMThreads, errs = collectInt(errs, "AKASHI_CONFLICT_LLM_THREADS", defaultLLMThreads)
	cfg.WALSegmentSize, errs = collectInt(errs, "AKASHI_WAL_SEGMENT_SIZE", 64*1024*1024)
	cfg.WALSegmentRecords, errs = collectInt(errs, "AKASHI_WAL_SEGMENT_RECORDS", 100_000)

	var dbMaxConns int
	dbMaxConns, errs = collectInt(errs, "AKASHI_DB_MAX_CONNS", 0)
	if dbMaxConns > math.MaxInt32 {
		errs = append(errs, fmt.Errorf("config: AKASHI_DB_MAX_CONNS %d exceeds max (%d)", dbMaxConns, math.MaxInt32))
	} else {
		cfg.DBMaxConns = int32(dbMaxConns) //nolint:gosec // bounds checked above
	}
	var dbMinConns int
	dbMinConns, errs = collectInt(errs, "AKASHI_DB_MIN_CONNS", 0)
	if dbMinConns > math.MaxInt32 {
		errs = append(errs, fmt.Errorf("config: AKASHI_DB_MIN_CONNS %d exceeds max (%d)", dbMinConns, math.MaxInt32))
	} else {
		cfg.DBMinConns = int32(dbMinConns) //nolint:gosec // bounds checked above
	}

	var maxReqBody int
	maxReqBody, errs = collectInt(errs, "AKASHI_MAX_REQUEST_BODY_BYTES", 1*1024*1024)
	cfg.MaxRequestBodyBytes = int64(maxReqBody)

	// Float fields.
	cfg.RateLimitRPS, errs = collectFloat64(errs, "AKASHI_RATE_LIMIT_RPS", 100.0)
	// Load the conflict profile first to get profile defaults, then overlay
	// individual env var overrides. This ensures explicit env vars always win.
	cfg.ConflictProfile = envStr("AKASHI_CONFLICT_PROFILE", "balanced")

	// Resolve embedding model profile for threshold selection. Explicit override
	// takes priority; otherwise auto-detect from provider config.
	cfg.EmbeddingModelProfile = envStr("AKASHI_EMBEDDING_MODEL_PROFILE", "")
	if cfg.EmbeddingModelProfile == "" {
		switch cfg.EmbeddingProvider {
		case "ollama":
			cfg.EmbeddingModelProfile = cfg.OllamaModel
		case "openai":
			cfg.EmbeddingModelProfile = cfg.EmbeddingModel
		default: // "auto" — ollama is tried first
			cfg.EmbeddingModelProfile = cfg.OllamaModel
		}
	}

	profileDefaults := conflictProfileDefaults(cfg.ConflictProfile, cfg.EmbeddingModelProfile)

	cfg.ConflictSignificanceThreshold, errs = collectFloat64(errs, "AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD", profileDefaults.significanceThreshold)
	cfg.ConflictDecayLambda, errs = collectFloat64(errs, "AKASHI_CONFLICT_DECAY_LAMBDA", profileDefaults.decayLambda)
	cfg.ConflictClaimTopicSimFloor, errs = collectFloat64(errs, "AKASHI_CONFLICT_CLAIM_TOPIC_SIM_FLOOR", profileDefaults.claimTopicSimFloor)
	cfg.ConflictClaimDivFloor, errs = collectFloat64(errs, "AKASHI_CONFLICT_CLAIM_DIV_FLOOR", profileDefaults.claimDivFloor)
	cfg.ConflictDecisionTopicSimFloor, errs = collectFloat64(errs, "AKASHI_CONFLICT_DECISION_TOPIC_SIM_FLOOR", profileDefaults.decisionTopicSimFloor)
	cfg.ConflictEarlyExitFloor, errs = collectFloat64(errs, "AKASHI_CONFLICT_EARLY_EXIT_FLOOR", profileDefaults.earlyExitFloor)
	cfg.ConflictOutcomeSimFloor, errs = collectFloat64(errs, "AKASHI_CONFLICT_OUTCOME_SIM_FLOOR", profileDefaults.outcomeSimFloor)
	cfg.CrossEncoderThreshold, errs = collectFloat64(errs, "AKASHI_CONFLICT_CROSS_ENCODER_THRESHOLD", profileDefaults.crossEncoderThreshold)
	var highConfThreshF64 float64
	highConfThreshF64, errs = collectFloat64(errs, "AKASHI_HIGH_CONFIDENCE_WARN_THRESHOLD", 0.85)
	cfg.HighConfidenceWarnThreshold = float32(highConfThreshF64)

	// Boolean fields.
	cfg.RateLimitEnabled, errs = collectBool(errs, "AKASHI_RATE_LIMIT_ENABLED", true)
	cfg.TrustProxy, errs = collectBool(errs, "AKASHI_TRUST_PROXY", false)
	cfg.OTELInsecure, errs = collectBool(errs, "OTEL_EXPORTER_OTLP_INSECURE", false)
	cfg.SkipEmbeddedMigrations, errs = collectBool(errs, "AKASHI_SKIP_EMBEDDED_MIGRATIONS", false)
	cfg.EnableDestructiveDelete, errs = collectBool(errs, "AKASHI_ENABLE_DESTRUCTIVE_DELETE", false)
	cfg.WALDisable, errs = collectBool(errs, "AKASHI_WAL_DISABLE", false)
	cfg.ClaimExtractionLLM, errs = collectBool(errs, "AKASHI_CLAIM_EXTRACTION_LLM", false)
	cfg.ForceConflictRescore, errs = collectBool(errs, "AKASHI_FORCE_CONFLICT_RESCORE", false)
	cfg.SignupEnabled, errs = collectBool(errs, "AKASHI_SIGNUP_ENABLED", false)
	cfg.HooksEnabled, errs = collectBool(errs, "AKASHI_HOOKS_ENABLED", true)
	cfg.AutoTrace, errs = collectBool(errs, "AKASHI_AUTO_TRACE", true)

	// Duration fields.
	cfg.ReadTimeout, errs = collectDuration(errs, "AKASHI_READ_TIMEOUT", 30*time.Second)
	cfg.WriteTimeout, errs = collectDuration(errs, "AKASHI_WRITE_TIMEOUT", 30*time.Second)
	cfg.JWTExpiration, errs = collectDuration(errs, "AKASHI_JWT_EXPIRATION", 24*time.Hour)
	cfg.OutboxPollInterval, errs = collectDuration(errs, "AKASHI_OUTBOX_POLL_INTERVAL", 1*time.Second)
	cfg.ConflictRefreshInterval, errs = collectDuration(errs, "AKASHI_CONFLICT_REFRESH_INTERVAL", 30*time.Second)
	cfg.IntegrityProofInterval, errs = collectDuration(errs, "AKASHI_INTEGRITY_PROOF_INTERVAL", 5*time.Minute)
	cfg.IntegrityAuditInterval, errs = collectDuration(errs, "AKASHI_INTEGRITY_AUDIT_INTERVAL", 15*time.Minute)
	cfg.IntegrityAuditTimeout, errs = collectDuration(errs, "AKASHI_INTEGRITY_AUDIT_TIMEOUT", 5*time.Minute)
	cfg.IntegrityFullAuditInterval, errs = collectDuration(errs, "AKASHI_INTEGRITY_FULL_AUDIT_INTERVAL", 24*time.Hour)
	cfg.IntegrityFullAuditProofs, errs = collectInt(errs, "AKASHI_INTEGRITY_FULL_AUDIT_PROOFS", 50)
	cfg.EventFlushTimeout, errs = collectDuration(errs, "AKASHI_EVENT_FLUSH_TIMEOUT", 100*time.Millisecond)
	cfg.WALSyncInterval, errs = collectDuration(errs, "AKASHI_WAL_SYNC_INTERVAL", 10*time.Millisecond)
	cfg.ShutdownHTTPTimeout, errs = collectDuration(errs, "AKASHI_SHUTDOWN_HTTP_TIMEOUT", 10*time.Second)
	cfg.ShutdownAsyncDrainTimeout, errs = collectDuration(errs, "AKASHI_SHUTDOWN_ASYNC_DRAIN_TIMEOUT", 30*time.Second)
	cfg.ShutdownBufferDrainTimeout, errs = collectDuration(errs, "AKASHI_SHUTDOWN_BUFFER_DRAIN_TIMEOUT", 30*time.Second)
	cfg.ShutdownOutboxDrainTimeout, errs = collectDuration(errs, "AKASHI_SHUTDOWN_OUTBOX_DRAIN_TIMEOUT", 0)
	cfg.ShutdownLoopDrainTimeout, errs = collectDuration(errs, "AKASHI_SHUTDOWN_LOOP_DRAIN_TIMEOUT", 10*time.Second)
	cfg.IdempotencyCleanupInterval, errs = collectDuration(errs, "AKASHI_IDEMPOTENCY_CLEANUP_INTERVAL", time.Hour)
	cfg.IdempotencyCompletedTTL, errs = collectDuration(errs, "AKASHI_IDEMPOTENCY_COMPLETED_TTL", 7*24*time.Hour)
	cfg.IdempotencyAbandonedTTL, errs = collectDuration(errs, "AKASHI_IDEMPOTENCY_ABANDONED_TTL", 24*time.Hour)
	cfg.RetentionInterval, errs = collectDuration(errs, "AKASHI_RETENTION_INTERVAL", 24*time.Hour)
	cfg.ClaimRetryInterval, errs = collectDuration(errs, "AKASHI_CLAIM_RETRY_INTERVAL", 2*time.Minute)
	cfg.PercentileRefreshInterval, errs = collectDuration(errs, "AKASHI_PERCENTILE_REFRESH_INTERVAL", 1*time.Hour)
	cfg.AutoResolveInterval, errs = collectDuration(errs, "AKASHI_AUTO_RESOLVE_INTERVAL", 1*time.Hour)

	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return Config{}, fmt.Errorf("config: invalid environment variables:\n  %s", strings.Join(msgs, "\n  "))
	}

	// WAL disable overrides any configured WAL directory.
	if cfg.WALDisable {
		cfg.WALDir = ""
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
	if c.DBMaxConns < 0 {
		errs = append(errs, errors.New("config: AKASHI_DB_MAX_CONNS must be >= 0"))
	}
	if c.DBMinConns < 0 {
		errs = append(errs, errors.New("config: AKASHI_DB_MIN_CONNS must be >= 0"))
	}
	if c.DBMinConns > 0 && c.DBMaxConns == 0 {
		errs = append(errs, errors.New("config: AKASHI_DB_MAX_CONNS must be set explicitly when AKASHI_DB_MIN_CONNS is set"))
	}
	if c.DBMaxConns > 0 && c.DBMinConns > c.DBMaxConns {
		errs = append(errs, errors.New("config: AKASHI_DB_MIN_CONNS must not exceed AKASHI_DB_MAX_CONNS"))
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
	if c.ShutdownHTTPTimeout < 0 {
		errs = append(errs, errors.New("config: AKASHI_SHUTDOWN_HTTP_TIMEOUT must be >= 0"))
	}
	if c.ShutdownAsyncDrainTimeout < 0 {
		errs = append(errs, errors.New("config: AKASHI_SHUTDOWN_ASYNC_DRAIN_TIMEOUT must be >= 0"))
	}
	if c.ShutdownBufferDrainTimeout < 0 {
		errs = append(errs, errors.New("config: AKASHI_SHUTDOWN_BUFFER_DRAIN_TIMEOUT must be >= 0"))
	}
	if c.ShutdownOutboxDrainTimeout < 0 {
		errs = append(errs, errors.New("config: AKASHI_SHUTDOWN_OUTBOX_DRAIN_TIMEOUT must be >= 0"))
	}
	if c.ShutdownLoopDrainTimeout < 0 {
		errs = append(errs, errors.New("config: AKASHI_SHUTDOWN_LOOP_DRAIN_TIMEOUT must be >= 0"))
	}
	if c.IdempotencyCleanupInterval <= 0 {
		errs = append(errs, errors.New("config: AKASHI_IDEMPOTENCY_CLEANUP_INTERVAL must be positive"))
	}
	if c.IdempotencyCompletedTTL <= 0 {
		errs = append(errs, errors.New("config: AKASHI_IDEMPOTENCY_COMPLETED_TTL must be positive"))
	}
	if c.IdempotencyAbandonedTTL <= 0 {
		errs = append(errs, errors.New("config: AKASHI_IDEMPOTENCY_ABANDONED_TTL must be positive"))
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
	if c.IntegrityAuditInterval <= 0 {
		errs = append(errs, errors.New("config: AKASHI_INTEGRITY_AUDIT_INTERVAL must be positive"))
	}
	if c.IntegrityAuditTimeout <= 0 {
		errs = append(errs, errors.New("config: AKASHI_INTEGRITY_AUDIT_TIMEOUT must be positive"))
	}
	if c.IntegrityFullAuditInterval < 0 {
		errs = append(errs, errors.New("config: AKASHI_INTEGRITY_FULL_AUDIT_INTERVAL must be >= 0 (0 disables)"))
	}
	if c.IntegrityFullAuditProofs <= 0 {
		errs = append(errs, errors.New("config: AKASHI_INTEGRITY_FULL_AUDIT_PROOFS must be positive"))
	}
	if c.RateLimitEnabled {
		if c.RateLimitRPS <= 0 {
			errs = append(errs, errors.New("config: AKASHI_RATE_LIMIT_RPS must be positive when rate limiting is enabled"))
		}
		if c.RateLimitBurst <= 0 {
			errs = append(errs, errors.New("config: AKASHI_RATE_LIMIT_BURST must be positive when rate limiting is enabled"))
		}
	}
	// Early-exit floor must be non-negative (0 disables) and must not exceed
	// the significance threshold, otherwise early exit prunes candidates that
	// would pass the threshold check.
	if c.ConflictEarlyExitFloor < 0 {
		errs = append(errs, errors.New("config: AKASHI_CONFLICT_EARLY_EXIT_FLOOR must be >= 0 (0 disables early exit)"))
	}
	if c.ConflictEarlyExitFloor > 0 && c.ConflictEarlyExitFloor > c.ConflictSignificanceThreshold {
		errs = append(errs, fmt.Errorf("config: AKASHI_CONFLICT_EARLY_EXIT_FLOOR (%.2f) must not exceed AKASHI_CONFLICT_SIGNIFICANCE_THRESHOLD (%.2f)",
			c.ConflictEarlyExitFloor, c.ConflictSignificanceThreshold))
	}
	if c.ConflictOutcomeSimFloor < 0 || c.ConflictOutcomeSimFloor > 1 {
		errs = append(errs, errors.New("config: AKASHI_CONFLICT_OUTCOME_SIM_FLOOR must be between 0.0 and 1.0 (0 disables)"))
	}

	// WAL fail-safe: refuse to start without WAL unless explicitly disabled.
	// The envStr helper prevents AKASHI_WAL_DIR="" from clearing the default,
	// but this catches programmatic Config construction or future refactors.
	if c.WALDir == "" && !c.WALDisable {
		errs = append(errs, errors.New("config: WAL is required for crash durability; set AKASHI_WAL_DIR or set AKASHI_WAL_DISABLE=true to explicitly accept data loss risk"))
	}

	// JWT keys must be both set or both empty (ephemeral mode). Mismatched config
	// would cause token validation to fail for all clients.
	privSet := c.JWTPrivateKeyPath != ""
	pubSet := c.JWTPublicKeyPath != ""
	if privSet != pubSet {
		errs = append(errs, errors.New("config: AKASHI_JWT_PRIVATE_KEY and AKASHI_JWT_PUBLIC_KEY must both be set or both be empty"))
	}
	if privSet {
		if err := validateKeyFile(c.JWTPrivateKeyPath, "AKASHI_JWT_PRIVATE_KEY"); err != nil {
			errs = append(errs, err)
		}
	}
	if pubSet {
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

func envFloat64(key string, fallback float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid float", key, v)
	}
	return f, nil
}

// collectFloat64 parses a float64 env var, appending any error to the accumulator.
func collectFloat64(errs []error, key string, fallback float64) (float64, []error) {
	v, err := envFloat64(key, fallback)
	if err != nil {
		errs = append(errs, err)
	}
	return v, errs
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

// conflictProfileValues holds the default threshold values for a conflict
// detection profile. Individual env var overrides always take precedence.
type conflictProfileValues struct {
	significanceThreshold float64
	decayLambda           float64
	claimTopicSimFloor    float64
	claimDivFloor         float64
	decisionTopicSimFloor float64
	earlyExitFloor        float64
	outcomeSimFloor       float64
	crossEncoderThreshold float64
}

// embeddingModelThresholds holds base scorer thresholds calibrated for a
// specific embedding model's similarity distribution. These are base values
// before detection profile adjustments are applied.
type embeddingModelThresholds struct {
	claimTopicSimFloor    float64
	claimDivFloor         float64
	decisionTopicSimFloor float64
}

// embeddingModelProfiles maps embedding model names to calibrated thresholds.
// The mxbai-embed-large profile matches the original hardcoded values, ensuring
// zero behavioral change for existing deployments. Other profiles are populated
// from benchmark results. Unknown models fall back to mxbai-embed-large.
var embeddingModelProfiles = map[string]embeddingModelThresholds{
	"mxbai-embed-large": {
		claimTopicSimFloor:    0.60,
		claimDivFloor:         0.15,
		decisionTopicSimFloor: 0.70,
	},
	"nomic-embed-text": {
		claimTopicSimFloor:    0.55,
		claimDivFloor:         0.12,
		decisionTopicSimFloor: 0.65,
	},
	"bge-large-en-v1.5": {
		claimTopicSimFloor:    0.58,
		claimDivFloor:         0.14,
		decisionTopicSimFloor: 0.68,
	},
	"text-embedding-3-small": {
		claimTopicSimFloor:    0.52,
		claimDivFloor:         0.12,
		decisionTopicSimFloor: 0.62,
	},
	"text-embedding-3-large": {
		claimTopicSimFloor:    0.55,
		claimDivFloor:         0.13,
		decisionTopicSimFloor: 0.65,
	},
}

// EmbeddingModelThresholds returns the calibrated thresholds for a model name.
// Returns defaults (mxbai-embed-large) for unknown models and a boolean
// indicating whether a known profile was found.
func EmbeddingModelThresholds(modelName string) (claimTopicSimFloor, claimDivFloor, decisionTopicSimFloor float64, known bool) {
	if t, ok := embeddingModelProfiles[modelName]; ok {
		return t.claimTopicSimFloor, t.claimDivFloor, t.decisionTopicSimFloor, true
	}
	def := embeddingModelProfiles["mxbai-embed-large"]
	return def.claimTopicSimFloor, def.claimDivFloor, def.decisionTopicSimFloor, false
}

// detectionProfileAdjustments defines relative adjustments to model-specific
// base thresholds for each detection aggressiveness profile.
type detectionProfileAdjustments struct {
	significanceThreshold float64 // absolute value (not model-dependent)
	decayLambda           float64 // absolute value
	claimTopicSimDelta    float64 // added to model base
	claimDivDelta         float64 // added to model base
	decisionTopicSimDelta float64 // added to model base
	earlyExitFloor        float64 // absolute value
	outcomeSimFloor       float64 // absolute value
	crossEncoderThreshold float64 // absolute value
}

// conflictProfileDefaults returns threshold defaults for the named detection
// profile, adjusted for the specified embedding model. The model provides
// base thresholds for similarity-dependent settings; the detection profile
// provides relative adjustments for aggressiveness. Unknown model names fall
// back to mxbai-embed-large. Unrecognized profile names fall back to "balanced".
func conflictProfileDefaults(profile string, embeddingModel string) conflictProfileValues {
	// Look up model base thresholds.
	modelBase, ok := embeddingModelProfiles[embeddingModel]
	if !ok {
		modelBase = embeddingModelProfiles["mxbai-embed-large"]
	}

	// Get detection profile adjustments.
	var adj detectionProfileAdjustments
	switch strings.ToLower(profile) {
	case "high_precision":
		adj = detectionProfileAdjustments{
			significanceThreshold: 0.40,
			decayLambda:           0.01,
			claimTopicSimDelta:    +0.05,
			claimDivDelta:         +0.05,
			decisionTopicSimDelta: +0.05,
			earlyExitFloor:        0.35,
			outcomeSimFloor:       0.80,
			crossEncoderThreshold: 0.60,
		}
	case "high_recall":
		adj = detectionProfileAdjustments{
			significanceThreshold: 0.20,
			decayLambda:           0.005,
			claimTopicSimDelta:    -0.05,
			claimDivDelta:         -0.05,
			decisionTopicSimDelta: -0.05,
			earlyExitFloor:        0.15,
			outcomeSimFloor:       0.90,
			crossEncoderThreshold: 0.35,
		}
	default: // "balanced"
		adj = detectionProfileAdjustments{
			significanceThreshold: 0.30,
			decayLambda:           0.01,
			claimTopicSimDelta:    0,
			claimDivDelta:         0,
			decisionTopicSimDelta: 0,
			earlyExitFloor:        0.25,
			outcomeSimFloor:       0.85,
			crossEncoderThreshold: 0.50,
		}
	}

	return conflictProfileValues{
		significanceThreshold: adj.significanceThreshold,
		decayLambda:           adj.decayLambda,
		claimTopicSimFloor:    modelBase.claimTopicSimFloor + adj.claimTopicSimDelta,
		claimDivFloor:         modelBase.claimDivFloor + adj.claimDivDelta,
		decisionTopicSimFloor: modelBase.decisionTopicSimFloor + adj.decisionTopicSimDelta,
		earlyExitFloor:        adj.earlyExitFloor,
		outcomeSimFloor:       adj.outcomeSimFloor,
		crossEncoderThreshold: adj.crossEncoderThreshold,
	}
}

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
