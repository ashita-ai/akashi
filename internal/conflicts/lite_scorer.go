package conflicts

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/compact"
	"github.com/ashita-ai/akashi/internal/storage"
)

// LiteScorer provides text-based conflict detection for lite-mode (no embeddings,
// no LLM). It compares decisions within the same org and decision_type by extracting
// claims from outcomes and measuring text overlap. When outcomes share the same topic
// but have divergent content, a conflict is created.
//
// This is intentionally simple — it catches obvious contradictions (different agents
// making different claims about the same topic) without requiring any external services.
// For more sophisticated conflict detection, use the full Scorer with embeddings + LLM.
type LiteScorer struct {
	db     *sql.DB
	logger *slog.Logger
}

// NewLiteScorer creates a LiteScorer backed by the given sql.DB.
func NewLiteScorer(db *sql.DB, logger *slog.Logger) *LiteScorer {
	return &LiteScorer{db: db, logger: logger}
}

// ScoreForDecision finds and scores potential conflicts for a newly traced decision.
// It compares against recent same-type decisions from different agents.
func (s *LiteScorer) ScoreForDecision(ctx context.Context, decisionID, orgID uuid.UUID) {
	// 1. Load the source decision.
	src, err := s.loadDecision(ctx, decisionID, orgID)
	if err != nil {
		s.logger.Debug("lite conflict scorer: skip decision", "decision_id", decisionID, "error", err)
		return
	}

	// Extract claims from the source decision.
	srcClaims := SplitClaims(src.outcome)
	if len(srcClaims) == 0 {
		return
	}

	// 2. Load recent same-type decisions from other agents (candidate pool).
	candidates, err := s.loadCandidates(ctx, orgID, src)
	if err != nil {
		s.logger.Warn("lite conflict scorer: load candidates failed", "decision_id", decisionID, "error", err)
		return
	}
	if len(candidates) == 0 {
		return
	}

	// 3. Score each candidate pair.
	for _, cand := range candidates {
		candClaims := SplitClaims(cand.outcome)
		if len(candClaims) == 0 {
			continue
		}

		topicSim, outcomeDivergence, explanation := scoreClaimOverlap(srcClaims, candClaims, src.outcome, cand.outcome)

		// Threshold: need meaningful topic overlap with outcome divergence.
		if topicSim < 0.3 || outcomeDivergence < 0.2 {
			continue
		}

		significance := topicSim * outcomeDivergence

		// Check for existing conflict between this pair (either direction).
		exists, err := s.conflictExists(ctx, decisionID, cand.id)
		if err != nil {
			s.logger.Warn("lite conflict scorer: check existing conflict", "error", err)
			continue
		}
		if exists {
			continue
		}

		// Determine conflict kind.
		conflictKind := "cross_agent"
		if src.agentID == cand.agentID {
			conflictKind = "self_contradiction"
		}

		// Compute severity from decision metadata (type tier, category),
		// independent of the significance score. See ADR-015.
		severity := ComputeSeverity(SeverityInput{
			DecisionTypeA: src.decisionType,
			DecisionTypeB: cand.decisionType,
			// LiteScorer has no access to decision confidence; zero values
			// are treated as "unknown" by ComputeSeverity (no demotion).
		})
		if severity == "" {
			severity = "low"
		}

		if err := s.insertConflict(ctx, orgID, conflictKind, src, cand, topicSim, outcomeDivergence, significance, severity, explanation); err != nil {
			s.logger.Warn("lite conflict scorer: insert conflict", "error", err)
			continue
		}
		s.logger.Info("lite conflict scored",
			"decision_a", decisionID,
			"decision_b", cand.id,
			"topic_similarity", topicSim,
			"outcome_divergence", outcomeDivergence,
			"significance", significance,
		)
	}
}

type liteDecision struct {
	id              uuid.UUID
	agentID         string
	decisionType    string
	outcome         string
	project         *string
	transactionTime time.Time
}

