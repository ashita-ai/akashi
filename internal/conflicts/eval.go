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
			Label: "subtle: same cache different TTL",
			Input: ValidateInput{
				OutcomeA: "Use Redis for session caching with a 5-minute TTL to prevent stale reads during deployments",
				OutcomeB: "Use Redis for session caching with a 24-hour TTL to minimize cache miss latency",
				TypeA:    "architecture", TypeB: "architecture",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(24 * h),
				ProjectA: "my-service", ProjectB: "my-service",
			},
			ExpectedRelationship: "contradiction",
		},
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

		// =====================================================================
		// PARTIAL CONTRADICTIONS (agree on approach, disagree on detail)
		// =====================================================================
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

		// =====================================================================
		// NEGATION PATTERNS (explicit semantic opposition)
		// =====================================================================
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

		// =====================================================================
		// SCALE/QUANTITY DISAGREEMENTS
		// =====================================================================
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

		// =====================================================================
		// CROSS-TYPE CONFLICTS (architecture standard vs finding)
		// =====================================================================
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

		// =====================================================================
		// MULTI-CLAIM OUTCOMES (only one claim pair conflicts)
		// =====================================================================
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

		// =====================================================================
		// TEMPORAL SUPERSESSION EDGE CASES
		// =====================================================================
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

		// =====================================================================
		// HIGH-CONFIDENCE WRONG (tests whether confidence misleads)
		// =====================================================================
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

		// =====================================================================
		// ADDITIONAL AGREEMENT PATTERNS
		// =====================================================================
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

		// =====================================================================
		// ADDITIONAL UNRELATED PATTERNS
		// =====================================================================
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

		// =====================================================================
		// ADDITIONAL REFINEMENT PATTERNS
		// =====================================================================
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
				OutcomeB: "Implemented conflict lifecycle: open → acknowledged → resolved, open → wont_fix. PATCH /v1/conflicts/{id} accepts status and resolution_note. Requires agent+ role.",
				TypeA:    "planning", TypeB: "architecture",
				AgentA: "planner", AgentB: "coder",
				CreatedA: now, CreatedB: now.Add(72 * h),
				ProjectA: "akashi", ProjectB: "akashi",
			},
			ExpectedRelationship: "refinement",
		},
	}
}
