package akashi

import (
	"io/fs"
	"log/slog"
)

// Option configures an App.
type Option func(*resolvedOptions)

// resolvedOptions holds all extension points after applying defaults.
// Unexported — callers use the With* functions.
type resolvedOptions struct {
	port              int
	databaseURL       string
	notifyURL         string
	logger            *slog.Logger
	version           string
	embeddingProvider EmbeddingProvider
	searcher          Searcher
	conflictScorer    ConflictScorer
	eventHooks        []EventHook
	policyEvaluator   PolicyEvaluator
	routeRegistrars   []RouteRegistrar
	middlewares       []Middleware
	extraMigrations   []fs.FS
}

// WithPort overrides the TCP port from config (AKASHI_PORT env var).
func WithPort(port int) Option {
	return func(o *resolvedOptions) { o.port = port }
}

// WithDatabaseURL overrides the database connection string from config (DATABASE_URL env var).
func WithDatabaseURL(url string) Option {
	return func(o *resolvedOptions) { o.databaseURL = url }
}

// WithNotifyURL overrides the direct Postgres URL used for LISTEN/NOTIFY (NOTIFY_URL env var).
// Set this when using a connection pooler (e.g. PgBouncer) for queries — LISTEN/NOTIFY
// requires a direct (non-pooled) connection.
func WithNotifyURL(url string) Option {
	return func(o *resolvedOptions) { o.notifyURL = url }
}

// WithLogger sets the structured logger for the App.
// If not set, the default slog logger is used.
func WithLogger(logger *slog.Logger) Option {
	return func(o *resolvedOptions) { o.logger = logger }
}

// WithVersion sets the version string reported in the health endpoint and logs.
func WithVersion(version string) Option {
	return func(o *resolvedOptions) { o.version = version }
}

// WithEmbeddingProvider replaces the auto-detected embedding provider (Ollama/OpenAI/noop).
// The provided implementation must satisfy the EmbeddingProvider interface.
func WithEmbeddingProvider(p EmbeddingProvider) Option {
	return func(o *resolvedOptions) { o.embeddingProvider = p }
}

// WithSearcher replaces the auto-detected Qdrant vector search index.
// Candidate finding for conflict detection still uses the internal Qdrant index if configured;
// this override only applies to user-facing search (POST /v1/search, POST /v1/check).
func WithSearcher(s Searcher) Option {
	return func(o *resolvedOptions) { o.searcher = s }
}

// WithConflictScorer replaces the built-in pairwise conflict scorer.
// Only the last call wins — if multiple are registered, only the last takes effect.
// Candidate finding via Qdrant still runs in OSS; this replaces only the confirmation step.
func WithConflictScorer(cs ConflictScorer) Option {
	return func(o *resolvedOptions) { o.conflictScorer = cs }
}

// WithPolicyEvaluator sets the policy engine for advisory and enforcement checks.
// Only the last call wins. The policy evaluator is not wired to any call sites yet —
// this option reserves the extension point for a future spec.
func WithPolicyEvaluator(pe PolicyEvaluator) Option {
	return func(o *resolvedOptions) { o.policyEvaluator = pe }
}

// WithEventHook registers an event hook to receive decision lifecycle notifications.
// Multiple hooks may be registered; all registered hooks receive every event.
func WithEventHook(hook EventHook) Option {
	return func(o *resolvedOptions) { o.eventHooks = append(o.eventHooks, hook) }
}

// WithExtraRoutes registers additional routes on the shared HTTP mux.
// Multiple registrars may be registered; all are called in registration order.
func WithExtraRoutes(fn RouteRegistrar) Option {
	return func(o *resolvedOptions) { o.routeRegistrars = append(o.routeRegistrars, fn) }
}

// WithMiddleware registers an outermost HTTP middleware.
// Multiple middlewares may be registered. Applied in registration order:
// the first-registered middleware is outermost (called first by every request).
func WithMiddleware(mw Middleware) Option {
	return func(o *resolvedOptions) { o.middlewares = append(o.middlewares, mw) }
}

// WithExtraMigrations adds an additional SQL migration filesystem to run after OSS migrations.
// Multiple filesystems may be registered; they are applied in registration order.
// The FS must contain sequential SQL files compatible with the Atlas migration format.
func WithExtraMigrations(dir fs.FS) Option {
	return func(o *resolvedOptions) { o.extraMigrations = append(o.extraMigrations, dir) }
}
