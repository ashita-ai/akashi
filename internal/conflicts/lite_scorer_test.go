package conflicts

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/compact"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Apply the relevant schema tables.
	_, err = db.Exec(`
		CREATE TABLE decisions (
			id TEXT PRIMARY KEY,
			org_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			decision_type TEXT NOT NULL,
			outcome TEXT NOT NULL,
			confidence REAL NOT NULL,
			reasoning TEXT,
			embedding BLOB,
			supersedes_id TEXT,
			valid_from TEXT NOT NULL,
			valid_to TEXT,
			project TEXT,
			transaction_time TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE scored_conflicts (
			id TEXT PRIMARY KEY,
			conflict_kind TEXT NOT NULL DEFAULT 'cross_agent',
			decision_a_id TEXT NOT NULL,
			decision_b_id TEXT NOT NULL,
			org_id TEXT NOT NULL,
			agent_a TEXT NOT NULL,
			agent_b TEXT NOT NULL,
			decision_type_a TEXT NOT NULL DEFAULT '',
			decision_type_b TEXT NOT NULL DEFAULT '',
			outcome_a TEXT NOT NULL DEFAULT '',
			outcome_b TEXT NOT NULL DEFAULT '',
			topic_similarity REAL,
			outcome_divergence REAL,
			significance REAL,
			scoring_method TEXT NOT NULL DEFAULT '',
			explanation TEXT,
			detected_at TEXT NOT NULL DEFAULT (datetime('now')),
			severity TEXT,
			status TEXT NOT NULL DEFAULT 'open',
			resolved_by TEXT,
			resolved_at TEXT,
			resolution_note TEXT,
			relationship TEXT,
			confidence_weight REAL,
			temporal_decay REAL,
			resolution_decision_id TEXT,
			winning_decision_id TEXT,
			group_id TEXT,
			UNIQUE(decision_a_id, decision_b_id)
		);
		CREATE TABLE conflict_groups (
			id TEXT PRIMARY KEY,
			org_id TEXT NOT NULL,
			agent_a TEXT NOT NULL,
			agent_b TEXT NOT NULL,
			conflict_kind TEXT NOT NULL DEFAULT 'cross_agent',
			decision_type TEXT NOT NULL DEFAULT '',
			group_topic TEXT,
			first_detected_at TEXT NOT NULL DEFAULT (datetime('now')),
			last_detected_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE decision_supersedes (
			superseding_id TEXT NOT NULL,
			superseded_id TEXT NOT NULL,
			org_id TEXT NOT NULL,
			relationship TEXT NOT NULL DEFAULT 'supersedes',
			is_primary INTEGER NOT NULL DEFAULT 0,
			recorded_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (superseding_id, superseded_id)
		)`)
	require.NoError(t, err)
	return db
}

func insertTestDecision(t *testing.T, db *sql.DB, id, orgID uuid.UUID, agentID, decType, outcome string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, valid_from)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id.String(), orgID.String(), agentID, decType, outcome, 0.9,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)
}

// insertSupersedingDecision inserts a decision that supersedes another. The
// caller must explicitly set the superseded decision's valid_to in tests where
// that side effect matters — this helper does not invalidate the target,
// because some tests need to verify chain exclusion independently of the
// valid_to filter (which would otherwise mask the presence or absence of the
// chain check).
func insertSupersedingDecision(t *testing.T, db *sql.DB, id, orgID uuid.UUID, agentID, decType, outcome string, supersedesID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, supersedes_id, valid_from)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), orgID.String(), agentID, decType, outcome, 0.9,
		supersedesID.String(),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	require.NoError(t, err)
}

func insertSupersedesEdge(t *testing.T, db *sql.DB, supersedingID, supersededID, orgID uuid.UUID, relationship string, isPrimary bool) {
	t.Helper()
	primary := 0
	if isPrimary {
		primary = 1
	}
	_, err := db.Exec(
		`INSERT INTO decision_supersedes (superseding_id, superseded_id, org_id, relationship, is_primary)
		 VALUES (?, ?, ?, ?, ?)`,
		supersedingID.String(), supersededID.String(), orgID.String(), relationship, primary,
	)
	require.NoError(t, err)
}

