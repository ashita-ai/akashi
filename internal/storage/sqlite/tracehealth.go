package sqlite

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/storage"
)

// GetDecisionQualityStats returns aggregate quality metrics for an org's decisions.
func (l *LiteDB) GetDecisionQualityStats(ctx context.Context, orgID uuid.UUID) (storage.DecisionQualityStats, error) {
	var qs storage.DecisionQualityStats
	err := l.db.QueryRowContext(ctx,
		`SELECT
		     COUNT(*),
		     COALESCE(AVG(completeness_score), 0),
		     COALESCE(SUM(CASE WHEN completeness_score < 0.5 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN completeness_score < 0.33 THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN reasoning IS NOT NULL AND reasoning != '' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN EXISTS (SELECT 1 FROM alternatives a WHERE a.decision_id = decisions.id) THEN 1 ELSE 0 END), 0)
		 FROM decisions WHERE org_id = ? AND valid_to IS NULL`,
		uuidStr(orgID),
	).Scan(&qs.Total, &qs.AvgCompleteness, &qs.BelowHalf, &qs.BelowThird, &qs.WithReasoning, &qs.WithAlternatives)
	if err != nil {
		return storage.DecisionQualityStats{}, fmt.Errorf("sqlite: quality stats: %w", err)
	}
	return qs, nil
}

// GetEvidenceCoverageStats returns evidence coverage metrics for an org.
func (l *LiteDB) GetEvidenceCoverageStats(ctx context.Context, orgID uuid.UUID) (storage.EvidenceCoverageStats, error) {
	var (
		totalDecisions int
		withEvidence   int
		totalRecords   int
	)
	err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT d.id), COUNT(DISTINCT e.decision_id), COUNT(e.id)
		 FROM decisions d
		 LEFT JOIN evidence e ON d.id = e.decision_id AND e.org_id = d.org_id
		 WHERE d.org_id = ? AND d.valid_to IS NULL`,
		uuidStr(orgID),
	).Scan(&totalDecisions, &withEvidence, &totalRecords)
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
func (l *LiteDB) GetConflictStatusCounts(ctx context.Context, orgID uuid.UUID) (storage.ConflictStatusCounts, error) {
	var cc storage.ConflictStatusCounts
	err := l.db.QueryRowContext(ctx,
		`SELECT
		     COUNT(*),
		     COALESCE(SUM(CASE WHEN status = 'open' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN status = 'acknowledged' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN status = 'resolved' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN status = 'wont_fix' THEN 1 ELSE 0 END), 0)
		 FROM scored_conflicts WHERE org_id = ?`,
		uuidStr(orgID),
	).Scan(&cc.Total, &cc.Open, &cc.Acknowledged, &cc.Resolved, &cc.WontFix)
	if err != nil {
		return storage.ConflictStatusCounts{}, fmt.Errorf("sqlite: conflict status counts: %w", err)
	}
	return cc, nil
}

// GetWontFixRate computes the wont_fix false-positive rate for an org over the
// last 30 days.
func (l *LiteDB) GetWontFixRate(ctx context.Context, orgID uuid.UUID) (storage.WontFixRate, error) {
	var r storage.WontFixRate
	err := l.db.QueryRowContext(ctx,
		`SELECT
		     COALESCE(SUM(CASE WHEN status = 'resolved' THEN 1 ELSE 0 END), 0),
		     COALESCE(SUM(CASE WHEN status = 'wont_fix' THEN 1 ELSE 0 END), 0)
		 FROM scored_conflicts
		 WHERE org_id = ?
		   AND status IN ('resolved', 'wont_fix')
		   AND resolved_at >= datetime('now', '-30 days')`,
		uuidStr(orgID),
	).Scan(&r.Resolved, &r.WontFix)
	if err != nil {
		return r, fmt.Errorf("sqlite: wont_fix rate: %w", err)
	}
	denom := r.Resolved + r.WontFix
	if denom > 0 {
		r.Rate = float64(r.WontFix) / float64(denom)
	}
	return r, nil
}

// GetOutcomeSignalsSummary returns aggregate outcome signal metrics for an org.
func (l *LiteDB) GetOutcomeSignalsSummary(ctx context.Context, orgID uuid.UUID) (storage.OutcomeSignalsSummary, error) {
	var os storage.OutcomeSignalsSummary
	err := l.db.QueryRowContext(ctx,
		`SELECT
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
		 FROM decisions d WHERE d.org_id = ? AND d.valid_to IS NULL`,
		uuidStr(orgID),
	).Scan(
		&os.DecisionsTotal, &os.NeverSuperseded, &os.RevisedWithin48h,
		&os.NeverCited, &os.CitedAtLeastOnce,
		&os.ConflictsWon, &os.ConflictsLost, &os.ConflictsNoWinner,
	)
	if err != nil {
		return storage.OutcomeSignalsSummary{}, fmt.Errorf("sqlite: outcome signals summary: %w", err)
	}
	return os, nil
}

// GetConfidenceDistribution returns confidence histogram buckets and per-agent
// confidence statistics for all current decisions in an org.
func (l *LiteDB) GetConfidenceDistribution(ctx context.Context, orgID uuid.UUID) (storage.ConfidenceDistribution, error) {
	var d storage.ConfidenceDistribution

	// SQLite lacks percentile_cont and FILTER, so we use CASE/SUM.
	var b0, b1, b2, b3, b4, b5, b6, b7, b8, b9 int
	var highCount int
	err := l.db.QueryRowContext(ctx,
		`SELECT
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
		     COALESCE(SUM(CASE WHEN confidence >= 0.9 THEN 1 ELSE 0 END), 0)
		 FROM decisions
		 WHERE org_id = ? AND valid_to IS NULL`,
		uuidStr(orgID),
	).Scan(
		&d.TotalDecisions, &d.AvgConfidence,
		&b0, &b1, &b2, &b3, &b4, &b5, &b6, &b7, &b8, &b9,
		&highCount,
	)
	if err != nil {
		return d, fmt.Errorf("sqlite: confidence distribution: %w", err)
	}

	if d.TotalDecisions > 0 {
		d.HighConfidencePct = float64(highCount) * 100.0 / float64(d.TotalDecisions)
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
	var median float64
	err = l.db.QueryRowContext(ctx,
		`SELECT COALESCE(confidence, 0) FROM decisions
		 WHERE org_id = ? AND valid_to IS NULL
		 ORDER BY confidence
		 LIMIT 1 OFFSET (SELECT COUNT(*)/2 FROM decisions WHERE org_id = ? AND valid_to IS NULL)`,
		uuidStr(orgID), uuidStr(orgID),
	).Scan(&median)
	if err == nil {
		d.MedianConfidence = median
	}

	// Per-agent breakdown.
	rows, err := l.db.QueryContext(ctx,
		`SELECT agent_id, AVG(confidence), MIN(confidence), MAX(confidence), COUNT(*)
		 FROM decisions
		 WHERE org_id = ? AND valid_to IS NULL
		 GROUP BY agent_id
		 ORDER BY AVG(confidence) DESC`,
		uuidStr(orgID),
	)
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
