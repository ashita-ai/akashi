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
	}
}

// ---------------------------------------------------------------------------
// Scorer-level precision/recall evaluation (embedding pipeline, not LLM validator)
// ---------------------------------------------------------------------------

// ScorerPrecisionRecall holds evaluation metrics for a conflict detection run.
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
		// true negatives: !detected && !isPositive — not tracked, not needed for P/R
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