func TestLiteScorer_NoConflictDifferentTypes(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	d1 := uuid.New()
	d2 := uuid.New()
	insertTestDecision(t, db, d1, orgID, "agent-a", "architecture", "Use PostgreSQL for persistent storage with connection pooling and read replicas")
	insertTestDecision(t, db, d2, orgID, "agent-b", "code_review", "The implementation looks correct and follows established patterns")

	scorer.ScoreForDecision(ctx, d1, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "different decision types should not produce conflicts")
}

func TestLiteScorer_DetectsContradiction(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	d1 := uuid.New()
	d2 := uuid.New()

	// Two agents make contradicting architecture decisions about the same topic.
	insertTestDecision(t, db, d1, orgID, "agent-a", "architecture",
		"Use PostgreSQL for the primary database with read replicas and connection pooling for high availability")
	insertTestDecision(t, db, d2, orgID, "agent-b", "architecture",
		"Use MongoDB for the primary database with sharding and replica sets for horizontal scalability")

	scorer.ScoreForDecision(ctx, d2, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 1, count, "contradicting decisions should produce a conflict")

	// Verify the conflict details.
	var conflictKind, severity, status, scoringMethod string
	require.NoError(t, db.QueryRow(
		"SELECT conflict_kind, severity, status, scoring_method FROM scored_conflicts",
	).Scan(&conflictKind, &severity, &status, &scoringMethod))
	assert.Equal(t, "cross_agent", conflictKind)
	assert.Equal(t, "open", status)
	assert.Equal(t, "text_claims", scoringMethod)
}

func TestLiteScorer_NoDuplicateConflicts(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	d1 := uuid.New()
	d2 := uuid.New()

	insertTestDecision(t, db, d1, orgID, "agent-a", "architecture",
		"Use PostgreSQL for the primary database with read replicas and connection pooling for high availability")
	insertTestDecision(t, db, d2, orgID, "agent-b", "architecture",
		"Use MongoDB for the primary database with sharding and replica sets for horizontal scalability")

	// Score twice — should not create duplicate conflicts.
	scorer.ScoreForDecision(ctx, d2, orgID)
	scorer.ScoreForDecision(ctx, d2, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.LessOrEqual(t, count, 1, "should not create duplicate conflicts")
}

func TestLiteScorer_SelfContradiction(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	d1 := uuid.New()
	d2 := uuid.New()

	// Same agent makes contradicting decisions.
	insertTestDecision(t, db, d1, orgID, "agent-a", "architecture",
		"Use PostgreSQL for the primary database with read replicas and connection pooling for high availability")
	insertTestDecision(t, db, d2, orgID, "agent-a", "architecture",
		"Use MongoDB for the primary database with sharding and replica sets for horizontal scalability")

	scorer.ScoreForDecision(ctx, d2, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts WHERE conflict_kind = 'self_contradiction'").Scan(&count))
	assert.Equal(t, 1, count, "same agent contradicting itself should be self_contradiction")
}

func TestLiteScorer_ConflictGroupCreated(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	d1 := uuid.New()
	d2 := uuid.New()

	insertTestDecision(t, db, d1, orgID, "agent-a", "architecture",
		"Use PostgreSQL for the primary database with read replicas and connection pooling for high availability")
	insertTestDecision(t, db, d2, orgID, "agent-b", "architecture",
		"Use MongoDB for the primary database with sharding and replica sets for horizontal scalability")

	scorer.ScoreForDecision(ctx, d2, orgID)

	var groupCount int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM conflict_groups").Scan(&groupCount))
	assert.Equal(t, 1, groupCount, "a conflict group should be created")

	// Verify group_id is set on the conflict.
	var groupIDStr sql.NullString
	require.NoError(t, db.QueryRow("SELECT group_id FROM scored_conflicts").Scan(&groupIDStr))
	assert.True(t, groupIDStr.Valid, "conflict should reference a group")
}

func TestLiteScorer_NoConflictForShortOutcomes(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	d1 := uuid.New()
	d2 := uuid.New()

	// Short outcomes produce no claims (below 20-char threshold).
	insertTestDecision(t, db, d1, orgID, "agent-a", "code_review", "LGTM")
	insertTestDecision(t, db, d2, orgID, "agent-b", "code_review", "Approved")

	scorer.ScoreForDecision(ctx, d2, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "boilerplate/short outcomes should not produce conflicts")
}

func TestScoreClaimOverlap_IdenticalOutcomes(t *testing.T) {
	claims := SplitClaims("Use PostgreSQL for the primary database with read replicas for high availability")
	topicSim, divergence, _ := scoreClaimOverlap(claims, claims, "same outcome", "same outcome")
	assert.InDelta(t, 1.0, topicSim, 0.01, "identical outcomes should have full topic similarity")
	assert.InDelta(t, 0.0, divergence, 0.01, "identical outcomes should have zero divergence")
}

func TestScoreClaimOverlap_TotallyDifferent(t *testing.T) {
	claimsA := SplitClaims("Use PostgreSQL for database storage with connection pooling and read replicas for high availability")
	claimsB := SplitClaims("Implement comprehensive unit testing with full coverage of edge cases and integration scenarios")
	topicSim, _, _ := scoreClaimOverlap(claimsA, claimsB,
		"Use PostgreSQL for database storage with connection pooling",
		"Implement comprehensive unit testing with full coverage")
	assert.Less(t, topicSim, float32(0.3), "completely different topics should have low similarity")
}

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]bool
		want float32
	}{
		{"identical", map[string]bool{"a": true, "b": true}, map[string]bool{"a": true, "b": true}, 1.0},
		{"disjoint", map[string]bool{"a": true}, map[string]bool{"b": true}, 0.0},
		{"half_overlap", map[string]bool{"a": true, "b": true}, map[string]bool{"b": true, "c": true}, 1.0 / 3.0},
		{"empty_a", map[string]bool{}, map[string]bool{"a": true}, 0.0},
		{"empty_b", map[string]bool{"a": true}, map[string]bool{}, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jaccardSimilarity(tt.a, tt.b)
			assert.InDelta(t, tt.want, got, 0.001)
		})
	}
}

