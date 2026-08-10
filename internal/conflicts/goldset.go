//go:build !lite

package conflicts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// goldQuerier is the subset of pgx.Tx / pgxpool.Pool needed to read gold
// labels. Declared here rather than taking a concrete pool so the eval can run
// against a pool, a transaction, or a test double.
type goldQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// The gold set is the corpus-derived evaluation dataset for the validator.
//
// It exists because the hardcoded DefaultEvalDataset scored 1.000/1.000 while
// the live detector was running at ~3% precision: a dataset written from the
// same intuitions as the prompt cannot falsify that prompt. Gold labels are
// blind classifications of REAL scored pairs — the rater sees only the two
// decision texts and their structural metadata, never the pipeline's verdict.
//
// Labels live in the conflict_gold_labels table (see migration 107). Inter-rater
// agreement on a 200-pair re-rate was Cohen's kappa 0.766 for
// contradiction-vs-rest, which is the ceiling any validator change should be
// measured against: a judge cannot be meaningfully "better" than the label noise.
const (
	GoldContradiction = "contradiction"
	GoldSupersession  = "supersession"
	GoldRelated       = "related_not_contradicting"
	GoldUnrelated     = "unrelated"
	GoldInsufficient  = "insufficient"
)

// goldToExpected maps a gold label onto the validator's relationship vocabulary.
//
// The gold taxonomy deliberately has one "related but compatible" bucket where
// the validator has two (complementary and refinement). Collapsing them is not
// a loss: the split is not reliably separable by human or model raters, and
// both sides are non-conflicts, so no routing decision depends on it. Scoring
// treats the pair as equivalent via relationshipEquivalent.
var goldToExpected = map[string]string{
	GoldContradiction: "contradiction",
	GoldSupersession:  "supersession",
	GoldRelated:       "complementary",
	GoldUnrelated:     "unrelated",
}

// RelationshipEquivalent reports whether an actual relationship should count as
// a hit for the expected one. Exact match, plus complementary/refinement
// equivalence (see goldToExpected).
func RelationshipEquivalent(expected, actual string) bool {
	if expected == actual {
		return true
	}
	related := map[string]bool{"complementary": true, "refinement": true}
	return related[expected] && related[actual]
}

// LoadGoldPairs reads labeled pairs from conflict_gold_labels joined to the
// scored_conflicts and decisions rows they describe, returning them as EvalPairs
// ready to run through a validator.
//
// Pairs labeled "insufficient" are excluded: the rater judged the stored text
// too truncated to classify, so they measure the corpus, not the judge. Passing
// limit <= 0 loads every eligible pair.
func LoadGoldPairs(ctx context.Context, db goldQuerier, orgID string, limit int) ([]EvalPair, error) {
	const query = `
SELECT sc.id, g.label,
       sc.outcome_a, sc.outcome_b,
       sc.decision_type_a, sc.decision_type_b,
       sc.agent_a, sc.agent_b,
       da.created_at, db.created_at,
       COALESCE(da.reasoning, ''), COALESCE(db.reasoning, ''),
       COALESCE(sc.project_a, ''), COALESCE(sc.project_b, ''),
       COALESCE(da.session_id::text, ''), COALESCE(db.session_id::text, ''),
       COALESCE(sc.topic_similarity, 0)
FROM conflict_gold_labels g
JOIN scored_conflicts sc ON sc.id = g.scored_conflict_id
JOIN decisions da ON da.id = sc.decision_a_id
JOIN decisions db ON db.id = sc.decision_b_id
WHERE g.org_id = $1 AND g.label <> $2
ORDER BY sc.detected_at`

	q := query
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(ctx, q, orgID, GoldInsufficient)
	if err != nil {
		return nil, fmt.Errorf("load gold pairs: %w", err)
	}
	defer rows.Close()

	var pairs []EvalPair
	for rows.Next() {
		var (
			id, label     string
			in            ValidateInput
			createdA      time.Time
			createdB      time.Time
			topicSim      float64
			sessA, sessB  string
			projA, projB  string
			reasonA, reaB string
		)
		if err := rows.Scan(&id, &label,
			&in.OutcomeA, &in.OutcomeB,
			&in.TypeA, &in.TypeB,
			&in.AgentA, &in.AgentB,
			&createdA, &createdB,
			&reasonA, &reaB,
			&projA, &projB,
			&sessA, &sessB,
			&topicSim,
		); err != nil {
			return nil, fmt.Errorf("load gold pairs: scan: %w", err)
		}

		expected, ok := goldToExpected[label]
		if !ok {
			return nil, fmt.Errorf("load gold pairs: unrecognized gold label %q on conflict %s", label, id)
		}

		in.CreatedA, in.CreatedB = createdA, createdB
		in.ReasoningA, in.ReasoningB = reasonA, reaB
		in.ProjectA, in.ProjectB = projA, projB
		in.SessionIDA, in.SessionIDB = sessA, sessB
		in.TopicSimilarity = topicSim

		pairs = append(pairs, EvalPair{
			Label:                fmt.Sprintf("gold/%s/%s", label, id[:8]),
			Input:                in,
			ExpectedRelationship: expected,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load gold pairs: iterate: %w", err)
	}
	return pairs, nil
}

// GoldSummary counts pairs by expected relationship, for reporting the shape of
// an eval run. Base rate matters when reading precision: contradictions are
// ~3% of the corpus, so a judge that answers "contradiction" often will look
// confident and be wrong.
func GoldSummary(pairs []EvalPair) string {
	counts := map[string]int{}
	for _, p := range pairs {
		counts[p.ExpectedRelationship]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gold set: %d pairs", len(pairs))
	for _, rel := range []string{"contradiction", "supersession", "complementary", "unrelated"} {
		if n := counts[rel]; n > 0 {
			fmt.Fprintf(&b, "  %s=%d (%.1f%%)", rel, n, float64(n)/float64(len(pairs))*100)
		}
	}
	return b.String()
}