func (s *LiteScorer) loadDecision(ctx context.Context, id, orgID uuid.UUID) (liteDecision, error) {
	var d liteDecision
	var idStr string
	var project sql.NullString
	var txTime string

	err := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, decision_type, outcome, project, transaction_time
		 FROM decisions WHERE id = ? AND org_id = ? AND valid_to IS NULL`,
		id.String(), orgID.String(),
	).Scan(&idStr, &d.agentID, &d.decisionType, &d.outcome, &project, &txTime)
	if err != nil {
		return liteDecision{}, fmt.Errorf("load decision: %w", err)
	}
	d.id, _ = uuid.Parse(idStr)
	if project.Valid {
		d.project = &project.String
	}
	d.transactionTime, _ = time.Parse(time.RFC3339Nano, txTime)
	return d, nil
}

func (s *LiteScorer) loadCandidates(ctx context.Context, orgID uuid.UUID, src liteDecision) ([]liteDecision, error) {
	// Load recent same-type decisions (last 50) from any agent.
	// Excludes the source decision and superseded decisions.
	q := `SELECT id, agent_id, decision_type, outcome, project, transaction_time
	      FROM decisions
	      WHERE org_id = ? AND decision_type = ? AND id != ?
	        AND valid_to IS NULL`
	args := []any{orgID.String(), src.decisionType, src.id.String()}

	// Scope to same project if the source has one.
	if src.project != nil {
		q += ` AND (project = ? OR project IS NULL)`
		args = append(args, *src.project)
	}

	q += ` ORDER BY valid_from DESC LIMIT 50`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []liteDecision
	for rows.Next() {
		var d liteDecision
		var idStr string
		var project sql.NullString
		var txTime string
		if err := rows.Scan(&idStr, &d.agentID, &d.decisionType, &d.outcome, &project, &txTime); err != nil {
			return nil, err
		}
		d.id, _ = uuid.Parse(idStr)
		if project.Valid {
			d.project = &project.String
		}
		d.transactionTime, _ = time.Parse(time.RFC3339Nano, txTime)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *LiteScorer) conflictExists(ctx context.Context, a, b uuid.UUID) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scored_conflicts
		 WHERE (decision_a_id = ? AND decision_b_id = ?)
		    OR (decision_a_id = ? AND decision_b_id = ?)`,
		a.String(), b.String(), b.String(), a.String(),
	).Scan(&count)
	return count > 0, err
}

