package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/storage"
)

// GetDecisionQualityStats returns aggregate quality metrics for an org's decisions.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (l *LiteDB) GetDecisionQualityStats(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (storage.DecisionQualityStats, error) {
	var qs storage.DecisionQualityStats
	q := `SELECT
		     COUNT(*),
		     COALESCE(AVG(completeness_score), 0),
		     COALESCE(SUM(CASE WHEN completeness_score < 0.5 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN completeness_score < 0.33 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN reasoning IS NOT NULL AND reasoning != '' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN EXISTS (SELECT 1 FROM alternatives a WHERE a.decision_id = decisions.id) THEN 1 ELSE 0 END), 0)
		 FROM decisions WHERE org_id = ? AND valid_to IS NULL`
	args := []any{uuidStr(orgID)}
	if from != nil {
		q += " AND valid_from >= ?"
		args = append(args, from.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	if to != nil {
		q += " AND valid_from < ?"
		args = append(args, to.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	err := l.db.QueryRowContext(ctx, q, args...).Scan(
		&qs.Total, &qs.AvgCompleteness, &qs.BelowHalf, &qs.BelowThird, &qs.WithReasoning, &qs.WithAlternatives)
	if err != nil {
		return storage.DecisionQualityStats{}, fmt.Errorf("sqlite: quality stats: %w", err)
	}
	return qs, nil
}

// GetEvidenceCoverageStats returns evidence coverage metrics for an org.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (l *LiteDB) GetEvidenceCoverageStats(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (storage.EvidenceCoverageStats, error) {
	var (
		totalDecisions int
		withEvidence   int
		totalRecords   int
	)
	q := `SELECT COUNT(DISTINCT d.id), COUNT(DISTINCT e.decision_id), COUNT(e.id)
		 FROM decisions d
		 LEFT JOIN evidence e ON d.id = e.decision_id AND e.org_id = d.org_id
		 WHERE d.org_id = ? AND d.valid_to IS NULL`
	args := []any{uuidStr(orgID)}
	if from != nil {
		q += " AND d.valid_from >= ?"
		args = append(args, from.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	if to != nil {
		q += " AND d.valid_from < ?"
		args = append(args, to.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	err := l.db.QueryRowContext(ctx, q, args...).Scan(&totalDecisions, &withEvidence, &totalRecords)
	if err != nil {
		return storage.EvidenceCoverageStats{}, fmt.Errorf("sqlite: evidence coverage: %w", err)
	}

	without := totalDecisions - withEvidence
	var coveragePct float64
	var avgPerDecision float64
	if totalDecisions > 0 {
		coveragePct = float64(withEvidence) / float64(totalDecisions) * 100
		avgPerDecision = float64(totalRecords) / float64(totalDecisions)
	}

	return storage.EvidenceCoverageStats{
		TotalDecisions:       totalDecisions,
		WithEvidence:         withEvidence,
		WithoutEvidenceCount: without,
		CoveragePercent:      coveragePct,
		TotalRecords:         totalRecords,
		AvgPerDecision:       avgPerDecision,
	}, nil
}

// GetConflictStatusCounts returns conflict status breakdown for an org.
// When from/to are non-nil, only conflicts with detected_at in [from, to) are included.
func (l *LiteDB) GetConflictStatusCounts(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (storage.ConflictStatusCounts, error) {
	var cc storage.ConflictStatusCounts
	q := `SELECT
		     COUNT(*),
		     COALESCE(SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN status = 'resolved' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN status = 'false_positive' THEN 1 ELSE 0 END), 0)
		 FROM scored_conflicts WHERE org_id = ?`
	args := []any{uuidStr(orgID)}
	if from != nil {
		q += " AND detected_at >= ?"
		args = append(args, from.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	if to != nil {
		q += " AND detected_at < ?"
		args = append(args, to.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	err := l.db.QueryRowContext(ctx, q, args...).Scan(
		&cc.Total, &cc.Open, &cc.Resolved, &cc.FalsePositive)
	if err != nil {
		return storage.ConflictStatusCounts{}, fmt.Errorf("sqlite: conflict status counts: %w", err)
	}
	return cc, nil
}

// GetFalsePositiveRate computes the false-positive rate for an org over the
// last 30 days. Rate = FalsePositive / (Resolved + FalsePositive).
func (l *LiteDB) GetFalsePositiveRate(ctx context.Context, orgID uuid.UUID) (storage.FalsePositiveRate, error) {
	var r storage.FalsePositiveRate
	err := l.db.QueryRowContext(ctx,
		`SELECT
		     COALESCE(SUM(CASE WHEN status = 'resolved' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN status = 'false_positive' THEN 1 ELSE 0 END), 0)
		 FROM scored_conflicts
		 WHERE org_id = ?
		   AND status IN ('resolved', 'false_positive')
		   AND resolved_at >= datetime('now', '-30 days')`,
		uuidStr(orgID),
	).Scan(&r.Resolved, &r.FalsePositive)
	if err != nil {
		return r, fmt.Errorf("sqlite: false positive rate: %w", err)
	}
	denom := r.Resolved + r.FalsePositive
	if denom > 0 {
		r.Rate = float64(r.FalsePositive) / float64(denom)
	}
	return r, nil
}

// GetOutcomeSignalsSummary returns aggregate outcome signal metrics for an org.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (l *LiteDB) GetOutcomeSignalsSummary(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (storage.OutcomeSignalsSummary, error) {
	var os storage.OutcomeSignalsSummary
	q := `SELECT
		     COUNT(*),
		     COALESCE(SUM(CASE WHEN NOT EXISTS (
		         SELECT 1 FROM decisions sup WHERE sup.supersedes_id = d.id AND sup.org_id = d.org_id
		     ) THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN EXISTS (
		         SELECT 1 FROM decisions sup
		         WHERE sup.supersedes_id = d.id AND sup.org_id = d.org_id
		           AND (julianday(sup.valid_from) - julianday(d.valid_from)) * 24.0 < 48
		     ) THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN NOT EXISTS (
		         SELECT 1 FROM decisions cite
		         WHERE cite.precedent_ref = d.id AND cite.org_id = d.org_id AND cite.valid_to IS NULL
		     ) THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN EXISTS (
		         SELECT 1 FROM decisions cite
		         WHERE cite.precedent_ref = d.id AND cite.org_id = d.org_id AND cite.valid_to IS NULL
		     ) THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN EXISTS (
		         SELECT 1 FROM scored_conflicts sc
		         WHERE (sc.decision_a_id = d.id OR sc.decision_b_id = d.id)
		           AND sc.winning_decision_id = d.id
		     ) THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN EXISTS (
		         SELECT 1 FROM scored_conflicts sc
		         WHERE (sc.decision_a_id = d.id OR sc.decision_b_id = d.id)
		           AND sc.winning_decision_id IS NOT NULL AND sc.winning_decision_id != d.id
		     ) THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN EXISTS (
		         SELECT 1 FROM scored_conflicts sc
		         WHERE (sc.decision_a_id = d.id OR sc.decision_b_id = d.id)
		           AND sc.status = 'resolved' AND sc.winning_decision_id IS NULL
		     ) THEN 1 ELSE 0 END), 0)
		 FROM decisions d WHERE d.org_id = ? AND d.valid_to IS NULL`
	args := []any{uuidStr(orgID)}
	if from != nil {
		q += " AND d.valid_from >= ?"
		args = append(args, from.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	if to != nil {
		q += " AND d.valid_from < ?"
		args = append(args, to.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	err := l.db.QueryRowContext(ctx, q, args...).Scan(
		&os.DecisionsTotal, &os.NeverSuperseded, &os.RevisedWithin48h,
		&os.NeverCited, &os.CitedAtLeastOnce,
		&os.ConflictsWon, &os.ConflictsLost, &os.ConflictsNoWinner,
	)
	if err != nil {
		return storage.OutcomeSignalsSummary{}, fmt.Errorf("sqlite: outcome signals summary: %w", err)
	}
	return os, nil
}

// GetConfidenceCalibration returns per-tier and per-agent calibration signals
// correlating declared confidence with revision rates and assessment outcomes.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (l *LiteDB) GetConfidenceCalibration(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (storage.ConfidenceCalibration, error) {
	var cal storage.ConfidenceCalibration

	// Build optional time-range clause.
	timeFilter := ""
	args := []any{uuidStr(orgID)}
	if from != nil {
		args = append(args, from.Format(time.RFC3339Nano))
		timeFilter += " AND d.valid_from >= ?"
	}
	if to != nil {
		args = append(args, to.Format(time.RFC3339Nano))
		timeFilter += " AND d.valid_from < ?"
	}

	// Per-tier calibration: group into low/mid/high and compute revision rate + outcome.
	tierQuery := `SELECT` + //nolint:gosec // G202: timeFilter contains only parameterized time-range clauses
		`
		    tier,
		    COUNT(*)                                                            AS total,
		    COALESCE(SUM(CASE WHEN revised = 1 THEN 1 ELSE 0 END), 0)         AS revised_count,
		    COALESCE(SUM(CASE WHEN outcome_score IS NOT NULL THEN 1 ELSE 0 END), 0) AS assessed_count,
		    AVG(CASE WHEN outcome_score IS NOT NULL THEN outcome_score END)
		FROM (
		    SELECT
		        d.id,
		        d.outcome_score,
		        CASE
		            WHEN d.confidence >= 0.85 THEN 'high'
		            WHEN d.confidence >= 0.5  THEN 'mid'
		            ELSE 'low'
		        END AS tier,
		        CASE WHEN EXISTS (
		            SELECT 1 FROM decisions sup
		            WHERE sup.supersedes_id = d.id
		              AND sup.org_id = d.org_id
		              AND (julianday(sup.valid_from) - julianday(d.valid_from)) * 24.0 < 48
		        ) THEN 1 ELSE 0 END AS revised
		    FROM decisions d
		    WHERE d.org_id = ? AND d.valid_to IS NULL` + timeFilter + //nolint:gosec // G202: timeFilter contains only parameterized time-range clauses
		`) sub
		GROUP BY tier
		ORDER BY tier`
	rows, err := l.db.QueryContext(ctx, tierQuery, args...)
	if err != nil {
		return cal, fmt.Errorf("sqlite: confidence calibration tiers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tierMap := make(map[string]*storage.ConfidenceTier, 3)
	for rows.Next() {
		var t storage.ConfidenceTier
		var revisedCount int
		var avgOutcome *float64
		if err := rows.Scan(&t.Tier, &t.Total, &revisedCount, &t.AssessedCount, &avgOutcome); err != nil {
			return cal, fmt.Errorf("sqlite: scan calibration tier: %w", err)
		}
		if t.Total > 0 {
			t.RevisionRate = float64(revisedCount) / float64(t.Total) * 100
		}
		t.AvgOutcome = avgOutcome
		tierMap[t.Tier] = &t
		cal.Tiers = append(cal.Tiers, t)
	}
	if err := rows.Err(); err != nil {
		return cal, fmt.Errorf("sqlite: iterate calibration tiers: %w", err)
	}

	// Determine calibration state.
	cal.HasOutcomeData = false
	for _, t := range cal.Tiers {
		if t.AssessedCount > 0 {
			cal.HasOutcomeData = true
			break
		}
	}
	cal.Calibrated = storage.ComputeCalibrated(tierMap, cal.HasOutcomeData)

	// Per-agent calibration.
	agentQuery := `SELECT` + //nolint:gosec // G202: timeFilter contains only parameterized time-range clauses
		`
		    d.agent_id,
		    COUNT(*),
		    AVG(d.confidence),
		    COALESCE(SUM(CASE WHEN EXISTS (
		        SELECT 1 FROM decisions sup
		        WHERE sup.supersedes_id = d.id
		          AND sup.org_id = d.org_id
		          AND (julianday(sup.valid_from) - julianday(d.valid_from)) * 24.0 < 48
		    ) THEN 1 ELSE 0 END), 0),
		    COALESCE(SUM(CASE WHEN d.outcome_score IS NOT NULL THEN 1 ELSE 0 END), 0),
		    AVG(CASE WHEN d.outcome_score IS NOT NULL THEN d.outcome_score END)
		FROM decisions d
		WHERE d.org_id = ? AND d.valid_to IS NULL` + timeFilter + //nolint:gosec // G202: timeFilter contains only parameterized time-range clauses
		` GROUP BY d.agent_id
		ORDER BY AVG(d.confidence) DESC`
	agentRows, err := l.db.QueryContext(ctx, agentQuery, args...)
	if err != nil {
		return cal, fmt.Errorf("sqlite: confidence calibration by agent: %w", err)
	}
	defer func() { _ = agentRows.Close() }()

	for agentRows.Next() {
		var a storage.AgentCalibration
		var revisedCount int
		var avgOutcome *float64
		if err := agentRows.Scan(&a.AgentID, &a.Total, &a.AvgConfidence, &revisedCount, &a.AssessedCount, &avgOutcome); err != nil {
			return cal, fmt.Errorf("sqlite: scan agent calibration: %w", err)
		}
		if a.Total > 0 {
			a.RevisionRate = float64(revisedCount) / float64(a.Total) * 100
		}
		a.AvgOutcome = avgOutcome
		cal.ByAgent = append(cal.ByAgent, a)
	}
	if err := agentRows.Err(); err != nil {
		return cal, fmt.Errorf("sqlite: iterate agent calibration: %w", err)
	}

	return cal, nil
}

// GetConfidenceDistribution returns confidence histogram buckets and per-agent
// confidence statistics for current decisions in an org.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (l *LiteDB) GetConfidenceDistribution(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (storage.ConfidenceDistribution, error) {
	var d storage.ConfidenceDistribution

	// SQLite lacks percentile_cont and FILTER, so we use CASE/SUM.
	var b0, b1, b2, b3, b4, b5, b6, b7, b8, b9 int
	var highCount, overconfidentCount int
	q := `SELECT
		     COUNT(*),
		     COALESCE(AVG(confidence), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.0 AND confidence < 0.1 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.1 AND confidence < 0.2 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.2 AND confidence < 0.3 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.3 AND confidence < 0.4 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.4 AND confidence < 0.5 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.5 AND confidence < 0.6 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.6 AND confidence < 0.7 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.7 AND confidence < 0.8 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.8 AND confidence < 0.9 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.9 AND confidence <= 1.0 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.9 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN confidence >= 0.85 THEN 1 ELSE 0 END), 0)
		 FROM decisions
		 WHERE org_id = ? AND valid_to IS NULL`
	args := []any{uuidStr(orgID)}
	if from != nil {
		q += " AND valid_from >= ?"
		args = append(args, from.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	if to != nil {
		q += " AND valid_from < ?"
		args = append(args, to.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	err := l.db.QueryRowContext(ctx, q, args...).Scan(
		&d.TotalDecisions, &d.AvgConfidence,
		&b0, &b1, &b2, &b3, &b4, &b5, &b6, &b7, &b8, &b9,
		&highCount, &overconfidentCount,
	)
	if err != nil {
		return d, fmt.Errorf("sqlite: confidence distribution: %w", err)
	}

	if d.TotalDecisions > 0 {
		d.HighConfidencePct = float64(highCount) * 100.0 / float64(d.TotalDecisions)
		d.OverconfidentPct = float64(overconfidentCount) * 100.0 / float64(d.TotalDecisions)
	}

	labels := [10]string{
		"0.0-0.1", "0.1-0.2", "0.2-0.3", "0.3-0.4", "0.4-0.5",
		"0.5-0.6", "0.6-0.7", "0.7-0.8", "0.8-0.9", "0.9-1.0",
	}
	counts := [10]int{b0, b1, b2, b3, b4, b5, b6, b7, b8, b9}
	d.Buckets = make([]storage.ConfidenceBucket, len(labels))
	for i := range labels {
		d.Buckets[i] = storage.ConfidenceBucket{Bucket: labels[i], Count: counts[i]}
	}

	// Approximate median: sort all confidence values. This is fine for moderate
	// decision counts; SQLite has no built-in percentile function.
	// Apply the same time-range filter to the median subquery.
	medianQ := `SELECT COALESCE(confidence, 0) FROM decisions
		 WHERE org_id = ? AND valid_to IS NULL`
	countQ := `SELECT COUNT(*)/2 FROM decisions WHERE org_id = ? AND valid_to IS NULL`
	medianArgs := []any{uuidStr(orgID)}
	countArgs := []any{uuidStr(orgID)}
	if from != nil {
		fmtFrom := from.UTC().Format("2006-01-02T15:04:05.999999999Z")
		medianQ += " AND valid_from >= ?"
		countQ += " AND valid_from >= ?"
		medianArgs = append(medianArgs, fmtFrom)
		countArgs = append(countArgs, fmtFrom)
	}
	if to != nil {
		fmtTo := to.UTC().Format("2006-01-02T15:04:05.999999999Z")
		medianQ += " AND valid_from < ?"
		countQ += " AND valid_from < ?"
		medianArgs = append(medianArgs, fmtTo)
		countArgs = append(countArgs, fmtTo)
	}
	medianQ += " ORDER BY confidence LIMIT 1 OFFSET (" + countQ + ")"
	medianArgs = append(medianArgs, countArgs...)
	var median float64
	err = l.db.QueryRowContext(ctx, medianQ, medianArgs...).Scan(&median)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, fmt.Errorf("sqlite: confidence median: %w", err)
	}
	if err == nil {
		d.MedianConfidence = median
	}

	// Per-agent breakdown. Same time-range filter applies.
	agentQ := `SELECT agent_id, AVG(confidence), MIN(confidence), MAX(confidence), COUNT(*)
		 FROM decisions
		 WHERE org_id = ? AND valid_to IS NULL`
	agentArgs := []any{uuidStr(orgID)}
	if from != nil {
		agentQ += " AND valid_from >= ?"
		agentArgs = append(agentArgs, from.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	if to != nil {
		agentQ += " AND valid_from < ?"
		agentArgs = append(agentArgs, to.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	agentQ += " GROUP BY agent_id ORDER BY AVG(confidence) DESC"
	rows, err := l.db.QueryContext(ctx, agentQ, agentArgs...)
	if err != nil {
		return d, fmt.Errorf("sqlite: confidence by agent: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var a storage.AgentConfidenceStats
		if err := rows.Scan(&a.AgentID, &a.AvgConfidence, &a.MinConfidence, &a.MaxConfidence, &a.DecisionCount); err != nil {
			return d, fmt.Errorf("sqlite: scan agent confidence: %w", err)
		}
		d.ByAgent = append(d.ByAgent, a)
	}
	if err := rows.Err(); err != nil {
		return d, fmt.Errorf("sqlite: iterate agent confidence: %w", err)
	}

	return d, nil
}

// GetHighConfOutcomeSignals returns behavioral outcome signals scoped to
// decisions with confidence >= 0.85. When from/to are non-nil, only decisions
// with valid_from in [from, to) are included.
func (l *LiteDB) GetHighConfOutcomeSignals(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (storage.HighConfOutcomeSignals, error) {
	var s storage.HighConfOutcomeSignals
	q := `SELECT
	         COUNT(*),
	         COALESCE(SUM(CASE WHEN EXISTS (
	             SELECT 1 FROM decisions sup
	             WHERE sup.supersedes_id = d.id AND sup.org_id = d.org_id
	               AND (julianday(sup.valid_from) - julianday(d.valid_from)) * 24.0 < 48
	         ) THEN 1 ELSE 0 END), 0),
	         COALESCE(SUM(CASE WHEN EXISTS (
	             SELECT 1 FROM scored_conflicts sc
	             WHERE sc.org_id = d.org_id
	               AND (sc.decision_a_id = d.id OR sc.decision_b_id = d.id)
	               AND sc.status IN ('resolved', 'false_positive')
	               AND sc.winning_decision_id IS NOT NULL
	               AND sc.winning_decision_id != d.id
	         ) THEN 1 ELSE 0 END), 0),
	         COALESCE(SUM(CASE WHEN d.outcome_score IS NOT NULL THEN 1 ELSE 0 END), 0),
	         COALESCE(AVG(CASE WHEN d.outcome_score IS NOT NULL THEN d.outcome_score END), 0)
	     FROM decisions d
	     WHERE d.org_id = ? AND d.valid_to IS NULL AND d.confidence >= 0.85`
	args := []any{uuidStr(orgID)}
	if from != nil {
		q += " AND d.valid_from >= ?"
		args = append(args, from.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	if to != nil {
		q += " AND d.valid_from < ?"
		args = append(args, to.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	err := l.db.QueryRowContext(ctx, q, args...).Scan(
		&s.Total, &s.RevisedWithin48h, &s.ConflictsLost, &s.AssessedCount, &s.AvgOutcomeScore)
	if err != nil {
		return storage.HighConfOutcomeSignals{}, fmt.Errorf("sqlite: high-conf outcome signals: %w", err)
	}
	return s, nil
}

// GetCompletenessByDecisionType returns per-type average completeness for current
// decisions, ordered by average completeness ascending.
func (l *LiteDB) GetCompletenessByDecisionType(ctx context.Context, orgID uuid.UUID, from, to *time.Time) ([]storage.DecisionTypeCompleteness, error) {
	q := `SELECT decision_type, COUNT(*), COALESCE(AVG(completeness_score), 0)
		 FROM decisions
		 WHERE org_id = ? AND valid_to IS NULL`
	args := []any{uuidStr(orgID)}
	if from != nil {
		q += " AND valid_from >= ?"
		args = append(args, from.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	if to != nil {
		q += " AND valid_from < ?"
		args = append(args, to.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	q += " GROUP BY decision_type ORDER BY COALESCE(AVG(completeness_score), 0) ASC"

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: completeness by decision type: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []storage.DecisionTypeCompleteness
	for rows.Next() {
		var dtc storage.DecisionTypeCompleteness
		if err := rows.Scan(&dtc.DecisionType, &dtc.Count, &dtc.AvgCompleteness); err != nil {
			return nil, fmt.Errorf("sqlite: scan completeness by type: %w", err)
		}
		result = append(result, dtc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate completeness by type: %w", err)
	}
	return result, nil
}

// GetDecisionTypeDistribution returns the count of current decisions grouped by
// decision_type, ordered by count descending.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (l *LiteDB) GetDecisionTypeDistribution(ctx context.Context, orgID uuid.UUID, from, to *time.Time) ([]storage.DecisionTypeCount, error) {
	q := `SELECT decision_type, COUNT(*)
		 FROM decisions
		 WHERE org_id = ? AND valid_to IS NULL`
	args := []any{uuidStr(orgID)}
	if from != nil {
		args = append(args, from.Format(time.RFC3339Nano))
		q += " AND valid_from >= ?"
	}
	if to != nil {
		args = append(args, to.Format(time.RFC3339Nano))
		q += " AND valid_from < ?"
	}
	q += ` GROUP BY decision_type ORDER BY COUNT(*) DESC`

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: decision type distribution: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []storage.DecisionTypeCount
	for rows.Next() {
		var dt storage.DecisionTypeCount
		if err := rows.Scan(&dt.DecisionType, &dt.Count); err != nil {
			return nil, fmt.Errorf("sqlite: scan decision type count: %w", err)
		}
		result = append(result, dt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate decision type distribution: %w", err)
	}
	return result, nil
}