func TestUniqueWords(t *testing.T) {
	words := uniqueWords("Use PostgreSQL for the database! Short a b c words.")
	// Should include 3+ char words, lowercased, punctuation stripped.
	assert.True(t, words["use"])
	assert.True(t, words["postgresql"])
	assert.True(t, words["database"])
	assert.True(t, words["words"])
	// Should exclude short words.
	assert.False(t, words["a"])
	assert.False(t, words["b"])
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "abc...", compact.Truncate("abcdef", 3))
	assert.Equal(t, "ab", compact.Truncate("ab", 3))
	assert.Equal(t, "", compact.Truncate("", 3))
}

// TestScoreClaimOverlap_EmptyClaims verifies that empty claim slices return zeros.
func TestScoreClaimOverlap_EmptyClaims(t *testing.T) {
	topicSim, outcomeDivergence, explanation := scoreClaimOverlap(nil, []string{"claim"}, "a", "b")
	assert.Equal(t, float32(0), topicSim)
	assert.Equal(t, float32(0), outcomeDivergence)
	assert.Empty(t, explanation)

	topicSim, outcomeDivergence, explanation = scoreClaimOverlap([]string{"claim"}, nil, "a", "b")
	assert.Equal(t, float32(0), topicSim)
	assert.Equal(t, float32(0), outcomeDivergence)
	assert.Empty(t, explanation)

	topicSim, outcomeDivergence, explanation = scoreClaimOverlap([]string{}, []string{}, "a", "b")
	assert.Equal(t, float32(0), topicSim)
	assert.Equal(t, float32(0), outcomeDivergence)
	assert.Empty(t, explanation)
}

// TestScoreClaimOverlap_DissimilarTopics verifies that outcomes with low word overlap
// return low topic similarity and zero divergence.
func TestScoreClaimOverlap_DissimilarTopics(t *testing.T) {
	claimsA := []string{"PostgreSQL provides strong ACID guarantees"}
	claimsB := []string{"quantum entanglement exhibits nonlocal correlations"}
	topicSim, outcomeDivergence, explanation := scoreClaimOverlap(
		claimsA, claimsB,
		"PostgreSQL provides strong ACID guarantees",
		"quantum entanglement exhibits nonlocal correlations",
	)
	assert.Less(t, topicSim, float32(0.15), "dissimilar topics should have low topic similarity")
	assert.Equal(t, float32(0), outcomeDivergence)
	assert.Contains(t, explanation, "dissimilar")
}

