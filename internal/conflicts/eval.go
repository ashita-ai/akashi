//go:build !lite

package conflicts

import (
	"fmt"
	"strings"
	"time"
)

// EvalPair is a labeled decision pair for evaluating validator accuracy.
type EvalPair struct {
	Label                string        // Human-readable label for this pair.
	Input                ValidateInput // The input to the validator.
	ExpectedRelationship string        // Expected classification: contradiction, complementary, refinement, supersession, unrelated.
}

// EvalResult holds the outcome of evaluating a single pair.
type EvalResult struct {
	Label                string `json:"label"`
	ExpectedRelationship string `json:"expected_relationship"`
	ActualRelationship   string `json:"actual_relationship"`
	Correct              bool   `json:"correct"`
	ConflictExpected     bool   `json:"conflict_expected"`
	ConflictActual       bool   `json:"conflict_actual"`
	Explanation          string `json:"explanation"`
	Error                string `json:"error,omitempty"`
}

// EvalMetrics holds aggregate precision/recall/F1 metrics.
// ConflictPrec/ConflictRecall measure binary IsConflict() accuracy.
// RelationshipAcc measures exact 5-class relationship match rate.
type EvalMetrics struct {
	TotalPairs       int     `json:"total_pairs"`
	Errors           int     `json:"errors"`
	RelationshipAcc  float64 `json:"relationship_accuracy"`
	ConflictPrec     float64 `json:"conflict_precision"`
	ConflictRecall   float64 `json:"conflict_recall"`
	ConflictF1       float64 `json:"conflict_f1"`
	TruePositives    int     `json:"true_positives"`
	FalsePositives   int     `json:"false_positives"`
	TrueNegatives    int     `json:"true_negatives"`
	FalseNegatives   int     `json:"false_negatives"`
	RelationshipHits int     `json:"relationship_hits"`
}

// ComputeMetrics calculates precision, recall, F1, and accuracy from eval results.
func ComputeMetrics(results []EvalResult) EvalMetrics {
	m := EvalMetrics{TotalPairs: len(results)}

	for _, r := range results {
		if r.Error != "" {
			m.Errors++
			continue
		}
		if r.Correct {
			m.RelationshipHits++
		}
		switch {
		case r.ConflictExpected && r.ConflictActual:
			m.TruePositives++
		case !r.ConflictExpected && r.ConflictActual:
			m.FalsePositives++
		case !r.ConflictExpected && !r.ConflictActual:
			m.TrueNegatives++
		case r.ConflictExpected && !r.ConflictActual:
			m.FalseNegatives++
		}
	}

	evaluated := m.TotalPairs - m.Errors
	if evaluated > 0 {
		m.RelationshipAcc = float64(m.RelationshipHits) / float64(evaluated)
	}
	if m.TruePositives+m.FalsePositives > 0 {
		m.ConflictPrec = float64(m.TruePositives) / float64(m.TruePositives+m.FalsePositives)
	}
	if m.TruePositives+m.FalseNegatives > 0 {
		m.ConflictRecall = float64(m.TruePositives) / float64(m.TruePositives+m.FalseNegatives)
	}
	if m.ConflictPrec+m.ConflictRecall > 0 {
		m.ConflictF1 = 2 * m.ConflictPrec * m.ConflictRecall / (m.ConflictPrec + m.ConflictRecall)
	}
	return m
}

// FormatMetrics returns a human-readable summary of eval metrics.
func FormatMetrics(m EvalMetrics, results []EvalResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== Conflict Validator Eval ===\n")
	fmt.Fprintf(&b, "Pairs: %d  Errors: %d\n\n", m.TotalPairs, m.Errors)
	fmt.Fprintf(&b, "Conflict Detection (IsConflict binary):\n")
	fmt.Fprintf(&b, "  Precision: %.1f%%  (%d TP, %d FP)\n", m.ConflictPrec*100, m.TruePositives, m.FalsePositives)
	fmt.Fprintf(&b, "  Recall:    %.1f%%  (%d TP, %d FN)\n", m.ConflictRecall*100, m.TruePositives, m.FalseNegatives)
	fmt.Fprintf(&b, "  F1:        %.1f%%\n\n", m.ConflictF1*100)
	fmt.Fprintf(&b, "Relationship Accuracy: %.1f%%  (%d/%d exact matches)\n\n", m.RelationshipAcc*100, m.RelationshipHits, m.TotalPairs-m.Errors)

	// Per-pair detail for failures.
	var failures []EvalResult
	for _, r := range results {
		if !r.Correct && r.Error == "" {
			failures = append(failures, r)
		}
	}
	if len(failures) > 0 {
		fmt.Fprintf(&b, "--- Misclassifications (%d) ---\n", len(failures))
		for _, r := range failures {
			fmt.Fprintf(&b, "  [%s] expected=%s actual=%s  explanation=%q\n",
				r.Label, r.ExpectedRelationship, r.ActualRelationship, r.Explanation)
		}
	}
	return b.String()
}

// ScorerPrecisionRecall holds precision/recall/F1 for the full scorer pipeline eval.
type ScorerPrecisionRecall struct {
	TruePositives  int     // detected + labeled genuine
	FalsePositives int     // detected + labeled related_not_contradicting or unrelated_false_positive
	FalseNegatives int     // not detected + labeled genuine (missed conflicts)
	Precision      float64 // TP / (TP + FP)
	Recall         float64 // TP / (TP + FN)
	F1             float64 // 2 * (P * R) / (P + R)
}

// ScorerEvalResult pairs a decision pair with its expected and actual outcome.
type ScorerEvalResult struct {
	DecisionAOutcome string
	DecisionBOutcome string
	ExpectedLabel    string // "genuine", "related_not_contradicting", "unrelated_false_positive"
	Detected         bool   // whether the scorer produced a conflict for this pair
}

// ComputePrecisionRecall calculates precision, recall, and F1 from evaluation results.
// A "genuine" label is a positive; everything else is a negative.
func ComputePrecisionRecall(results []ScorerEvalResult) ScorerPrecisionRecall {
	var pr ScorerPrecisionRecall
	for _, r := range results {
		isPositive := r.ExpectedLabel == "genuine"
		switch {
		case r.Detected && isPositive:
			pr.TruePositives++
		case r.Detected && !isPositive:
			pr.FalsePositives++
		case !r.Detected && isPositive:
			pr.FalseNegatives++
		}
	}

	if pr.TruePositives+pr.FalsePositives > 0 {
		pr.Precision = float64(pr.TruePositives) / float64(pr.TruePositives+pr.FalsePositives)
	}
	if pr.TruePositives+pr.FalseNegatives > 0 {
		pr.Recall = float64(pr.TruePositives) / float64(pr.TruePositives+pr.FalseNegatives)
	}
	if pr.Precision+pr.Recall > 0 {
		pr.F1 = 2 * pr.Precision * pr.Recall / (pr.Precision + pr.Recall)
	}
	return pr
}

