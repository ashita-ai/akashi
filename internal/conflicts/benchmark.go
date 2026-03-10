//go:build !lite

package conflicts

// BenchmarkPair is a labeled text pair for benchmarking embedding models.
// Unlike the synthetic dataset in benchmark_test.go (which uses synthetic vectors),
// these pairs are meant to be embedded by a real model so we can measure how the
// model's similarity distribution interacts with scorer thresholds.
type BenchmarkPair struct {
	Label    string // "genuine", "related_not_contradicting", "unrelated_false_positive"
	TopicA   string // decision topic/context for side A
	TopicB   string // decision topic/context for side B
	OutcomeA string // outcome text for side A
	OutcomeB string // outcome text for side B
}

// BenchmarkDataset returns labeled text pairs for embedding model benchmarking.
// The pairs mirror the categories in the synthetic dataset but use natural language
// that a real embedding model can meaningfully encode.
func BenchmarkDataset() []BenchmarkPair {
	var pairs []BenchmarkPair

	// --- Genuine conflicts: same topic, opposite outcomes ---
	genuinePairs := []BenchmarkPair{
		{
			TopicA: "caching strategy for session data", TopicB: "caching strategy for session data",
			OutcomeA: "chose Redis for caching because it supports TTL natively and has pub/sub for invalidation",
			OutcomeB: "chose Memcached for caching because it has simpler memory management and better multi-threaded performance",
		},
		{
			TopicA: "primary database selection", TopicB: "primary database selection",
			OutcomeA: "use PostgreSQL as primary data store for strong consistency and JSONB support",
			OutcomeB: "use MongoDB as primary data store for flexible schema and horizontal scaling",
		},
		{
			TopicA: "deployment region decision", TopicB: "deployment region decision",
			OutcomeA: "deploy to us-east-1 for lowest latency to our US customer base and best AWS service availability",
			OutcomeB: "deploy to eu-west-1 for GDPR compliance and proximity to European customers",
		},
		{
			TopicA: "service architecture", TopicB: "service architecture",
			OutcomeA: "adopt microservices architecture to enable independent deployment and team autonomy",
			OutcomeB: "start with a monolith to reduce operational complexity and speed up initial development",
		},
		{
			TopicA: "inter-service communication protocol", TopicB: "inter-service communication protocol",
			OutcomeA: "use REST API for inter-service communication for simplicity and broad tooling support",
			OutcomeB: "use gRPC for inter-service communication for type safety and better performance",
		},
		{
			TopicA: "session storage approach", TopicB: "session storage approach",
			OutcomeA: "store sessions in Redis for fast lookups and automatic expiration",
			OutcomeB: "store sessions in the database for durability and simpler infrastructure",
		},
		{
			TopicA: "authentication mechanism", TopicB: "authentication mechanism",
			OutcomeA: "use JWT tokens for auth because they are stateless and work well with microservices",
			OutcomeB: "use session cookies for auth because they can be revoked immediately and are simpler to secure",
		},
		{
			TopicA: "data access pattern", TopicB: "data access pattern",
			OutcomeA: "implement CQRS pattern to separate read and write models for better scalability",
			OutcomeB: "keep single read-write model to reduce complexity and maintain data consistency",
		},
		{
			TopicA: "event streaming platform", TopicB: "event streaming platform",
			OutcomeA: "use Kafka for event streaming for high throughput and durable message retention",
			OutcomeB: "use RabbitMQ for message queuing for simpler operations and flexible routing",
		},
		{
			TopicA: "container orchestration", TopicB: "container orchestration",
			OutcomeA: "deploy on Kubernetes for declarative infrastructure and automatic scaling",
			OutcomeB: "deploy on bare metal with systemd for lower overhead and simpler debugging",
		},
	}
	for i := range genuinePairs {
		genuinePairs[i].Label = "genuine"
	}
	pairs = append(pairs, genuinePairs...)

	// --- Related but not contradicting: same topic, paraphrased outcomes ---
	relatedPairs := []BenchmarkPair{
		{
			TopicA: "caching layer implementation", TopicB: "caching layer implementation",
			OutcomeA: "added Redis caching layer with 5-minute TTL for frequently accessed queries",
			OutcomeB: "implemented Redis-based cache with 300-second expiration for hot query results",
		},
		{
			TopicA: "database performance optimization", TopicB: "database performance optimization",
			OutcomeA: "added PostgreSQL indexes for query performance on the users and orders tables",
			OutcomeB: "created database indexes to speed up queries against users and orders",
		},
		{
			TopicA: "service deployment", TopicB: "service deployment",
			OutcomeA: "deployed service to production with zero-downtime rolling update",
			OutcomeB: "rolled out service to prod environment using rolling deployment strategy",
		},
		{
			TopicA: "rate limiting configuration", TopicB: "rate limiting configuration",
			OutcomeA: "added rate limiting at 100 requests per second per client IP",
			OutcomeB: "implemented rate limit of 100 req/s using token bucket per IP address",
		},
		{
			TopicA: "TLS configuration", TopicB: "TLS configuration",
			OutcomeA: "enabled TLS 1.3 for all endpoints with strong cipher suites",
			OutcomeB: "configured TLS v1.3 across all services with modern cipher configuration",
		},
		{
			TopicA: "connection pool tuning", TopicB: "connection pool tuning",
			OutcomeA: "set database connection pool size to 20 based on load testing results",
			OutcomeB: "configured pool with 20 connections after benchmarking under production load",
		},
		{
			TopicA: "health check endpoint", TopicB: "health check endpoint",
			OutcomeA: "added health check endpoint at /health with readiness and liveness probes",
			OutcomeB: "implemented /health readiness probe that checks database and cache connectivity",
		},
		{
			TopicA: "response compression", TopicB: "response compression",
			OutcomeA: "enabled gzip compression for all HTTP responses over 1KB",
			OutcomeB: "turned on gzip for HTTP responses to reduce bandwidth for large payloads",
		},
		{
			TopicA: "request tracing", TopicB: "request tracing",
			OutcomeA: "added request ID middleware that generates a UUID for each incoming request",
			OutcomeB: "implemented request tracing via middleware that assigns unique IDs to requests",
		},
		{
			TopicA: "logging infrastructure", TopicB: "logging infrastructure",
			OutcomeA: "switched to structured JSON logging with log levels and correlation IDs",
			OutcomeB: "implemented structured logging in JSON format with severity levels and trace IDs",
		},
	}
	for i := range relatedPairs {
		relatedPairs[i].Label = "related_not_contradicting"
	}
	pairs = append(pairs, relatedPairs...)

	// --- Unrelated: different topics entirely ---
	unrelatedPairs := []BenchmarkPair{
		{
			TopicA: "caching strategy", TopicB: "database indexing",
			OutcomeA: "added Redis caching for session data with pub/sub invalidation",
			OutcomeB: "added PostgreSQL indexes on created_at and user_id columns",
		},
		{
			TopicA: "frontend framework", TopicB: "reverse proxy setup",
			OutcomeA: "chose React for frontend for component reuse and large ecosystem",
			OutcomeB: "configured Nginx reverse proxy with SSL termination and load balancing",
		},
		{
			TopicA: "user authentication", TopicB: "data export feature",
			OutcomeA: "implemented user authentication with OAuth 2.0 and PKCE flow",
			OutcomeB: "added CSV export feature for admin dashboard reporting",
		},
		{
			TopicA: "CI/CD pipeline", TopicB: "database schema design",
			OutcomeA: "set up CI/CD pipeline with GitHub Actions for automated testing and deployment",
			OutcomeB: "designed normalized database schema with proper foreign key constraints",
		},
		{
			TopicA: "logging middleware", TopicB: "payment processing",
			OutcomeA: "added structured logging middleware for HTTP request/response audit trail",
			OutcomeB: "implemented Stripe payment processing with webhook verification",
		},
		{
			TopicA: "DNS configuration", TopicB: "unit testing",
			OutcomeA: "configured DNS records with Route53 for automatic failover between regions",
			OutcomeB: "wrote comprehensive unit tests for the auth module with 95% coverage",
		},
		{
			TopicA: "monitoring and alerting", TopicB: "search functionality",
			OutcomeA: "set up Prometheus monitoring with Grafana dashboards and PagerDuty alerts",
			OutcomeB: "implemented full-text search using PostgreSQL tsvector with ranking",
		},
		{
			TopicA: "containerization", TopicB: "API rate limiting",
			OutcomeA: "added multi-stage Docker builds for smaller production images",
			OutcomeB: "designed API rate limiting with sliding window counters in Redis",
		},
		{
			TopicA: "load balancing", TopicB: "email notifications",
			OutcomeA: "configured AWS ALB with health checks and connection draining",
			OutcomeB: "implemented transactional email notifications via SES with template engine",
		},
		{
			TopicA: "real-time communication", TopicB: "database replication",
			OutcomeA: "added WebSocket support for real-time collaboration features",
			OutcomeB: "set up PostgreSQL streaming replication with automatic failover",
		},
	}
	for i := range unrelatedPairs {
		unrelatedPairs[i].Label = "unrelated_false_positive"
	}
	pairs = append(pairs, unrelatedPairs...)

	return pairs
}