// TestScoreClaimOverlap_SameOutcomesHighTopicSim verifies that identical outcomes
// have high topic similarity but zero divergence.
func TestScoreClaimOverlap_SameOutcomesHighTopicSim(t *testing.T) {
	outcome := "Use PostgreSQL for the primary database with connection pooling and read replicas"
	claims := SplitClaims(outcome)
	if len(claims) == 0 {
		claims = []string{outcome}
	}

	topicSim, _, _ := scoreClaimOverlap(claims, claims, outcome, outcome)
	assert.Greater(t, topicSim, float32(0.5), "identical outcomes should have high topic similarity")
}

// TestJaccardSimilarity_EmptySets verifies edge cases for empty word sets.
func TestJaccardSimilarity_EmptySets(t *testing.T) {
	assert.Equal(t, float32(0), jaccardSimilarity(nil, nil))
	assert.Equal(t, float32(0), jaccardSimilarity(map[string]bool{}, map[string]bool{}))
	assert.Equal(t, float32(0), jaccardSimilarity(map[string]bool{"abc": true}, nil))
	assert.Equal(t, float32(0), jaccardSimilarity(nil, map[string]bool{"abc": true}))
}

// TestJaccardSimilarity_IdenticalSets verifies that identical word sets return 1.0.
func TestJaccardSimilarity_IdenticalSets(t *testing.T) {
	words := map[string]bool{"postgresql": true, "database": true, "storage": true}
	assert.InDelta(t, 1.0, float64(jaccardSimilarity(words, words)), 0.001)
}

// TestJaccardSimilarity_DisjointSets verifies that disjoint word sets return 0.
func TestJaccardSimilarity_DisjointSets(t *testing.T) {
	a := map[string]bool{"postgresql": true, "database": true}
	b := map[string]bool{"react": true, "frontend": true}
	assert.Equal(t, float32(0), jaccardSimilarity(a, b))
}

// TestJaccardSimilarity_PartialOverlap verifies partial overlap calculation.
func TestJaccardSimilarity_PartialOverlap(t *testing.T) {
	a := map[string]bool{"postgresql": true, "database": true, "storage": true}
	b := map[string]bool{"database": true, "storage": true, "mongodb": true}
	// intersection = 2, union = 4
	assert.InDelta(t, 0.5, float64(jaccardSimilarity(a, b)), 0.001)
}

// TestUniqueWords_PunctuationAndLength verifies word extraction with punctuation stripping and length filtering.
func TestUniqueWords_PunctuationAndLength(t *testing.T) {
	words := uniqueWords("Use PostgreSQL for the primary database, with connection-pooling!")
	assert.True(t, words["use"])
	assert.True(t, words["postgresql"])
	assert.True(t, words["primary"])
	assert.True(t, words["database"])
	assert.True(t, words["connection-pooling"] || words["pooling"])
	// Short words (< 3 chars) should be filtered.
	assert.False(t, words[""])
	assert.False(t, words["a"])
}

// TestUniqueWords_EmptyString verifies that empty input produces empty output.
func TestUniqueWords_EmptyString(t *testing.T) {
	words := uniqueWords("")
	assert.Empty(t, words)
}

// TestTruncate_LiteScorer verifies rune-safe truncation via compact.Truncate.
func TestTruncate_LiteScorer(t *testing.T) {
	assert.Equal(t, "hello", compact.Truncate("hello", 10))
	assert.Equal(t, "hel...", compact.Truncate("hello", 3))
	assert.Equal(t, "", compact.Truncate("", 5))
	assert.Equal(t, "hello", compact.Truncate("hello", 5))
	// Multi-byte: rune-safe, no split mid-codepoint.
	assert.Equal(t, "こん...", compact.Truncate("こんにちは", 2))
}

