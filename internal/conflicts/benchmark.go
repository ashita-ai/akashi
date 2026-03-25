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

	// --- FP-category pairs: targeting the 6 root causes of false positives ---
	// These cover the specific patterns identified in issue #534.

	fpCategoryPairs := []BenchmarkPair{
		// Supersession chains: sequential refinements of the same parameter
		{
			TopicA: "request timeout configuration", TopicB: "request timeout configuration",
			OutcomeA: "set HTTP request timeout to 30 seconds based on initial load testing",
			OutcomeB: "increased HTTP request timeout to 45 seconds after observing p99 latency spikes under peak load",
		},
		{
			TopicA: "connection pool sizing", TopicB: "connection pool sizing",
			OutcomeA: "configured database connection pool to 10 connections based on development workload",
			OutcomeB: "tuned database connection pool to 25 connections after production traffic analysis showed pool exhaustion",
		},
		{
			TopicA: "rate limit configuration", TopicB: "rate limit configuration",
			OutcomeA: "set rate limit to 50 requests per second per client",
			OutcomeB: "adjusted rate limit to 100 requests per second after customer feedback about throttling during peak hours",
		},
		{
			TopicA: "retry backoff configuration", TopicB: "retry backoff configuration",
			OutcomeA: "configured exponential backoff with base delay 100ms and max 3 retries",
			OutcomeB: "refined retry policy to base delay 200ms with max 5 retries after observing transient failure patterns",
		},
		{
			TopicA: "log level configuration", TopicB: "log level configuration",
			OutcomeA: "set production log level to INFO for balanced observability",
			OutcomeB: "changed production log level to WARN after log volume exceeded storage budget at INFO level",
		},
		// Complementary outcomes: different tools for different aspects
		{
			TopicA: "performance optimization", TopicB: "performance optimization",
			OutcomeA: "added Redis caching for frequently accessed user profiles to reduce database load",
			OutcomeB: "added PostgreSQL indexes on user_id and created_at to speed up historical queries",
		},
		{
			TopicA: "security hardening", TopicB: "security hardening",
			OutcomeA: "implemented rate limiting on authentication endpoints to prevent brute force attacks",
			OutcomeB: "added Content-Security-Policy headers and CSRF tokens to prevent cross-site attacks",
		},
		{
			TopicA: "monitoring setup", TopicB: "monitoring setup",
			OutcomeA: "configured Prometheus metrics collection for API latency and error rates",
			OutcomeB: "set up structured logging with correlation IDs for distributed tracing across services",
		},
		{
			TopicA: "API versioning strategy", TopicB: "API versioning strategy",
			OutcomeA: "added v2 prefix to new endpoints while maintaining v1 backward compatibility",
			OutcomeB: "implemented content negotiation via Accept headers for version selection on existing endpoints",
		},
		{
			TopicA: "data validation improvements", TopicB: "data validation improvements",
			OutcomeA: "added JSON schema validation for all incoming API request bodies",
			OutcomeB: "implemented database-level CHECK constraints for data integrity on critical columns",
		},
		// Review/implementation pairs: finding followed by fix
		{
			TopicA: "error handling audit", TopicB: "error handling fixes",
			OutcomeA: "code review found 12 instances of swallowed errors in the payment processing module",
			OutcomeB: "fixed all swallowed errors in payment module by adding proper error propagation and logging",
		},
		{
			TopicA: "SQL injection assessment", TopicB: "SQL injection remediation",
			OutcomeA: "security audit identified 3 SQL injection vulnerabilities in the search endpoint",
			OutcomeB: "remediated SQL injection vulnerabilities by switching to parameterized queries in search handlers",
		},
		{
			TopicA: "memory leak investigation", TopicB: "memory leak fix",
			OutcomeA: "profiling revealed goroutine leak in WebSocket handler due to missing connection cleanup",
			OutcomeB: "fixed goroutine leak by adding deferred connection close and context cancellation to WebSocket handler",
		},
		{
			TopicA: "test coverage analysis", TopicB: "test coverage improvement",
			OutcomeA: "coverage analysis shows auth module at 45% — missing tests for token refresh and session expiry",
			OutcomeB: "added comprehensive tests for token refresh and session expiry flows, auth module now at 89% coverage",
		},
		{
			TopicA: "dependency audit", TopicB: "dependency update",
			OutcomeA: "audit found 4 dependencies with known CVEs including critical vulnerability in XML parser",
			OutcomeB: "updated all flagged dependencies to patched versions and verified no breaking API changes",
		},
		// Workflow steps: sequential pipeline stages
		{
			TopicA: "user notification system", TopicB: "user notification system",
			OutcomeA: "designed notification service API schema with endpoints for preferences, templates, and delivery",
			OutcomeB: "implemented notification service handlers and storage layer following the approved API schema",
		},
		{
			TopicA: "data migration project", TopicB: "data migration project",
			OutcomeA: "completed data mapping analysis: 47 tables need migration with 12 schema transformations identified",
			OutcomeB: "wrote and tested migration scripts for all 47 tables including the 12 schema transformations",
		},
		{
			TopicA: "feature flag system", TopicB: "feature flag system",
			OutcomeA: "evaluated LaunchDarkly vs Unleash vs custom solution — recommending Unleash for self-hosted control",
			OutcomeB: "deployed Unleash instance and integrated SDK into all backend services with gradual rollout support",
		},
		{
			TopicA: "load testing infrastructure", TopicB: "load testing infrastructure",
			OutcomeA: "created k6 load test scripts simulating realistic user journeys across 5 critical API paths",
			OutcomeB: "executed load tests and identified 3 bottlenecks: connection pool exhaustion, N+1 queries, and slow serialization",
		},
		{
			TopicA: "CI/CD pipeline redesign", TopicB: "CI/CD pipeline redesign",
			OutcomeA: "designed new pipeline architecture with parallel test stages and artifact caching for 60% faster builds",
			OutcomeB: "implemented the new pipeline in GitHub Actions with matrix builds, caching, and deployment gates",
		},
		// Scoping differences: different granularity levels
		{
			TopicA: "system architecture", TopicB: "service-level design",
			OutcomeA: "adopted microservices architecture with event-driven communication between bounded contexts",
			OutcomeB: "chose REST with synchronous calls for the user-service API since it has simple CRUD semantics",
		},
		{
			TopicA: "database strategy", TopicB: "table design",
			OutcomeA: "selected PostgreSQL as the primary data store across all services for consistency and ecosystem",
			OutcomeB: "designed the audit_logs table with BRIN index on timestamp for efficient time-range queries",
		},
		{
			TopicA: "deployment approach", TopicB: "service configuration",
			OutcomeA: "standardized on Kubernetes for all production deployments with Helm charts for configuration",
			OutcomeB: "configured the payment service with 3 replicas, 512MB memory limit, and 200m CPU request",
		},
		{
			TopicA: "testing philosophy", TopicB: "test implementation",
			OutcomeA: "established testing pyramid: 70% unit, 20% integration, 10% e2e with mandatory coverage thresholds",
			OutcomeB: "wrote integration tests for the checkout flow using testcontainers with real PostgreSQL and Redis",
		},
		{
			TopicA: "observability platform", TopicB: "service instrumentation",
			OutcomeA: "chose OpenTelemetry as the unified observability framework for traces, metrics, and logs",
			OutcomeB: "added custom span attributes for order_id and user_id to the payment processing trace",
		},
		// Temporal context: decisions correct at different points in time
		{
			TopicA: "API client library", TopicB: "API client library",
			OutcomeA: "using Stripe API v2022-11-15 which has stable payment intent flow for our use case",
			OutcomeB: "migrating to Stripe API v2024-04-10 for improved 3D Secure handling required by new EU regulations",
		},
		{
			TopicA: "container base image", TopicB: "container base image",
			OutcomeA: "using Alpine 3.17 as base image for minimal attack surface and small image size",
			OutcomeB: "upgraded to Alpine 3.19 to pick up OpenSSL security patches and glibc compatibility fixes",
		},
		{
			TopicA: "framework version", TopicB: "framework version",
			OutcomeA: "pinned to React 17 for compatibility with our enzyme test suite and class component patterns",
			OutcomeB: "migrated to React 19 after completing test suite migration to React Testing Library",
		},
		{
			TopicA: "encryption standard", TopicB: "encryption standard",
			OutcomeA: "using AES-128-GCM for field-level encryption which meets current compliance requirements",
			OutcomeB: "upgrading to AES-256-GCM after updated compliance audit required 256-bit minimum key length",
		},
		{
			TopicA: "authentication protocol", TopicB: "authentication protocol",
			OutcomeA: "implemented OAuth 2.0 implicit flow for single-page application authentication",
			OutcomeB: "migrated from implicit flow to authorization code with PKCE after OAuth 2.1 deprecated implicit grant",
		},
	}
	for i := range fpCategoryPairs {
		fpCategoryPairs[i].Label = "related_not_contradicting"
	}
	pairs = append(pairs, fpCategoryPairs...)

	return pairs
}