// DefaultEvalDataset returns the labeled evaluation dataset.
// Pairs are derived from real conflicts observed in production plus synthetic
// edge cases covering the major false positive patterns.
//
// Categories:
//   - genuine_conflict: True contradictions/supersessions that SHOULD be flagged.
//   - review_then_fix: Review identifies issues, fix resolves them (REFINEMENT/COMPLEMENTARY).
//   - different_scope: Same codebase but different review scopes (COMPLEMENTARY).
//   - agreement: Both recommend the same approach (COMPLEMENTARY).
//   - identify_then_improve: Problem identified, then improvements implemented (REFINEMENT).
//   - cross_project: Different projects entirely (UNRELATED).
func DefaultEvalDataset() []EvalPair {
	now := time.Now()
	h := time.Hour

	return []EvalPair{
		// =====================================================================
		// GENUINE CONFLICTS (should be flagged)
		// =====================================================================
		{
			Label: "genuine: opposite Qdrant startup strategy",
			Input: ValidateInput{
				OutcomeA: "Do NOT run Qdrant CreateFieldIndex on every startup — use explicit versioned migrations instead for all index changes",
				OutcomeB: "Always run Qdrant CreateFieldIndex on startup (not just at collection creation) to backfill indexes idempotently",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "reviewer",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: Redis vs Memcached for caching",
			Input: ValidateInput{
				OutcomeA: "Use Redis for the session caching layer — supports pub/sub for real-time invalidation",
				OutcomeB: "Use Memcached for session caching — simpler, faster, and we don't need pub/sub",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: REST vs gRPC API protocol",
			Input: ValidateInput{
				OutcomeA: "Use REST for all inter-service communication — simpler tooling and debugging",
				OutcomeB: "Use gRPC for inter-service communication — type safety and performance justify the complexity",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: nested vs flat agent_context",
			Input: ValidateInput{
				OutcomeA: "server/client namespace split is the right shape for agent_context",
				OutcomeB: "server/client namespace split is wrong; use flat structure with trust_source enum",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.85,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: supersession REST to gRPC",
			Input: ValidateInput{
				OutcomeA: "use REST v1 API for the gateway",
				OutcomeB: "replaced REST v1 with gRPC — REST was too slow for streaming use cases",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "planner",
				CreatedA: now, CreatedB: now.Add(72 * h),
				ProjectA: "gateway", ProjectB: "gateway",
			},
			ExpectedRelationship: "supersession",
		},

		// =====================================================================
		// REVIEW → FIX (false positive pattern: should NOT be flagged)
		// =====================================================================
		{
			Label: "review_then_fix: codebase review findings then bug fixes",
			Input: ValidateInput{
				OutcomeA: "Comprehensive staff-level codebase review of Akashi. Aggregate score 86/100. Three critical findings: (1) CreateDecision conditionally queues search_outbox (embedding != nil) while CreateTraceTx always queues — decisions without embeddings become permanently invisible to search. (2) ClearAllConflicts deletes scored_conflicts at startup with no audit trail. (3) WAL checkpoint tracks by event count rather than LSN.",
				OutcomeB: "Implemented 5 fixes from codebase review: (1) CreateDecision/ReviseDecision now unconditionally queue search_outbox — decisions without embeddings are no longer permanently invisible to search. (2) ClearAllConflicts/ClearUnvalidatedConflicts now log deletion counts. (3) WAL checkpoint now uses LSN-based tracking.",
				TypeA:    "code_review", TypeB: "bug_fix",
				AgentA: "coder", AgentB: "senior-engineer",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "review_then_fix: security audit then remediation",
			Input: ValidateInput{
				OutcomeA: "Security audit found 3 vulnerabilities: SQL injection in search handler, missing rate limiting on auth endpoint, API keys stored in plaintext logs",
				OutcomeB: "Fixed all 3 security vulnerabilities from audit: parameterized queries in search, rate limiter on auth, redacted API keys from logs",
				TypeA:    "audit", TypeB: "fix",
				AgentA: "security-auditor", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(4 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},

		// =====================================================================
		// IDENTIFY → IMPROVE (false positive pattern)
		// =====================================================================
		{
			Label: "identify_then_improve: naive pipeline then improvements",
			Input: ValidateInput{
				OutcomeA: "Identified conflict candidate pipeline (internal/conflicts/scorer.go) as the most naive part of Akashi: fetches 50 Qdrant candidates and scores all unconditionally with no early exit, no significance-based pruning, no adaptive candidate limits",
				OutcomeB: "Implemented issue #263: conflict candidate pipeline early exit and adaptive pruning. Pre-computes cheap significance for all candidates, sorts descending, and exits early when significance drops below floor",
				TypeA:    "assessment", TypeB: "implementation",
				AgentA: "coder", AgentB: "reviewer",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "identify_then_improve: performance bottleneck then optimization",
			Input: ValidateInput{
				OutcomeA: "Database query performance analysis: the ListConflicts query is doing a sequential scan on scored_conflicts because the composite index is missing org_id as the leading column",
				OutcomeB: "Added composite index (org_id, status, detected_at DESC) on scored_conflicts, reducing ListConflicts P99 from 450ms to 12ms",
				TypeA:    "analysis", TypeB: "bug_fix",
				AgentA: "analyst", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(3 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},

		// =====================================================================
		// DIFFERENT SCOPE (false positive pattern)
		// =====================================================================
		{
			Label: "different_scope: PR review clean vs codebase review with findings",
			Input: ValidateInput{
				OutcomeA: "Reviewed PR #342 (feat: outcome_score from assessments, anti-gaming for alternatives). Four parallel review agents found zero high-signal issues. Code is clean, well-tested, follows project conventions.",
				OutcomeB: "Full staff-level review complete. Scores: Correctness 85, Consistency 88, Readability 92. Found issues with retention batch delete missing org_id on dependent tables, WAL mutex contention under concurrent load, and saveCheckpoint missing directory fsync.",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "admin", AgentB: "senior-engineer",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "different_scope: module review vs full system review",
			Input: ValidateInput{
				OutcomeA: "Auth module review: all endpoints properly validate JWT tokens, rate limiting is correctly applied, no security issues found",
				OutcomeB: "System-wide security review: found 2 high-severity issues in the search module (unvalidated user input in Qdrant queries) and 1 medium issue in retention (missing org_id filter)",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "reviewer-a", AgentB: "reviewer-b",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},

		// =====================================================================
		// AGREEMENT (false positive pattern)
		// =====================================================================
		{
			Label: "agreement: both recommend ClickHouse",
			Input: ValidateInput{
				OutcomeA: "Use ClickHouse as the primary analytics store — columnar storage is necessary for the query patterns",
				OutcomeB: "Use ClickHouse as the core columnar database for your observability platform to handle 2TB/day ingestion with sub-second queries",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "data-engineer",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "analytics", ProjectB: "analytics",
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "agreement: both recommend PostgreSQL with different rationale",
			Input: ValidateInput{
				OutcomeA: "Use PostgreSQL for the decision store — ACID guarantees are critical for audit trail integrity",
				OutcomeB: "PostgreSQL is the right choice for decision storage — pgvector extension enables semantic search without a separate vector DB",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "data-engineer",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},

		// =====================================================================
		// CROSS-PROJECT (false positive pattern)
		// =====================================================================
		{
			Label: "cross_project: reviews of different codebases",
			Input: ValidateInput{
				OutcomeA: "Comprehensive review of Engram memory system: aggregate 67/100. Critical finding: memory consolidation has no TTL, leading to unbounded growth",
				OutcomeB: "Reviewed akashi OSS codebase: aggregate 78/100. The conflict detection pipeline is well-designed with proper two-stage validation",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "senior-staff-reviewer", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "engram", ProjectB: "akashi",
			},
			ExpectedRelationship: "unrelated",
		},
		{
			Label: "cross_project: architecture decisions for different services",
			Input: ValidateInput{
				OutcomeA: "Use event sourcing for the payment service — audit trail is a regulatory requirement",
				OutcomeB: "Use simple CRUD for the notification service — event sourcing adds unnecessary complexity here",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "planner",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "payment-service", ProjectB: "notification-service",
			},
			ExpectedRelationship: "unrelated",
		},

		// =====================================================================
		// REFINEMENT (should NOT be flagged as conflict)
		// =====================================================================
		{
			Label: "refinement: initial design then detailed implementation",
			Input: ValidateInput{
				OutcomeA: "Architecture decision: use a two-stage conflict detection pipeline with embedding similarity followed by LLM validation",
				OutcomeB: "Implemented the two-stage conflict pipeline: Qdrant for candidate retrieval (top-20 by cosine similarity), claim-level scoring for fine-grained analysis, then Ollama LLM for final validation with 5-class relationship output",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(72 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "refinement: same agent iterating on ReScore formula",
			Input: ValidateInput{
				OutcomeA: "Redesigned ReScore formula: assessment primary signal (0.40), logarithmic citations, conflict win rate, completeness reward",
				OutcomeB: "Implemented distribution-aware ReScore normalization: percentile-normalized citation counts via in-memory cache refreshed hourly, replacing arbitrary log saturation at 5",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				SessionIDA: "sess-1", SessionIDB: "sess-2",
			},
			ExpectedRelationship: "refinement",
		},

		// =====================================================================
		// SUBTLE CONTRADICTIONS (same technology, different configuration)
		// =====================================================================
		{
			Label: "subtle: Redis TTL 5min vs 24h",
			Input: ValidateInput{
				OutcomeA: "Set Redis session cache TTL to 5 minutes — short-lived sessions reduce stale data risk and memory usage",
				OutcomeB: "Set Redis session cache TTL to 24 hours — users shouldn't have to re-authenticate constantly, and cache hit rate matters more than memory",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "backend-eng", AgentB: "platform-eng",
				CreatedA: now, CreatedB: now.Add(4 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "subtle: PostgreSQL max_connections 100 vs 500",
			Input: ValidateInput{
				OutcomeA: "Configure PostgreSQL max_connections = 100 with PgBouncer in front — direct connections above 100 cause scheduler contention",
				OutcomeB: "Set PostgreSQL max_connections = 500 to handle peak connection burst from the 20 microservices — PgBouncer adds latency we can't afford",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "dba", AgentB: "backend-eng",
				CreatedA: now, CreatedB: now.Add(6 * h),
				ProjectA: "platform", ProjectB: "platform",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "subtle: JWT expiry 15min vs 4h",
			Input: ValidateInput{
				OutcomeA: "JWT access token expiry should be 15 minutes — short-lived tokens limit the blast radius of token theft",
				OutcomeB: "JWT access token expiry should be 4 hours — 15-minute tokens cause excessive refresh traffic and degrade UX on mobile",
				TypeA:    "security", TypeB: "architecture",
				AgentA: "security-eng", AgentB: "mobile-eng",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "auth-service", ProjectB: "auth-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "subtle: batch size 100 vs 10000",
			Input: ValidateInput{
				OutcomeA: "Process event ingestion in batches of 100 — keeps memory footprint predictable and latency under 50ms per batch",
				OutcomeB: "Use batch size of 10000 for event ingestion — smaller batches waste IOPS on transaction overhead and can't keep up with peak throughput",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "data-eng",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "subtle: log level INFO vs DEBUG in production",
			Input: ValidateInput{
				OutcomeA: "Production log level must be INFO — DEBUG logging in production generates 50GB/day and masks real issues in noise",
				OutcomeB: "Enable DEBUG logging in production for the decision pipeline — we can't diagnose conflict detection issues without it, use structured sampling to manage volume",
				TypeA:    "operations", TypeB: "operations",
				AgentA: "sre", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "subtle: connection pool 10 vs 50",
			Input: ValidateInput{
				OutcomeA: "Database connection pool size should be 10 per service instance — matches CPU core count and avoids connection starvation across the cluster",
				OutcomeB: "Database connection pool size should be 50 per service instance — our async workloads spend 80% of time waiting on I/O, so we need more connections than cores",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "dba", AgentB: "backend-eng",
				CreatedA: now, CreatedB: now.Add(3 * h),
				ProjectA: "platform", ProjectB: "platform",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "subtle: gRPC timeout 5s vs 30s",
			Input: ValidateInput{
				OutcomeA: "Set gRPC deadline to 5 seconds for inter-service calls — fail fast to prevent cascading timeouts",
				OutcomeB: "Set gRPC deadline to 30 seconds for inter-service calls — the embedding service regularly takes 10-15s for large payloads and 5s causes false failures",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "sre", AgentB: "ml-eng",
				CreatedA: now, CreatedB: now.Add(12 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "subtle: retry 3 vs 10 attempts",
			Input: ValidateInput{
				OutcomeA: "Maximum 3 retries with exponential backoff for failed API calls — more retries just amplify load on an already-struggling downstream",
				OutcomeB: "Allow up to 10 retries with jittered backoff for embedding API calls — the service has transient cold-start failures that resolve within 30s",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "sre", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(5 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},

		// =====================================================================
		// PARTIAL CONTRADICTIONS (agree on approach, disagree on detail)
		// =====================================================================
		{
			Label: "partial: both use Redis, cluster vs standalone",
			Input: ValidateInput{
				OutcomeA: "Use Redis Cluster with 6 nodes for the caching layer — we need horizontal scalability and automatic failover",
				OutcomeB: "Use a single Redis Sentinel setup for caching — cluster mode adds operational complexity we don't need at our scale, Sentinel gives us failover",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "platform-eng", AgentB: "sre",
				CreatedA: now, CreatedB: now.Add(8 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "partial: both use Postgres, disagree on partitioning",
			Input: ValidateInput{
				OutcomeA: "Partition the decisions table by created_at using monthly range partitions — time-based queries dominate and old months can be detached cheaply",
				OutcomeB: "Partition the decisions table by org_id using hash partitioning — multi-tenant isolation is the primary concern, time-based queries can use indexes",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "dba", AgentB: "backend-eng",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "partial: both use JWT, cookie vs localStorage",
			Input: ValidateInput{
				OutcomeA: "Store JWT tokens in HttpOnly Secure cookies — immune to XSS, automatic inclusion on requests",
				OutcomeB: "Store JWT tokens in localStorage with short expiry — cookies are vulnerable to CSRF and complicate the CORS setup for our multi-domain SPA",
				TypeA:    "security", TypeB: "architecture",
				AgentA: "security-eng", AgentB: "frontend-eng",
				CreatedA: now, CreatedB: now.Add(6 * h),
				ProjectA: "auth-service", ProjectB: "auth-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "partial: both cache, disagree on invalidation",
			Input: ValidateInput{
				OutcomeA: "Use cache-aside pattern with TTL-based expiration — simple, predictable, no cache coherence complexity",
				OutcomeB: "Use write-through caching with event-driven invalidation — TTL-based expiration causes stale reads that confuse users when they update settings",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "backend-eng", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(4 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "partial: both use K8s, namespace per team vs per environment",
			Input: ValidateInput{
				OutcomeA: "Organize Kubernetes namespaces by team — each team owns their namespace with resource quotas and RBAC scoped to their services",
				OutcomeB: "Organize Kubernetes namespaces by environment (dev/staging/prod) — team-based namespaces fragment monitoring and make cross-team service discovery harder",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "platform-eng", AgentB: "sre",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "platform", ProjectB: "platform",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "partial: both use event sourcing, snapshot frequency",
			Input: ValidateInput{
				OutcomeA: "Take event store snapshots every 100 events — keeps replay time under 50ms for any aggregate",
				OutcomeB: "Take event store snapshots every 10000 events — frequent snapshots waste storage and the P99 replay time at 1000 events is only 200ms which is acceptable",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "backend-eng", AgentB: "data-eng",
				CreatedA: now, CreatedB: now.Add(12 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},

		// =====================================================================
		// TEMPORAL SUPERSESSION EDGE CASES
		// =====================================================================
		{
			Label: "supersession: same agent revises own timeout decision",
			Input: ValidateInput{
				OutcomeA: "Set HTTP client timeout to 5s for the embedding service",
				OutcomeB: "Revised: increased embedding service timeout from 5s to 30s after observing P99 latency of 12s under load — 5s was causing 15% request failures",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(72 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				SessionIDA: "sess-a", SessionIDB: "sess-b",
			},
			ExpectedRelationship: "supersession",
		},
		{
			Label: "supersession: team lead overrides junior decision",
			Input: ValidateInput{
				OutcomeA: "Use MongoDB for the audit log — schemaless storage is flexible for evolving event shapes",
				OutcomeB: "Overriding MongoDB decision: use PostgreSQL with JSONB columns for audit logs — we already run Postgres, adding MongoDB doubles operational burden for minimal flexibility benefit",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "junior-eng", AgentB: "tech-lead",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "supersession",
		},
		{
			Label: "supersession: narrowing scope after production data",
			Input: ValidateInput{
				OutcomeA: "Enable WAL-based replication for all tables to support real-time analytics",
				OutcomeB: "Scaled back WAL replication to critical tables only (decisions, scored_conflicts) — full replication was generating 200GB/day of WAL and causing replica lag",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "data-eng", AgentB: "data-eng",
				CreatedA: now, CreatedB: now.Add(168 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "supersession",
		},
		{
			Label: "supersession: explicit replacement with migration",
			Input: ValidateInput{
				OutcomeA: "Store embeddings in a separate Qdrant collection per org for tenant isolation",
				OutcomeB: "Migrated from per-org Qdrant collections to a single shared collection with org_id payload filter — per-org collections don't scale past 100 tenants and complicate backup/restore",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "platform-eng",
				CreatedA: now, CreatedB: now.Add(720 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "supersession",
		},
		{
			Label: "not_supersession: same agent different session adding context",
			Input: ValidateInput{
				OutcomeA: "Use cosine similarity threshold of 0.7 for conflict candidate retrieval",
				OutcomeB: "Added a second retrieval pass: after cosine similarity (threshold 0.7), also run BM25 keyword search to catch conflicts that share terminology but have low embedding similarity",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				SessionIDA: "sess-x", SessionIDB: "sess-y",
			},
			ExpectedRelationship: "refinement",
		},

		// =====================================================================
		// NEGATION PATTERNS
		// =====================================================================
		{
			Label: "negation: secure vs has vulnerabilities",
			Input: ValidateInput{
				OutcomeA: "The authentication module is secure — all endpoints validate tokens, rate limiting is applied, no injection vectors found",
				OutcomeB: "The authentication module has critical vulnerabilities — token validation can be bypassed with a malformed JWT header, and the rate limiter doesn't cover the /token endpoint",
				TypeA:    "audit", TypeB: "audit",
				AgentA: "reviewer-a", AgentB: "reviewer-b",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "auth-service", ProjectB: "auth-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "negation: no issues vs critical issues",
			Input: ValidateInput{
				OutcomeA: "Code review of the storage layer: no issues found, implementation follows project conventions, queries are properly parameterized",
				OutcomeB: "Code review of the storage layer: found 3 critical issues — missing org_id filter in ListDecisionsByTag, SQL injection in full-text search handler, N+1 query in GetDecisionWithAlternatives",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "reviewer-a", AgentB: "reviewer-b",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "negation: backwards compatible vs breaking change",
			Input: ValidateInput{
				OutcomeA: "The v2 API changes are backwards compatible — existing clients will continue to work without modification",
				OutcomeB: "The v2 API introduces breaking changes — the response envelope changed from {data: ...} to {result: ...} and three fields were renamed",
				TypeA:    "assessment", TypeB: "assessment",
				AgentA: "api-reviewer", AgentB: "integration-tester",
				CreatedA: now, CreatedB: now.Add(3 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "negation: thread-safe vs race condition",
			Input: ValidateInput{
				OutcomeA: "The event buffer implementation is thread-safe — all shared state is protected by sync.Mutex and the flush goroutine coordinates via channels",
				OutcomeB: "Race condition detected in the event buffer — the flush goroutine reads buf.events without holding the mutex when checking length, causing data races under concurrent Append calls",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "reviewer", AgentB: "fuzzer",
				CreatedA: now, CreatedB: now.Add(4 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "negation: performance acceptable vs degraded",
			Input: ValidateInput{
				OutcomeA: "Load test results: P99 latency is 45ms at 1000 RPS, well within the 100ms SLO — performance is acceptable for GA launch",
				OutcomeB: "Load test results: P99 latency spikes to 800ms at 1000 RPS when conflict detection is enabled — the synchronous embedding call in the hot path is a bottleneck",
				TypeA:    "assessment", TypeB: "assessment",
				AgentA: "sre", AgentB: "perf-eng",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "negation: no data loss vs potential data loss",
			Input: ValidateInput{
				OutcomeA: "The retention policy is safe — expired decisions are soft-deleted with a 30-day recovery window before hard deletion",
				OutcomeB: "Potential data loss in retention policy — the hard delete job doesn't check for active references from scored_conflicts, so deleting a decision orphans its conflict records",
				TypeA:    "audit", TypeB: "code_review",
				AgentA: "auditor", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(6 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},

		// =====================================================================
		// SCALE / QUANTITY DISAGREEMENTS
		// =====================================================================
		{
			Label: "scale: 3 critical issues vs no issues",
			Input: ValidateInput{
				OutcomeA: "Security scan of the API gateway: 3 critical findings — exposed debug endpoint, missing CORS validation, API key logged in plaintext",
				OutcomeB: "Security scan of the API gateway: clean — no critical or high-severity findings, all endpoints properly secured",
				TypeA:    "audit", TypeB: "audit",
				AgentA: "scanner-a", AgentB: "scanner-b",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "scale: 5 services needed vs 2 sufficient",
			Input: ValidateInput{
				OutcomeA: "The platform needs 5 microservices: auth, decisions, conflicts, search, and analytics — each has distinct scaling requirements",
				OutcomeB: "Two services are sufficient: a monolithic API server and a background worker — 5 services adds network hops and deployment complexity we can't justify at current scale",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "architect", AgentB: "pragmatist",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "scale: 99.99% uptime vs 99.9% sufficient",
			Input: ValidateInput{
				OutcomeA: "Target 99.99% uptime (52 min/year downtime) — multi-region active-active deployment with automated failover required",
				OutcomeB: "99.9% uptime (8.7 hours/year) is sufficient for our use case — the cost of multi-region is not justified when our users are in a single timezone and can tolerate brief maintenance windows",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "sre", AgentB: "product-eng",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "platform", ProjectB: "platform",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "scale: keep 90 days vs keep 365 days of data",
			Input: ValidateInput{
				OutcomeA: "Set data retention to 90 days — reduces storage costs and keeps query performance manageable",
				OutcomeB: "Set data retention to 365 days minimum — compliance requires 1 year of audit trail, and customers expect to query historical decisions",
				TypeA:    "architecture", TypeB: "compliance",
				AgentA: "sre", AgentB: "compliance-eng",
				CreatedA: now, CreatedB: now.Add(12 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},

		// =====================================================================
		// MULTI-CLAIM OUTCOMES (only one claim pair conflicts)
		// =====================================================================
		{
			Label: "multi_claim: agree on 4 points disagree on auth",
			Input: ValidateInput{
				OutcomeA: "Architecture review recommendations: (1) use PostgreSQL for persistence, (2) use Redis for caching, (3) use JWT for auth with 15-min expiry, (4) deploy on Kubernetes, (5) use OpenTelemetry for observability",
				OutcomeB: "Architecture review recommendations: (1) use PostgreSQL for persistence, (2) use Redis for caching, (3) use session tokens with server-side storage instead of JWT, (4) deploy on Kubernetes, (5) use OpenTelemetry for observability",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "architect-a", AgentB: "architect-b",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "multi_claim: long review mostly agrees except one finding",
			Input: ValidateInput{
				OutcomeA: "Codebase review: (1) error handling is consistent and follows Go conventions, (2) test coverage is good at 82%, (3) the conflict scorer correctly implements early exit, (4) the WAL implementation is correct and handles fsync properly, (5) migration files are well-structured",
				OutcomeB: "Codebase review: (1) error handling is consistent, (2) test coverage is 82%, (3) conflict scorer early exit works correctly, (4) the WAL implementation has a bug — fsync is called on the file but not the parent directory, so renames may be lost on crash, (5) migrations look good",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "reviewer-a", AgentB: "reviewer-b",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "multi_claim: fix addresses 2 of 3 findings (partial)",
			Input: ValidateInput{
				OutcomeA: "Audit findings: (1) missing org_id filter in ListByTag, (2) N+1 query in GetDecisionTree, (3) retention job doesn't cascade to scored_conflicts",
				OutcomeB: "Fixed audit findings: (1) added org_id filter to ListByTag, (2) replaced N+1 with LEFT JOIN in GetDecisionTree. Note: retention cascade fix deferred to next sprint pending schema review",
				TypeA:    "audit", TypeB: "fix",
				AgentA: "auditor", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(8 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "multi_claim: architecture doc 5 choices all agree",
			Input: ValidateInput{
				OutcomeA: "System design: Go for the backend, PostgreSQL for storage, Qdrant for vectors, React for UI, OpenTelemetry for tracing",
				OutcomeB: "Implementation plan: using Go stdlib net/http for HTTP server, PostgreSQL 16 with pgvector, Qdrant for semantic search, React 19 with TypeScript for the dashboard, OTEL SDK for distributed tracing",
				TypeA:    "architecture", TypeB: "implementation",
				AgentA: "architect", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(72 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},

		// =====================================================================
		// CROSS-TYPE CONFLICTS (architecture standard vs code review)
		// =====================================================================
		{
			Label: "cross_type: standard says pool, review finds no pooling",
			Input: ValidateInput{
				OutcomeA: "Architecture standard: all database access must use connection pooling with a maximum of 20 connections per service instance",
				OutcomeB: "Code review finding: the search handler opens a new database connection per request using sql.Open() instead of using the shared connection pool",
				TypeA:    "architecture", TypeB: "code_review",
				AgentA: "architect", AgentB: "reviewer",
				CreatedA: now.Add(-720 * h), CreatedB: now,
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "cross_type: security policy vs implementation shortcut",
			Input: ValidateInput{
				OutcomeA: "Security policy: all data at rest must be encrypted using AES-256, including database backups and log archives",
				OutcomeB: "Implementation decision: skip encryption for the analytics staging tables — they contain only aggregated counts with no PII, and encryption adds 30% overhead to the ETL pipeline",
				TypeA:    "security", TypeB: "implementation",
				AgentA: "security-eng", AgentB: "data-eng",
				CreatedA: now.Add(-168 * h), CreatedB: now,
				ProjectA: "platform", ProjectB: "platform",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "cross_type: API standard vs actual endpoint",
			Input: ValidateInput{
				OutcomeA: "API design standard: all list endpoints must support cursor-based pagination with a maximum page size of 100",
				OutcomeB: "Implemented GET /v1/decisions endpoint with offset-based pagination and a default page size of 500 for backwards compatibility with the existing dashboard",
				TypeA:    "architecture", TypeB: "implementation",
				AgentA: "api-designer", AgentB: "coder",
				CreatedA: now.Add(-240 * h), CreatedB: now,
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "cross_type: standard followed correctly",
			Input: ValidateInput{
				OutcomeA: "Architecture standard: all HTTP handlers must extract org_id from the auth context and pass it to storage queries for tenant isolation",
				OutcomeB: "Code review: HandleListConflicts correctly extracts org_id from OrgIDFromContext and passes it to storage.ListConflicts — tenant isolation is properly maintained",
				TypeA:    "architecture", TypeB: "code_review",
				AgentA: "architect", AgentB: "reviewer",
				CreatedA: now.Add(-720 * h), CreatedB: now,
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},

		// =====================================================================
		// HIGH-CONFIDENCE WRONG DECISIONS (misleading confidence)
		// =====================================================================
		{
			Label: "high_confidence: confident assertion later proven wrong",
			Input: ValidateInput{
				OutcomeA: "The current embedding model (all-MiniLM-L6-v2) provides sufficient accuracy for conflict detection — tested on 50 pairs with 95% accuracy",
				OutcomeB: "Switching embedding model from all-MiniLM-L6-v2 to text-embedding-3-small — the MiniLM model misses semantic similarity for domain-specific terms, causing 40% false negative rate on production data",
				TypeA:    "assessment", TypeB: "architecture",
				AgentA: "ml-eng", AgentB: "ml-eng",
				CreatedA: now, CreatedB: now.Add(168 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "supersession",
		},
		{
			Label: "high_confidence: overconfident clean audit",
			Input: ValidateInput{
				OutcomeA: "Complete security audit: the system is production-ready with no vulnerabilities. All inputs are validated, all queries are parameterized, authentication is robust.",
				OutcomeB: "Penetration test results: found 2 critical vulnerabilities — the /v1/admin/eval endpoint lacks authentication middleware, and the SSE endpoint leaks events across org boundaries when connection pool is exhausted",
				TypeA:    "audit", TypeB: "audit",
				AgentA: "auditor", AgentB: "pentester",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "high_confidence: both confident opposite conclusions",
			Input: ValidateInput{
				OutcomeA: "Load testing confirms the system handles 10,000 RPS with P99 under 100ms — ready for enterprise rollout",
				OutcomeB: "Load testing shows the system degrades above 2,000 RPS — connection pool exhaustion causes cascading failures at 3,000 RPS. The 10K test likely hit a cached path.",
				TypeA:    "assessment", TypeB: "assessment",
				AgentA: "perf-eng-a", AgentB: "perf-eng-b",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},

		// =====================================================================
		// ADDITIONAL UNRELATED (strengthen negative class)
		// =====================================================================
		{
			Label: "unrelated: completely different domains",
			Input: ValidateInput{
				OutcomeA: "Implemented image resize pipeline using Sharp with WebP output — reduces bundle size by 60% compared to PNG",
				OutcomeB: "Configured Kafka consumer group with 12 partitions for the event ingestion topic — partition count matches peak throughput requirements",
				TypeA:    "implementation", TypeB: "architecture",
				AgentA: "frontend-eng", AgentB: "data-eng",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "website", ProjectB: "analytics",
			},
			ExpectedRelationship: "unrelated",
		},
		{
			Label: "unrelated: same tech different context",
			Input: ValidateInput{
				OutcomeA: "Use PostgreSQL row-level security for the multi-tenant SaaS billing system",
				OutcomeB: "Use PostgreSQL LISTEN/NOTIFY for real-time dashboard updates in the monitoring tool",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "billing-eng", AgentB: "frontend-eng",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "billing", ProjectB: "monitoring",
			},
			ExpectedRelationship: "unrelated",
		},

		// =====================================================================
		// ADDITIONAL COMPLEMENTARY (strengthen negative class)
		// =====================================================================
		{
			Label: "complementary: different layers same system",
			Input: ValidateInput{
				OutcomeA: "The API layer should validate all input using JSON Schema before passing to handlers — fail fast with 400 errors for malformed requests",
				OutcomeB: "The storage layer should use CHECK constraints and NOT NULL to enforce data integrity — defense in depth against bugs in the API validation",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "backend-eng", AgentB: "dba",
				CreatedA: now, CreatedB: now.Add(4 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "complementary: performance and security non-overlapping",
			Input: ValidateInput{
				OutcomeA: "Add a read-through cache in front of GetDecision to reduce P99 from 15ms to 2ms for repeated lookups",
				OutcomeB: "Add audit logging for all GetDecision calls to track who accessed which decisions for compliance",
				TypeA:    "architecture", TypeB: "security",
				AgentA: "perf-eng", AgentB: "compliance-eng",
				CreatedA: now, CreatedB: now.Add(8 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},

		// =====================================================================
		// ADDITIONAL REFINEMENT (strengthen negative class)
		// =====================================================================
		{
			Label: "refinement: prototype then production implementation",
			Input: ValidateInput{
				OutcomeA: "Prototype conflict detection using basic string matching — proof of concept to validate the product hypothesis before investing in embeddings",
				OutcomeB: "Replaced string matching prototype with embedding-based similarity search using Qdrant — cosine similarity with 0.7 threshold followed by LLM validation for 5-class relationship classification",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(336 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "refinement: error handling improvement on existing code",
			Input: ValidateInput{
				OutcomeA: "Implemented the trace ingestion endpoint — accepts POST /v1/trace with outcome, reasoning, confidence, creates a decision record",
				OutcomeB: "Enhanced trace ingestion with idempotency keys, input validation (confidence 0-1 range, non-empty outcome), and structured error responses with error codes for each validation failure",
				TypeA:    "implementation", TypeB: "implementation",
				AgentA: "coder", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(96 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				SessionIDA: "sess-impl-1", SessionIDB: "sess-impl-2",
			},
			ExpectedRelationship: "refinement",
		},

		// =====================================================================
		// PAIRS FROM PRIOR EXPANSION (merged from main)
		// =====================================================================
		{
			Label: "subtle: same database different replication mode",
			Input: ValidateInput{
				OutcomeA: "PostgreSQL replication must be synchronous — we cannot tolerate any data loss for financial records",
				OutcomeB: "Use asynchronous PostgreSQL replication — sync replication adds 40ms latency and our RPO allows 1s of loss",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "data-engineer",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "payments", ProjectB: "payments",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "subtle: same framework different version strategy",
			Input: ValidateInput{
				OutcomeA: "Pin all Go dependencies to exact versions in go.mod — reproducible builds are critical for audit",
				OutcomeB: "Use minimum version selection with caret ranges — pinning creates a maintenance burden and blocks security patches",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "security-auditor", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(72 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "partial: both want rate limiting, disagree on strategy",
			Input: ValidateInput{
				OutcomeA: "Rate limit the API at 100 req/s per API key using token bucket — protects against abuse while allowing bursts",
				OutcomeB: "Rate limit the API at 10 req/s per API key using sliding window — stricter limits prevent resource exhaustion during peak",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "security-auditor", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "partial: both want monitoring, disagree on retention",
			Input: ValidateInput{
				OutcomeA: "Retain OTEL traces for 30 days — sufficient for debugging and keeps storage costs manageable",
				OutcomeB: "Retain OTEL traces for 90 days minimum — ISO 42001 requires extended audit windows for AI system monitoring",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "security-auditor",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "negation: security assessment positive vs negative",
			Input: ValidateInput{
				OutcomeA: "The authentication module is secure — all endpoints validate JWT tokens, rate limiting is correctly applied, no vulnerabilities found",
				OutcomeB: "The authentication module has critical vulnerabilities — JWT validation can be bypassed with malformed tokens, rate limiting is missing on 3 endpoints",
				TypeA:    "assessment", TypeB: "assessment",
				AgentA: "reviewer-a", AgentB: "security-auditor",
				CreatedA: now, CreatedB: now.Add(3 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "negation: performance assessment opposite conclusions",
			Input: ValidateInput{
				OutcomeA: "Query performance is acceptable — P99 latency is 45ms, well within our 100ms SLO",
				OutcomeB: "Query performance is unacceptable — P99 latency is 450ms under production load, far exceeding our 100ms SLO",
				TypeA:    "assessment", TypeB: "assessment",
				AgentA: "coder", AgentB: "analyst",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "scale: zero issues vs multiple critical issues",
			Input: ValidateInput{
				OutcomeA: "Reviewed the conflict detection module — no issues found, code is clean and well-tested",
				OutcomeB: "Reviewed the conflict detection module — found 4 critical issues: missing org_id filter on UpsertConflictLabel, unbounded memory in claim comparison, race condition in backfill workers, SQL injection in search handler",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "reviewer-a", AgentB: "security-auditor",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "scale: high score vs low score same system",
			Input: ValidateInput{
				OutcomeA: "Akashi codebase quality assessment: aggregate score 95/100. Excellent architecture, no significant issues",
				OutcomeB: "Akashi codebase quality assessment: aggregate score 62/100. Multiple architectural weaknesses, missing test coverage, inconsistent error handling",
				TypeA:    "assessment", TypeB: "assessment",
				AgentA: "reviewer-a", AgentB: "senior-engineer",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "cross_type: architecture standard violated by review finding",
			Input: ValidateInput{
				OutcomeA: "Architecture standard: all API endpoints must be idempotent — use idempotency keys for POST operations",
				OutcomeB: "Code review finding: POST /v1/trace is not idempotent — duplicate traces create duplicate rows with different IDs",
				TypeA:    "architecture", TypeB: "code_review",
				AgentA: "planner", AgentB: "reviewer",
				CreatedA: now.Add(-72 * h), CreatedB: now,
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "cross_type: planning decision contradicted by implementation finding",
			Input: ValidateInput{
				OutcomeA: "We will use eventual consistency for the search index — latency is acceptable for our use case",
				OutcomeB: "Bug fix: made search index writes synchronous because eventual consistency caused stale results that broke conflict detection",
				TypeA:    "planning", TypeB: "bug_fix",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now.Add(-168 * h), CreatedB: now,
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "supersession",
		},
		{
			Label: "multi_claim: one conflicting claim among several agreeing ones",
			Input: ValidateInput{
				OutcomeA: "Architecture review: (1) PostgreSQL is the right choice for storage. (2) Use Qdrant for vector search. (3) Deploy with 3 replicas for HA. (4) Use REST for the API.",
				OutcomeB: "Architecture review: (1) PostgreSQL is the right choice for storage. (2) Use Qdrant for vector search. (3) Deploy with 3 replicas for HA. (4) Use gRPC for the API — better performance and type safety.",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "temporal: same agent explicitly reverses own decision",
			Input: ValidateInput{
				OutcomeA: "Chose SQLite for the local storage backend — simpler than PostgreSQL for single-user mode",
				OutcomeB: "Reversed SQLite decision — switching to DuckDB for local storage because SQLite's single-writer model blocks concurrent embedding writes",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(168 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "supersession",
		},
		{
			Label: "temporal: different agent supersedes with new evidence",
			Input: ValidateInput{
				OutcomeA: "Use mxbai-embed-large (1024d) for embeddings — best quality/size trade-off based on MTEB benchmarks",
				OutcomeB: "Switching to text-embedding-3-small (1536d) — mxbai-embed-large requires Ollama which adds 2GB RAM to deployment; OpenAI API is acceptable for cloud tier",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(336 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "supersession",
		},
		{
			Label: "high_confidence_both: both very confident, contradictory",
			Input: ValidateInput{
				OutcomeA: "The data retention policy MUST delete records after 30 days — GDPR Article 5(1)(e) requires it",
				OutcomeB: "The data retention policy MUST retain records for 7 years minimum — financial regulations require full audit trail preservation",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "security-auditor", AgentB: "compliance-auditor",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "payments", ProjectB: "payments",
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "agreement: same conclusion different framing",
			Input: ValidateInput{
				OutcomeA: "The conflict detection pipeline needs an LLM validation stage — embedding similarity alone produces too many false positives",
				OutcomeB: "Added LLM-based validation to the conflict scorer — cosine similarity can't distinguish same-topic agreement from disagreement",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "reviewer", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "agreement: both identify same problem",
			Input: ValidateInput{
				OutcomeA: "The search outbox has a bug — decisions without embeddings are never queued, making them permanently invisible",
				OutcomeB: "Found that CreateDecision conditionally queues search_outbox only when embedding is non-nil, causing decisions to be lost from search",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "coder", AgentB: "senior-engineer",
				CreatedA: now, CreatedB: now.Add(h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "unrelated: same decision type different domains entirely",
			Input: ValidateInput{
				OutcomeA: "Use event sourcing for the payment ledger — auditability is a regulatory requirement for financial transactions",
				OutcomeB: "Use CQRS for the notification preferences API — read/write patterns are highly asymmetric",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "planner",
				CreatedA: now, CreatedB: now.Add(48 * h),
				ProjectA: "payments", ProjectB: "notifications",
			},
			ExpectedRelationship: "unrelated",
		},
		{
			Label: "unrelated: similar vocabulary different systems",
			Input: ValidateInput{
				OutcomeA: "The batch processing pipeline handles 10K records/minute — sufficient for current load but needs horizontal scaling by Q3",
				OutcomeB: "The batch processing pipeline handles 50K records/minute — we over-provisioned but the cost is acceptable for the SLA guarantee",
				TypeA:    "assessment", TypeB: "assessment",
				AgentA: "analyst", AgentB: "analyst",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "data-pipeline-v1", ProjectB: "data-pipeline-v2",
			},
			ExpectedRelationship: "unrelated",
		},
		{
			Label: "refinement: general principle then specific implementation",
			Input: ValidateInput{
				OutcomeA: "All database queries must be scoped by org_id for multi-tenancy isolation",
				OutcomeB: "Added org_id filter to 14 queries in storage/conflicts.go, storage/claims.go, and storage/grants.go that were missing the multi-tenancy scope",
				TypeA:    "architecture", TypeB: "bug_fix",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now.Add(-720 * h), CreatedB: now,
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "refinement: design sketch then full spec",
			Input: ValidateInput{
				OutcomeA: "We need a conflict resolution workflow — agents should be able to acknowledge, resolve, or dismiss conflicts",
				OutcomeB: "Implemented conflict lifecycle: open → resolved, open → false_positive. PATCH /v1/conflicts/{id} accepts status and resolution_note. Requires agent+ role.",
				TypeA:    "planning", TypeB: "architecture",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(72 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},
		// =================================================================
		// PRODUCTION-LABELED PAIRS (from real conflict audit, 37 pairs)
		// =================================================================
		{
			Label: "refinement: Decision A explicitly resolves prior conflicts regarding ...",
			Input: ValidateInput{
				OutcomeA: "Resolved all open tagline/positioning conflicts for Akashi in favor of 'version control for AI decisions' as the primary frame. 'Git blame for AI decisions' is retired as a primary tagline. Active coordination (not passive audit trail) is the prim...",
				OutcomeB: "Added Akashi project page, logo, and home page card to ashita-ai; pushed to main (333d4a2). Used 'version control for AI decisions' as primary frame with active coordination as value prop.",
				TypeA:    "trade_off", TypeB: "deployment",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(1 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.81,
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "genuine: The agent \"strategist\" recommends changing the tagline ...",
			Input: ValidateInput{
				OutcomeA: "\"Git blame for AI decisions\" is a strong developer hook but incomplete as a full marketing strategy. Recommend keeping it for developer-facing channels (Show HN, README, engineering audiences) while maintaining separate frames for compliance buy...",
				OutcomeB: "Updated tagline and marketing copy across 12 files in akashi/. Primary change: \"Git blame for AI decisions\" → \"Version control for AI decisions\" as primary tagline. Secondary: \"the black box recorder for AI decisions\" → \"version control for...",
				TypeA:    "assessment", TypeB: "documentation",
				AgentA: "analyst", AgentB: "strategist",
				CreatedA: now, CreatedB: now.Add(2 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.71,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "agreement: Decision A advocates for \"Git blame\" as an effective de...",
			Input: ValidateInput{
				OutcomeA: "Recommended primary tagline: \"Version control for AI decisions.\" Enterprise frame: \"Coordination infrastructure for multi-agent AI.\" Developer bridging copy: \"Like git blame — but it runs before you commit, not after.\" Candidate E (\"The bla...",
				OutcomeB: "\"Git blame for AI decisions\" is a strong developer hook but incomplete as a full marketing strategy. Recommend keeping it for developer-facing channels (Show HN, README, engineering audiences) while maintaining separate frames for compliance buy...",
				TypeA:    "positioning_recommendation", TypeB: "assessment",
				AgentA: "analyst", AgentB: "strategist",
				CreatedA: now, CreatedB: now.Add(3 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.82,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "refinement: The first decision advocates for \"Version control for AI...",
			Input: ValidateInput{
				OutcomeA: "\"Git blame for AI decisions\" is a strong developer hook but incomplete as a full marketing strategy. Recommend keeping it for developer-facing channels (Show HN, README, engineering audiences) while maintaining separate frames for compliance buy...",
				OutcomeB: "Four candidate repositioning directions ranked by strategic clarity: (1) \"Version control for AI decisions\" — broadens git analogy beyond blame to full VC paradigm; (2) \"Shared memory for your AI agents\" — captures coordination story, explains...",
				TypeA:    "assessment", TypeB: "positioning_recommendation",
				AgentA: "analyst", AgentB: "strategist",
				CreatedA: now, CreatedB: now.Add(4 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.74,
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "genuine: Decision A emphasizes Akashi's role in active coordinatio...",
			Input: ValidateInput{
				OutcomeA: "Akashi is a decision audit trail for multi-agent AI systems — git blame for AI decisions. Agents trace decisions with outcome/reasoning/confidence, check for precedents before deciding, and conflicts are detected automatically via embedding simila...",
				OutcomeB: "\"Git blame for AI decisions\" accurately describes one feature (the audit trail) but misrepresents Akashi's primary value, which is active coordination infrastructure for multi-agent AI systems. The tagline describes the exhaust (the log), not th...",
				TypeA:    "assessment", TypeB: "positioning_analysis",
				AgentA: "admin", AgentB: "strategist",
				CreatedA: now, CreatedB: now.Add(5 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.79,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: Both decisions evaluate the tagline \"Git blame for AI de...",
			Input: ValidateInput{
				OutcomeA: "\"Git blame for AI decisions\" is a strong developer hook but incomplete as a full marketing strategy. Recommend keeping it for developer-facing channels (Show HN, README, engineering audiences) while maintaining separate frames for compliance buy...",
				OutcomeB: "\"Git blame for AI decisions\" accurately describes one feature (the audit trail) but misrepresents Akashi's primary value, which is active coordination infrastructure for multi-agent AI systems. The tagline describes the exhaust (the log), not th...",
				TypeA:    "assessment", TypeB: "positioning_analysis",
				AgentA: "analyst", AgentB: "strategist",
				CreatedA: now, CreatedB: now.Add(6 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.84,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "unrelated: Decision A focuses on identifying and validating code iss...",
			Input: ValidateInput{
				OutcomeA: "Reviewed PR #343 (feat: ground truth dataset for conflict detection precision/recall). Found 2 validated high-signal issues: (1) UpsertConflictLabel has a multi-tenancy bypass — ON CONFLICT (scored_conflict_id) DO UPDATE lacks org_id guard, allowi...",
				OutcomeB: "Marked 9 conflict groups as resolved/false_positive to clean contaminated conflict queue: 6 false positives (demo/cross-project data) as false_positive, 3 intentional evolutions (admin vs admin: audit readiness→trace health refactoring, mat view→scored_confli...",
				TypeA:    "code_review", TypeB: "assessment",
				AgentA: "admin", AgentB: "reviewer",
				CreatedA: now, CreatedB: now.Add(7 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.72,
			},
			ExpectedRelationship: "unrelated",
		},
		{
			Label: "genuine: Decision A claims a 4-agent review found zero high-signal...",
			Input: ValidateInput{
				OutcomeA: "Reviewed PR #348 (feat(ui): add Vitest + Playwright test infrastructure). Four parallel review agents (2x CLAUDE.md compliance, 1x bug scan, 1x security/logic) found zero high-signal issues. All 16 changed files are in ui/ directory — test configs...",
				OutcomeB: "Audited last 35 merged PRs (#305-#345). Key findings: (1) Zero GitHub reviews on all 35 PRs — every one self-merged. (2) 17-hour merge window with 12-min median time-to-merge. (3) Critical open conflict: 4-agent PR review missed bugs that broader ...",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(8 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.72,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "refinement: Decision A addresses the issue of the conditional queuing...",
			Input: ValidateInput{
				OutcomeA: "Comprehensive staff-level codebase review of Akashi. Aggregate score 86/100. Three critical findings: (1) CreateDecision conditionally queues search_outbox (embedding != nil) while CreateTraceTx always queues — decisions without embeddings become ...",
				OutcomeB: "Implemented 5 fixes from codebase review: (1) CreateDecision/ReviseDecision now unconditionally queue search_outbox — decisions without embeddings are no longer permanently invisible to search. (2) ClearAllConflicts and ClearUnvalidatedConflicts n...",
				TypeA:    "code_review", TypeB: "bug_fix",
				AgentA: "coder", AgentB: "senior-engineer",
				CreatedA: now, CreatedB: now.Add(9 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.72,
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "genuine: Decision B identifies multiple significant issues and inc...",
			Input: ValidateInput{
				OutcomeA: "Reviewed PR #342 (feat: outcome_score from assessments, anti-gaming for alternatives). Four parallel review agents (2x CLAUDE.md compliance, 1x bug scan, 1x security/logic) found zero high-signal issues across 19 changed files. All storage queries...",
				OutcomeB: "Full staff-level review complete. Scores: Correctness 85, Consistency 88, Readability 92, Auditability 82, Durability 84, Ease-of-use 86, Documentation 83, Maintainability 89, Performance 87, Architecture 90, Design 88. Aggregate 87/100. Critical ...",
				TypeA:    "review", TypeB: "code_review",
				AgentA: "admin", AgentB: "senior-engineer",
				CreatedA: now, CreatedB: now.Add(10 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.72,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: Decision A's implementation of distribution-aware ReScore...",
			Input: ValidateInput{
				OutcomeA: "Implemented distribution-aware ReScore normalization for search re-ranking: (1) percentile-normalized citation counts via in-memory cache refreshed hourly, replacing arbitrary log saturation at 5; (2) Qdrant rank preserved as tie-breaker when adju...",
				OutcomeB: "redesigned ReScore formula (issue #235): assessment primary signal (0.40, no phantom neutral), logarithmic citations (log1p/log(6)), conflict win rate zero-contribution when no history, completeness removed from relevance formula",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(11 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.76,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "refinement: Decision A implements an early exit and adaptive pruning ...",
			Input: ValidateInput{
				OutcomeA: "Identified conflict candidate pipeline (internal/conflicts/scorer.go) as the most naive part of Akashi: fetches 50 Qdrant candidates and scores all unconditionally with no early exit, no significance-based pruning, and a hardcoded limit with no em...",
				OutcomeB: "Implemented issue #263: conflict candidate pipeline early exit and adaptive pruning. Pre-computes cheap significance for all candidates, sorts descending, and exits early when significance drops below configurable floor (default 0.25). Changed def...",
				TypeA:    "assessment", TypeB: "architecture",
				AgentA: "coder", AgentB: "reviewer",
				CreatedA: now, CreatedB: now.Add(12 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.84,
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "refinement: Decision A (PR #306) explicitly replaces Decision B (PR #...",
			Input: ValidateInput{
				OutcomeA: "PR #308 (CLOSED, superseded by #306): feat: GDPR tombstone erasure for decisions — early attempt; closed in favor of PR #306 which implemented the same feature with a cleaner approach using SET LOCAL akashi.erasure_in_progress to bypass the immuta...",
				OutcomeB: "PR #306 (MERGED 2026-03-06): feat: GDPR tombstone erasure via POST /v1/decisions/{id}/erase — scrubs PII fields in-place (outcome, reasoning, alternatives, evidence, embeddings) without deleting the row; recomputes hash over scrubbed content; pres...",
				TypeA:    "feature_scope", TypeB: "feature_scope",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(13 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.86,
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "unrelated: Decision A asserts that all bugs from the prior review ar...",
			Input: ValidateInput{
				OutcomeA: "Third comprehensive review of Engram. Aggregate 66/100. Scores: Correctness 60, Consistency 68, Readability 84, Auditability 55, Durability 50, Ease of Use 78, Documentation 80, Maintainability 72, Performance 68, Architecture 76, Design 74. Found...",
				OutcomeB: "Full staff-level review complete. Scores: Correctness 87, Consistency 87, Readability 93, Auditability 85, Durability 87, Ease-of-use 86, Documentation 85, Maintainability 89, Performance 87, Architecture 91, Design 89. Aggregate 88/100. Key findi...",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "senior-engineer", AgentB: "senior-staff-reviewer",
				CreatedA: now, CreatedB: now.Add(14 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.75,
			},
			ExpectedRelationship: "unrelated",
		},
		{
			Label: "genuine: Both decisions recommend different approaches to model se...",
			Input: ValidateInput{
				OutcomeA: "Deploy a fine-tuned, on-premise or private-cloud open-source LLM optimized for healthcare domain data combined with a lightweight retrieval-augmented generation (RAG) system to meet latency, privacy, accuracy, and time-to-market requirements.",
				OutcomeB: "Use a hybrid approach combining an on-premise fine-tuned smaller language model with selective cloud-based API augmentation for complex queries to balance latency, accuracy, data privacy, and time-to-market.",
				TypeA:    "model_selection", TypeB: "model_selection",
				AgentA: "ml-engineer", AgentB: "product-manager",
				CreatedA: now, CreatedB: now.Add(15 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.84,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: Both decisions propose fundamentally different architectu...",
			Input: ValidateInput{
				OutcomeA: "Start with a single robust, modular monolithic backend service focused on security and reliability, and evolve to a microservices architecture only after validating clear scalability or team autonomy needs.",
				OutcomeB: "Design a microservices-based backend architecture leveraging container orchestration (Kubernetes) with strict PCI-DSS compliance controls, isolated sensitive data handling, and horizontally scalable services, balanced with automation to keep opera...",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "senior-engineer", AgentB: "systems-architect",
				CreatedA: now, CreatedB: now.Add(16 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.78,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: Both decisions propose fundamentally different architectu...",
			Input: ValidateInput{
				OutcomeA: "Start with a simple, monolith-based backend architecture focused on robust, secure core payment processing, modular design, and database scalability, then extract microservices only as clear scalability or complexity bottlenecks emerge.",
				OutcomeB: "Design a microservices-based backend architecture leveraging container orchestration (Kubernetes) with strict PCI-DSS compliance controls, isolated sensitive data handling, and horizontally scalable services, balanced with automation to keep opera...",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "senior-engineer", AgentB: "systems-architect",
				CreatedA: now, CreatedB: now.Add(17 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.75,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "agreement: Both decisions propose different initial architectures (m...",
			Input: ValidateInput{
				OutcomeA: "Start with a single robust, modular monolithic backend service focused on security and reliability, and evolve to a microservices architecture only after validating clear scalability or team autonomy needs.",
				OutcomeB: "Start with a simple, monolith-based backend architecture focused on robust, secure core payment processing, modular design, and database scalability, then extract microservices only as clear scalability or complexity bottlenecks emerge.",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "senior-engineer", AgentB: "senior-engineer",
				CreatedA: now, CreatedB: now.Add(18 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.87,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "genuine: Decision A advocates for a microservices architecture, wh...",
			Input: ValidateInput{
				OutcomeA: "Start with a single robust, modular monolithic backend service focused on security and reliability, and evolve to a microservices architecture only after validating clear scalability or team autonomy needs.",
				OutcomeB: "Adopt microservices architecture: payment-service, fraud-service, and notification-service as independent deployables with Kafka for async inter-service communication and a shared PCI-DSS network segment.",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "senior-engineer", AgentB: "systems-architect",
				CreatedA: now, CreatedB: now.Add(19 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.77,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: Decision A advocates for adopting a microservices archite...",
			Input: ValidateInput{
				OutcomeA: "Start with a simple, monolith-based backend architecture focused on robust, secure core payment processing, modular design, and database scalability, then extract microservices only as clear scalability or complexity bottlenecks emerge.",
				OutcomeB: "Adopt microservices architecture: payment-service, fraud-service, and notification-service as independent deployables with Kafka for async inter-service communication and a shared PCI-DSS network segment.",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "senior-engineer", AgentB: "systems-architect",
				CreatedA: now, CreatedB: now.Add(20 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.78,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "agreement: Both decisions explicitly oppose each other by recommendi...",
			Input: ValidateInput{
				OutcomeA: "Explicitly contradicting: Do NOT run Qdrant CreateFieldIndex on startup - use versioned migrations for all index changes",
				OutcomeB: "Do NOT run Qdrant CreateFieldIndex on every startup — use explicit versioned migrations instead for all index changes",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "reviewer",
				CreatedA: now, CreatedB: now.Add(21 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.95,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "agreement: Both decisions address the same query about whether to ru...",
			Input: ValidateInput{
				OutcomeA: "Do NOT run Qdrant CreateFieldIndex on every startup — use explicit versioned migrations instead",
				OutcomeB: "Do NOT run Qdrant CreateFieldIndex on every startup — use explicit versioned migrations instead for all index changes",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "reviewer",
				CreatedA: now, CreatedB: now.Add(22 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.95,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "genuine: The two decisions present incompatible approaches regardi...",
			Input: ValidateInput{
				OutcomeA: "Explicitly contradicting: Do NOT run Qdrant CreateFieldIndex on startup - use versioned migrations for all index changes",
				OutcomeB: "Always run Qdrant CreateFieldIndex on startup (not just at collection creation) to backfill indexes idempotently",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(23 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.78,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "agreement: Decision B explicitly replaces Decision A by reiterating ...",
			Input: ValidateInput{
				OutcomeA: "Do NOT run Qdrant CreateFieldIndex on every startup — use explicit versioned migrations instead",
				OutcomeB: "Explicitly contradicting: Do NOT run Qdrant CreateFieldIndex on startup - use versioned migrations for all index changes",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.89,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "genuine: The two decisions present incompatible positions on the s...",
			Input: ValidateInput{
				OutcomeA: "Do NOT run Qdrant CreateFieldIndex on every startup — use explicit versioned migrations instead for all index changes",
				OutcomeB: "Always run Qdrant CreateFieldIndex on startup (not just at collection creation) to backfill indexes idempotently",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "reviewer", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(25 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.82,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: The two decisions advocate opposing approaches to index m...",
			Input: ValidateInput{
				OutcomeA: "Do NOT run Qdrant CreateFieldIndex on every startup — use explicit versioned migrations instead",
				OutcomeB: "Always run Qdrant CreateFieldIndex on startup (not just at collection creation) to backfill indexes idempotently",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(26 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.84,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: The decisions by the \"planner\" and \"coder\" recommend ...",
			Input: ValidateInput{
				OutcomeA: "Use ClickHouse as the primary analytics store — columnar storage is necessary for the query patterns",
				OutcomeB: "Use PostgreSQL as the primary data store — single source of truth, avoid distributed joins",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "planner",
				CreatedA: now, CreatedB: now.Add(27 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.68,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "unrelated: Decision A's approach of using ORM-level updates for trac...",
			Input: ValidateInput{
				OutcomeA: "Implemented decision row immutability via BEFORE UPDATE trigger (migration 036, closes #95). 11 core columns blocked (outcome, reasoning, confidence, decision_type, agent_id, run_id, org_id, content_hash, valid_from, created_at, transaction_time)....",
				OutcomeB: "Chose ORM-level onupdate=_utcnow for updated_at instead of database triggers. Added updated_at only to 7 mutable models (AssetDB, TeamDB, UserDB, ContractDB, RegistrationDB, ProposalDB, APIKeyDB). Excluded 4 immutable models (AuditEventDB, Acknowl...",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(28 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.76,
			},
			ExpectedRelationship: "unrelated",
		},
		{
			Label: "agreement: Decision A states that external witnessing via a self-ser...",
			Input: ValidateInput{
				OutcomeA: "ADR stress test across all 16 ADRs (9 public, 7 internal). 14 of 16 are internally and mutually consistent. Two cross-reference issues: (1) ADR-013 enterprise table says external witnessing 'cannot self-serve' which directly contradicts ADR-011's ...",
				OutcomeB: "Deferred external witnessing (RFC 3161 TSA, multi-TSA cross-signing, private transparency log, witness network) indefinitely. No compliance framework requires cryptographic timestamping — ISO 42001, EU AI Act, SR 11-7 all require having the docume...",
				TypeA:    "assessment", TypeB: "architecture",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(29 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.72,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "refinement: Decision B explicitly replaces and reorganizes the metric...",
			Input: ValidateInput{
				OutcomeA: "Refactored spec 32 from composite \"audit readiness score\" (weighted 35/35/30) to raw metrics \"decision trace health.\" No composite score — each metric group (decision completeness, evidence coverage, conflict resolution) reports independently ...",
				OutcomeB: "Implemented trace health endpoint (GET /v1/trace-health, admin-only) with aggregate quality stats, evidence coverage, conflict resolution rates, gap detection (max 3), and status computation. PR #104 on feature/trace-health branch.",
				TypeA:    "architecture", TypeB: "feature_scope",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(30 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.77,
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "genuine: Decision B explicitly refactors and replaces the composit...",
			Input: ValidateInput{
				OutcomeA: "Refactored spec 32 from composite \"audit readiness score\" (weighted 35/35/30) to raw metrics \"decision trace health.\" No composite score — each metric group (decision completeness, evidence coverage, conflict resolution) reports independently ...",
				OutcomeB: "Designed audit readiness score (spec 32): 3 scored dimensions (completeness 35%, evidence 35%, conflicts 30%) + confidence calibration stub. Fixed weights with 4 regulatory presets. Minimum 50 decisions. OSS returns org-scoped scores + basic recom...",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(31 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.81,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "genuine: The first decision outlines a phased TSA provider strateg...",
			Input: ValidateInput{
				OutcomeA: "TSA provider strategy: DigiCert free endpoint as default (phase 1), GlobalSign paid SLA when first enterprise customer arrives (phase 2), customer-configurable TSA URL always supported",
				OutcomeB: "Deferred external witnessing (RFC 3161 TSA, multi-TSA cross-signing, private transparency log, witness network) indefinitely. No compliance framework requires cryptographic timestamping — ISO 42001, EU AI Act, SR 11-7 all require having the docume...",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(32 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.71,
			},
			ExpectedRelationship: "contradiction",
		},
		{
			Label: "refinement: Decision B indicates that the issue identified in Decisio...",
			Input: ValidateInput{
				OutcomeA: "Completed all 9th code review P1/P2 items. P1-R9e/f (buffer/outbox tests) already comprehensive. P1-R9h (mat view refresh) moot — decision_conflicts dropped in migration 027. P2-R9i (O(n²) mat view) already fixed by scored_conflicts. P2-R9k remove...",
				OutcomeB: "Comprehensive schema/migration/data-model audit of Akashi OSS repo. Reviewed 12 migration files (001-032), 7 model source files, 12 storage layer files, and 1 integrity package file. Found 4 critical issues, 7 major issues, and 5 moderate issues. ...",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(33 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.73,
			},
			ExpectedRelationship: "refinement",
		},
		{
			Label: "agreement: Decision A asserts the ReScore score overflow issue has b...",
			Input: ValidateInput{
				OutcomeA: "ReScore scoring formula is broken: qualityBonus * recencyDecay can produce values exceeding 1.0 when the raw Qdrant cosine similarity is high, bypassing the Math.Min clamp",
				OutcomeB: "Deep subsystem review of integrity, search/outbox, qdrant, conflicts, embedding, quality, and decisions service. Found 14 issues across 7 subsystems: 2 critical (ReScore can produce scores >1.0 despite Math.Min clamping because qualityBonus*recenc...",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(34 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.74,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "agreement: Decision A indicates that ReScore is functioning properly...",
			Input: ValidateInput{
				OutcomeA: "ReScore scoring formula is broken: qualityBonus * recencyDecay can produce values exceeding 1.0 when the raw Qdrant cosine similarity is high, bypassing the Math.Min clamp",
				OutcomeB: "Deep review of 14 files across search/outbox, conflict detection, MCP, embedding, integrity, quality, and decisions subsystems. Found issues across multiple severity levels: ReScore can produce scores >1.0 despite clamping (quality bonus up to 0.9...",
				TypeA:    "code_review", TypeB: "code_review",
				AgentA: "admin", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(35 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.72,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "agreement: Both decisions recommend using ClickHouse for analytics p...",
			Input: ValidateInput{
				OutcomeA: "Use ClickHouse as the primary analytics store — columnar storage is necessary for the query patterns",
				OutcomeB: "Use ClickHouse as the core columnar database for your observability platform to handle 2TB/day ingestion with sub-second queries, support 13-month hot retention efficiently, and maintain operational manageability with a 4-engineer team.",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "data-engineer",
				CreatedA: now, CreatedB: now.Add(36 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.77,
			},
			ExpectedRelationship: "complementary",
		},
		{
			Label: "unrelated: Decision A focuses on closing multiple issues and enhanci...",
			Input: ValidateInput{
				OutcomeA: "Verified all 5 changes from the semantic/procedural quality plan are already implemented in main (PR #217). Added 4 missing tests to fill coverage gaps: dynamic fact cap scaling (#209) and consolidation confidence capping integration (#215). Closi...",
				OutcomeB: "Built claim-level conflict detection (PR #82) with dual-pass scoring. Claim splitter extracts sentences from outcomes, embeds individually, cross-compares claim pairs. Thresholds tuned against 32 real decisions: claimTopicSimFloor=0.60, claimDivFl...",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "coder", AgentB: "admin",
				CreatedA: now, CreatedB: now.Add(37 * h),
				ProjectA: "akashi", ProjectB: "akashi",
				TopicSimilarity: 0.74,
			},
			ExpectedRelationship: "unrelated",
		},
	}
}