// insertTestDecisionWithProject inserts a decision with an explicit project tag.
// Passing project == "" inserts a NULL project (matches Postgres' "untagged"
// semantic via sql.NullString).
func insertTestDecisionWithProject(t *testing.T, db *sql.DB, id, orgID uuid.UUID, agentID, decType, outcome, project string) {
	t.Helper()
	var proj sql.NullString
	if project != "" {
		proj = sql.NullString{String: project, Valid: true}
	}
	_, err := db.Exec(
		`INSERT INTO decisions (id, org_id, agent_id, decision_type, outcome, confidence, valid_from, project)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), orgID.String(), agentID, decType, outcome, 0.9,
		time.Now().UTC().Format(time.RFC3339Nano), proj,
	)
	require.NoError(t, err)
}

// Two contradicting architecture outcomes used as a fixed pair across the
// project-scoping tests below. Pre-fix, this pair scored a conflict in every
// project combination the scorer encountered; post-fix, it scores a conflict
// only when both decisions are in the same project (or both untagged).
const (
	pgProjectOutcome        = "Use PostgreSQL for the primary database with read replicas and connection pooling for high availability"
	mongoProjectOutcome     = "Use MongoDB for the primary database with sharding and replica sets for horizontal scalability"
	cassandraProjectOutcome = "Use Cassandra for the primary database with multi-datacenter replication and tunable consistency"
)

// TestLiteScorer_ProjectScoping_DifferentProjects verifies the issue #714 fix:
// a decision in project A must NOT generate a conflict against a contradicting
// decision in project B. Pre-fix the SQL was `project = ? OR project IS NULL`,
// which matched same-project rows but the test previously tolerated a 0-or-1
// outcome because the strict equality contract was not yet enforced.
func TestLiteScorer_ProjectScoping_DifferentProjects(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	srcID := uuid.New()
	otherID := uuid.New()
	insertTestDecisionWithProject(t, db, srcID, orgID, "agent-a", "architecture", pgProjectOutcome, "project-alpha")
	insertTestDecisionWithProject(t, db, otherID, orgID, "agent-b", "architecture", mongoProjectOutcome, "project-beta")

	scorer.ScoreForDecision(ctx, srcID, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "decisions in different non-null projects must not generate a conflict")
}

// TestLiteScorer_ProjectScoping_TaggedSourceUntaggedCandidate verifies that a
// project-tagged source does NOT pull in an untagged candidate. Pre-fix this
// was the dominant cross-project leak path: `project = ? OR project IS NULL`
// matched every untagged row in the org. Post-fix the source's project must
// match exactly.
func TestLiteScorer_ProjectScoping_TaggedSourceUntaggedCandidate(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	srcID := uuid.New()
	otherID := uuid.New()
	insertTestDecisionWithProject(t, db, srcID, orgID, "agent-a", "architecture", pgProjectOutcome, "project-alpha")
	insertTestDecisionWithProject(t, db, otherID, orgID, "agent-b", "architecture", mongoProjectOutcome, "")

	scorer.ScoreForDecision(ctx, srcID, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "project-tagged source must not match untagged candidates")
}

// TestLiteScorer_ProjectScoping_UntaggedSourceTaggedCandidate verifies the
// symmetric case: when the source has no project, the scorer must NOT pull in
// project-tagged candidates. Pre-fix the source-has-no-project branch applied
// no project filter at all, so an untagged source compared against every row
// in the org regardless of project — the most aggressive cross-project leak.
func TestLiteScorer_ProjectScoping_UntaggedSourceTaggedCandidate(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	srcID := uuid.New()
	otherID := uuid.New()
	insertTestDecisionWithProject(t, db, srcID, orgID, "agent-a", "architecture", pgProjectOutcome, "")
	insertTestDecisionWithProject(t, db, otherID, orgID, "agent-b", "architecture", mongoProjectOutcome, "project-alpha")

	scorer.ScoreForDecision(ctx, srcID, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "untagged source must not match project-tagged candidates")
}

// TestLiteScorer_ProjectScoping_SameProjectStillConflicts is the negative
// control: tightening the project filter must not suppress real same-project
// conflicts. Two contradicting decisions in project-alpha must still produce
// one conflict — this test is what would catch an over-eager filter that
// accidentally dropped same-project pairs (e.g., an `AND project != ?` typo).
func TestLiteScorer_ProjectScoping_SameProjectStillConflicts(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	srcID := uuid.New()
	otherID := uuid.New()
	insertTestDecisionWithProject(t, db, srcID, orgID, "agent-a", "architecture", pgProjectOutcome, "project-alpha")
	insertTestDecisionWithProject(t, db, otherID, orgID, "agent-b", "architecture", mongoProjectOutcome, "project-alpha")

	scorer.ScoreForDecision(ctx, srcID, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 1, count, "contradicting decisions in the same project must still produce a conflict")
}

// TestLiteScorer_ProjectScoping_BothUntaggedStillConflicts is the second
// negative control: two untagged decisions must still be compared against each
// other. Pre-fix the untagged-source branch applied no project filter, so this
// case worked by accident; post-fix the filter is explicit (`project IS NULL`)
// and we need a regression test to lock the behavior.
func TestLiteScorer_ProjectScoping_BothUntaggedStillConflicts(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	srcID := uuid.New()
	otherID := uuid.New()
	insertTestDecisionWithProject(t, db, srcID, orgID, "agent-a", "architecture", pgProjectOutcome, "")
	insertTestDecisionWithProject(t, db, otherID, orgID, "agent-b", "architecture", mongoProjectOutcome, "")

	scorer.ScoreForDecision(ctx, srcID, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 1, count, "contradicting decisions both untagged must still produce a conflict")
}

// TestLiteScorer_ProjectScoping_MixedPoolPicksOnlySameProject combines the
// three negative cases above into one pool to verify that with five candidates
// spanning project-alpha / project-beta / untagged, scoring a project-alpha
// source produces exactly one conflict — against the other project-alpha
// candidate — and ignores the other three.
func TestLiteScorer_ProjectScoping_MixedPoolPicksOnlySameProject(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	srcID := uuid.New()
	sameProjectID := uuid.New()
	otherProjectID := uuid.New()
	untaggedID := uuid.New()

	insertTestDecisionWithProject(t, db, srcID, orgID, "agent-a", "architecture", pgProjectOutcome, "project-alpha")
	insertTestDecisionWithProject(t, db, sameProjectID, orgID, "agent-b", "architecture", mongoProjectOutcome, "project-alpha")
	insertTestDecisionWithProject(t, db, otherProjectID, orgID, "agent-c", "architecture", cassandraProjectOutcome, "project-beta")
	insertTestDecisionWithProject(t, db, untaggedID, orgID, "agent-d", "architecture", cassandraProjectOutcome, "")

	scorer.ScoreForDecision(ctx, srcID, orgID)

	rows, err := db.Query("SELECT decision_a_id, decision_b_id FROM scored_conflicts")
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck

	var pairs [][2]string
	for rows.Next() {
		var a, b string
		require.NoError(t, rows.Scan(&a, &b))
		pairs = append(pairs, [2]string{a, b})
	}
	require.NoError(t, rows.Err())

	require.Len(t, pairs, 1, "exactly one same-project conflict must be produced from the mixed pool")
	got := pairs[0]
	wantSrc := srcID.String()
	wantOther := sameProjectID.String()
	matched := (got[0] == wantSrc && got[1] == wantOther) || (got[0] == wantOther && got[1] == wantSrc)
	assert.True(t, matched, "the produced conflict must be between the source and the same-project candidate; got %v", got)
}

// TestLiteScorer_RevisionChainExcluded verifies that a decision in the source's
// supersedes chain is NOT flagged as a conflict, even when its outcome diverges
// from the source's. This guards the contract independently of the valid_to
// filter — the test inserts the chain ancestor as still-active so a chain-blind
// scorer would create a conflict.
func TestLiteScorer_RevisionChainExcluded(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	older := uuid.New()
	newer := uuid.New()
	insertTestDecision(t, db, older, orgID, "agent-a", "architecture",
		"Use PostgreSQL for persistent storage with connection pooling and read replicas")
	insertSupersedingDecision(t, db, newer, orgID, "agent-a", "architecture",
		"Use MongoDB for persistent storage with sharding and replica sets",
		older,
	)

	scorer.ScoreForDecision(ctx, newer, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "the older decision is in newer's supersedes chain — not a conflict")
}

// TestLiteScorer_TransitiveChainExcluded verifies multi-hop chain walking.
// A → B → C: when C is scored, both A and B should be excluded as chain
// members even though the only direct supersedes pointer on C points at B.
func TestLiteScorer_TransitiveChainExcluded(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	a := uuid.New()
	b := uuid.New()
	c := uuid.New()
	insertTestDecision(t, db, a, orgID, "agent-a", "architecture",
		"Use Redis for caching with 5-minute TTL and LRU eviction")
	insertSupersedingDecision(t, db, b, orgID, "agent-a", "architecture",
		"Use Memcached for caching with 10-minute TTL and consistent hashing",
		a,
	)
	insertSupersedingDecision(t, db, c, orgID, "agent-a", "architecture",
		"Use Hazelcast for caching with 1-minute TTL and partition-aware routing",
		b,
	)

	scorer.ScoreForDecision(ctx, c, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "transitive chain ancestors A and B must both be excluded when scoring C")
}

// TestLiteScorer_BackwardChainExcluded verifies that walking backward from the
// source (decisions the source supersedes) is also excluded. This direction
// matters when the scorer is invoked on an older decision that has been
// superseded by a newer one — the newer decision must not be flagged as a
// conflict against the older one.
func TestLiteScorer_BackwardChainExcluded(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	older := uuid.New()
	newer := uuid.New()
	insertTestDecision(t, db, older, orgID, "agent-a", "architecture",
		"Use PostgreSQL for persistent storage with connection pooling and read replicas")
	insertSupersedingDecision(t, db, newer, orgID, "agent-a", "architecture",
		"Use MongoDB for persistent storage with sharding and replica sets",
		older,
	)

	// Score the older decision. The newer (which supersedes it) is reachable
	// via the forward direction of the chain walk and must be excluded.
	scorer.ScoreForDecision(ctx, older, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "the newer decision supersedes older — not a conflict in either direction")
}

// TestLiteScorer_UnrelatedDecisionStillConflicts verifies that the chain
// exclusion is targeted: a decision NOT in the chain still produces a conflict
// when its outcome diverges from the source. Guards against the chain query
// returning a too-wide result (e.g., walking via NULL joins) that would mask
// real conflicts.
func TestLiteScorer_UnrelatedDecisionStillConflicts(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	chainOlder := uuid.New()
	chainNewer := uuid.New()
	unrelated := uuid.New()

	insertTestDecision(t, db, chainOlder, orgID, "agent-a", "architecture",
		"Use PostgreSQL for persistent storage with connection pooling and read replicas")
	insertSupersedingDecision(t, db, chainNewer, orgID, "agent-a", "architecture",
		"Use MongoDB for persistent storage with sharding and replica sets",
		chainOlder,
	)
	// Unrelated decision: same topic, different agent, no supersedes link.
	insertTestDecision(t, db, unrelated, orgID, "agent-b", "architecture",
		"Use Cassandra for persistent storage with multi-datacenter replication and tunable consistency")

	scorer.ScoreForDecision(ctx, chainNewer, orgID)

	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM scored_conflicts
		 WHERE (decision_a_id = ? AND decision_b_id = ?)
		    OR (decision_a_id = ? AND decision_b_id = ?)`,
		chainNewer.String(), unrelated.String(),
		unrelated.String(), chainNewer.String(),
	).Scan(&count))
	assert.Equal(t, 1, count, "unrelated decision with no chain link must still conflict")
}

func TestLiteScorer_DecisionSupersedesEdgeExcluded(t *testing.T) {
	db := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scorer := NewLiteScorer(db, logger)
	ctx := context.Background()
	orgID := uuid.New()

	a := uuid.New()
	b := uuid.New()
	c := uuid.New()
	insertTestDecision(t, db, a, orgID, "agent-a", "architecture",
		"Use PostgreSQL for persistent storage with connection pooling and read replicas")
	insertTestDecision(t, db, b, orgID, "agent-b", "architecture",
		"Use MySQL for persistent storage with read replicas and connection pooling")
	insertTestDecision(t, db, c, orgID, "agent-c", "architecture",
		"Use MongoDB for persistent storage with sharding and replica sets")

	insertSupersedesEdge(t, db, c, a, orgID, "reconciles", true)
	insertSupersedesEdge(t, db, c, b, orgID, "reconciles", false)

	scorer.ScoreForDecision(ctx, c, orgID)

	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scored_conflicts").Scan(&count))
	assert.Equal(t, 0, count, "decision_supersedes join-table targets must be excluded even without direct supersedes_id")
}