func (s *LiteScorer) insertConflict(ctx context.Context, orgID uuid.UUID, conflictKind string, a, b liteDecision, topicSim, outcomeDivergence, significance float32, severity, explanation string) error {
	conflictID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Find or create conflict group.
	groupID, err := s.findOrCreateGroup(ctx, orgID, a, b, conflictKind)
	if err != nil {
		return fmt.Errorf("find/create group: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO scored_conflicts (
			id, conflict_kind, decision_a_id, decision_b_id, org_id,
			agent_a, agent_b, decision_type_a, decision_type_b,
			outcome_a, outcome_b,
			topic_similarity, outcome_divergence, significance, scoring_method,
			explanation, detected_at, severity, status, group_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conflictID.String(), conflictKind,
		a.id.String(), b.id.String(), orgID.String(),
		a.agentID, b.agentID,
		a.decisionType, b.decisionType,
		compact.Truncate(a.outcome, 500), compact.Truncate(b.outcome, 500),
		topicSim, outcomeDivergence, significance,
		"text_claims", explanation, now, severity, "open",
		groupID.String(),
	)
	return err
}

func (s *LiteScorer) findOrCreateGroup(ctx context.Context, orgID uuid.UUID, a, b liteDecision, conflictKind string) (uuid.UUID, error) {
	// Normalize agent ordering for deterministic group lookup.
	agentA, agentB := a.agentID, b.agentID
	if agentA > agentB {
		agentA, agentB = agentB, agentA
	}

	var groupIDStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM conflict_groups
		 WHERE org_id = ? AND agent_a = ? AND agent_b = ? AND conflict_kind = ? AND decision_type = ?`,
		orgID.String(), agentA, agentB, conflictKind, a.decisionType,
	).Scan(&groupIDStr)

	if err == nil {
		// Update last_detected_at.
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, _ = s.db.ExecContext(ctx,
			`UPDATE conflict_groups SET last_detected_at = ? WHERE id = ?`,
			now, groupIDStr,
		)
		id, _ := uuid.Parse(groupIDStr)
		return id, nil
	}

	if err != sql.ErrNoRows {
		return uuid.Nil, err
	}

	// Create new group. Use the later decision's transaction_time as
	// first_detected_at — a conflict can't exist before both decisions exist.
	groupID := uuid.New()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	firstDetected := now
	if !a.transactionTime.IsZero() && !b.transactionTime.IsZero() {
		earliest := a.transactionTime
		if b.transactionTime.After(earliest) {
			earliest = b.transactionTime
		}
		firstDetected = earliest.Format(time.RFC3339Nano)
	}
	topicLabel := storage.TruncateOutcome(a.outcome, 120)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO conflict_groups (id, org_id, agent_a, agent_b, conflict_kind, decision_type, group_topic, first_detected_at, last_detected_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		groupID.String(), orgID.String(), agentA, agentB, conflictKind, a.decisionType, topicLabel, firstDetected, now,
	)
	if err != nil {
		return uuid.Nil, err
	}
	return groupID, nil
}

// scoreClaimOverlap compares two sets of claims to estimate topic overlap and
// outcome divergence. Returns (topicSimilarity, outcomeDivergence, explanation).
//
// Topic similarity measures how many claims address the same subject.
// Outcome divergence measures how much the claims disagree once on the same topic.
func scoreClaimOverlap(claimsA, claimsB []string, outcomeA, outcomeB string) (topicSim, outcomeDivergence float32, explanation string) {
	if len(claimsA) == 0 || len(claimsB) == 0 {
		return 0, 0, ""
	}

	// Measure word-level overlap between outcomes for topic similarity.
	wordsA := uniqueWords(outcomeA)
	wordsB := uniqueWords(outcomeB)
	topicSim = jaccardSimilarity(wordsA, wordsB)

	if topicSim < 0.15 {
		return topicSim, 0, "topics too dissimilar"
	}

	// Compare claim pairs for contradictions. Two claims contradict when they
	// share significant word overlap (same topic) but differ overall.
	var matchingPairs, contradictions int
	for _, ca := range claimsA {
		caWords := uniqueWords(ca)
		for _, cb := range claimsB {
			cbWords := uniqueWords(cb)
			pairOverlap := jaccardSimilarity(caWords, cbWords)
			if pairOverlap < 0.25 {
				continue // different topics
			}
			matchingPairs++
			// Same topic — check if the claims meaningfully differ.
			if pairOverlap < 0.7 {
				contradictions++
			}
		}
	}

	if matchingPairs == 0 {
		return topicSim, 0, "no matching claim pairs"
	}

	outcomeDivergence = float32(contradictions) / float32(matchingPairs)

	explanation = fmt.Sprintf("%d/%d claim pairs diverge (topic overlap %.2f)", contradictions, matchingPairs, topicSim)
	return topicSim, outcomeDivergence, explanation
}

// uniqueWords extracts lowercased unique words of 3+ characters from text.
func uniqueWords(text string) map[string]bool {
	words := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		// Strip common punctuation.
		w = strings.Trim(w, ".,;:!?\"'()[]{}/-")
		if len(w) >= 3 {
			words[w] = true
		}
	}
	return words
}

// jaccardSimilarity computes |A ∩ B| / |A ∪ B|.
func jaccardSimilarity(a, b map[string]bool) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for w := range a {
		if b[w] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float32(intersection) / float32(union)
}
