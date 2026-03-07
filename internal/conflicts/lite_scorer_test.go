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
			valid_from TEXT NOT NULL,
			valid_to TEXT,
			project TEXT
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
			first_detected_at TEXT NOT NULL DEFAULT (datetime('now')),
			last_detected_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(org_id, agent_a, agent_b, conflict_kind, decision_type)
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
	assert.Equal(t, "abc", truncate("abcdef", 3))
	assert.Equal(t, "ab", truncate("ab", 3))
	assert.Equal(t, "", truncate("", 3))
}
