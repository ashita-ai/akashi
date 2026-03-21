//go:build !lite

package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// GetOutcomeSignalsSummary returns org-level aggregate outcome signal counts
// for use in GET /v1/trace-health.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (db *DB) GetOutcomeSignalsSummary(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (OutcomeSignalsSummary, error) {
	var s OutcomeSignalsSummary

	// Build optional time-range clause for the anchor decision (d).
	timeFilter := ""
	args := []any{orgID}
	if from != nil {
		args = append(args, *from)
		timeFilter += fmt.Sprintf(" AND d.valid_from >= $%d", len(args))
	}
	if to != nil {
		args = append(args, *to)
		timeFilter += fmt.Sprintf(" AND d.valid_from < $%d", len(args))
	}

	// Decision-level counts: total, never_superseded, revised_within_48h, citation coverage.
	// Uses EXISTS subqueries to avoid joins that would multiply rows.
	err := db.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*)::int,
		    COUNT(*) FILTER (WHERE NOT EXISTS (
		        SELECT 1 FROM decisions sup
		        WHERE sup.supersedes_id = d.id AND sup.org_id = d.org_id
		    ))::int,
		    COUNT(*) FILTER (WHERE EXISTS (
		        SELECT 1 FROM decisions sup
		        WHERE sup.supersedes_id = d.id
		          AND sup.org_id = d.org_id
		          AND EXTRACT(EPOCH FROM (sup.valid_from - d.valid_from)) / 3600 < 48
		    ))::int,
		    COUNT(*) FILTER (WHERE NOT EXISTS (
		        SELECT 1 FROM decisions cit
		        WHERE cit.precedent_ref = d.id AND cit.org_id = d.org_id AND cit.valid_to IS NULL
		    ))::int,
		    COUNT(*) FILTER (WHERE EXISTS (
		        SELECT 1 FROM decisions cit
		        WHERE cit.precedent_ref = d.id AND cit.org_id = d.org_id AND cit.valid_to IS NULL
		    ))::int
		FROM decisions d
		WHERE d.org_id = $1 AND d.valid_to IS NULL`+timeFilter, args...).Scan(
		&s.DecisionsTotal,
		&s.NeverSuperseded,
		&s.RevisedWithin48h,
		&s.NeverCited,
		&s.CitedAtLeastOnce,
	)
	if err != nil {
		return s, fmt.Errorf("storage: outcome signals summary: %w", err)
	}

	// Conflict fate: org-level counts from resolved/wont_fix conflicts.
	// ConflictsWon = conflicts where a winner was declared (= ConflictsLost, symmetric).
	// ConflictsNoWinner = conflicts resolved without declaring a winner.
	err = db.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*) FILTER (WHERE winning_decision_id IS NOT NULL)::int,
		    COUNT(*) FILTER (WHERE winning_decision_id IS NULL AND status = 'resolved')::int
		FROM scored_conflicts
		WHERE org_id = $1 AND status IN ('resolved', 'wont_fix')`, orgID).Scan(
		&s.ConflictsWon, &s.ConflictsNoWinner,
	)
	if err != nil {
		return s, fmt.Errorf("storage: outcome signals conflict counts: %w", err)
	}
	s.ConflictsLost = s.ConflictsWon // every decided win has a corresponding loss

	return s, nil
}

// GetConfidenceDistribution returns confidence histogram buckets and per-agent
// confidence statistics for current decisions in an org.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (db *DB) GetConfidenceDistribution(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (ConfidenceDistribution, error) {
	var d ConfidenceDistribution

	// Build optional time-range clause.
	timeFilter := ""
	args := []any{orgID}
	if from != nil {
		args = append(args, *from)
		timeFilter += fmt.Sprintf(" AND valid_from >= $%d", len(args))
	}
	if to != nil {
		args = append(args, *to)
		timeFilter += fmt.Sprintf(" AND valid_from < $%d", len(args))
	}

	// Aggregate stats + histogram in a single query. The FILTER clause counts
	// decisions in each 0.1-wide bucket, and percentile_cont gives the median.
	err := db.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*)::int,
		    COALESCE(AVG(confidence), 0),
		    COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY confidence), 0),
		    COUNT(*) FILTER (WHERE confidence >= 0.0 AND confidence < 0.1)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.1 AND confidence < 0.2)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.2 AND confidence < 0.3)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.3 AND confidence < 0.4)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.4 AND confidence < 0.5)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.5 AND confidence < 0.6)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.6 AND confidence < 0.7)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.7 AND confidence < 0.8)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.8 AND confidence < 0.9)::int,
		    COUNT(*) FILTER (WHERE confidence >= 0.9 AND confidence <= 1.0)::int,
		    COALESCE(COUNT(*) FILTER (WHERE confidence >= 0.9) * 100.0 / NULLIF(COUNT(*), 0), 0),
		    COALESCE(COUNT(*) FILTER (WHERE confidence >= 0.85) * 100.0 / NULLIF(COUNT(*), 0), 0)
		FROM decisions
		WHERE org_id = $1 AND valid_to IS NULL`+timeFilter, args...).Scan(
		&d.TotalDecisions, &d.AvgConfidence, &d.MedianConfidence,
		&bucketCount{&d, 0}, &bucketCount{&d, 1}, &bucketCount{&d, 2},
		&bucketCount{&d, 3}, &bucketCount{&d, 4}, &bucketCount{&d, 5},
		&bucketCount{&d, 6}, &bucketCount{&d, 7}, &bucketCount{&d, 8},
		&bucketCount{&d, 9},
		&d.HighConfidencePct,
		&d.OverconfidentPct,
	)
	if err != nil {
		return d, fmt.Errorf("storage: confidence distribution: %w", err)
	}

	// Per-agent confidence breakdown, ordered by avg descending so the most
	// confident agents appear first. Same time-range filter applies.
	rows, err := db.pool.Query(ctx, `
		SELECT agent_id,
		       AVG(confidence),
		       MIN(confidence),
		       MAX(confidence),
		       COUNT(*)::int
		FROM decisions
		WHERE org_id = $1 AND valid_to IS NULL`+timeFilter+`
		GROUP BY agent_id
		ORDER BY AVG(confidence) DESC`, args...)
	if err != nil {
		return d, fmt.Errorf("storage: confidence by agent: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var a AgentConfidenceStats
		if err := rows.Scan(&a.AgentID, &a.AvgConfidence, &a.MinConfidence, &a.MaxConfidence, &a.DecisionCount); err != nil {
			return d, fmt.Errorf("storage: scan agent confidence: %w", err)
		}
		d.ByAgent = append(d.ByAgent, a)
	}
	if err := rows.Err(); err != nil {
		return d, fmt.Errorf("storage: iterate agent confidence: %w", err)
	}

	return d, nil
}

// GetHighConfOutcomeSignals returns behavioral outcome signals scoped to
// current decisions with confidence >= 0.85 for an org.
// When from/to are non-nil, only decisions with valid_from in [from, to) are included.
func (db *DB) GetHighConfOutcomeSignals(ctx context.Context, orgID uuid.UUID, from, to *time.Time) (HighConfOutcomeSignals, error) {
	var s HighConfOutcomeSignals

	timeFilter := ""
	args := []any{orgID}
	if from != nil {
		args = append(args, *from)
		timeFilter += fmt.Sprintf(" AND d.valid_from >= $%d", len(args))
	}
	if to != nil {
		args = append(args, *to)
		timeFilter += fmt.Sprintf(" AND d.valid_from < $%d", len(args))
	}

	err := db.pool.QueryRow(ctx, `
		SELECT
		    COUNT(*)::int,
		    COUNT(*) FILTER (WHERE EXISTS (
		        SELECT 1 FROM decisions sup
		        WHERE sup.supersedes_id = d.id
		          AND sup.org_id = d.org_id
		          AND EXTRACT(EPOCH FROM (sup.valid_from - d.valid_from)) / 3600 < 48
		    ))::int,
		    COUNT(*) FILTER (WHERE EXISTS (
		        SELECT 1 FROM scored_conflicts sc
		        WHERE sc.org_id = d.org_id
		          AND (sc.decision_a_id = d.id OR sc.decision_b_id = d.id)
		          AND sc.status IN ('resolved', 'wont_fix')
		          AND sc.winning_decision_id IS NOT NULL
		          AND sc.winning_decision_id != d.id
		    ))::int,
		    COUNT(*) FILTER (WHERE d.outcome_score IS NOT NULL)::int,
		    COALESCE(AVG(d.outcome_score) FILTER (WHERE d.outcome_score IS NOT NULL), 0)
		FROM decisions d
		WHERE d.org_id = $1 AND d.valid_to IS NULL AND d.confidence >= 0.85`+timeFilter,
		args...,
	).Scan(&s.Total, &s.RevisedWithin48h, &s.ConflictsLost, &s.AssessedCount, &s.AvgOutcomeScore)
	if err != nil {
		return s, fmt.Errorf("storage: high-conf outcome signals: %w", err)
	}
	return s, nil
}

// GetConfidenceCalibration returns per-tier and per-agent calibration signals.
// It correlates declared confidence with revision rates (temporal proxy) and
// assessment outcome scores (ground truth, when available).
func (db *DB) GetConfidenceCalibration(ctx context.Context, orgID uuid.UUID) (ConfidenceCalibration, error) {
	var cal ConfidenceCalibration

	// Per-tier: group decisions into low/mid/high confidence bands and compute
	// revision rate + avg outcome_score for each.
	rows, err := db.pool.Query(ctx, `
		SELECT
		    tier,
		    COUNT(*)::int                                                          AS total,
		    COUNT(*) FILTER (WHERE revised)::int                                   AS revised_count,
		    COUNT(*) FILTER (WHERE outcome_score IS NOT NULL)::int                 AS assessed_count,
		    AVG(outcome_score) FILTER (WHERE outcome_score IS NOT NULL)
		FROM (
		    SELECT
		        d.id,
		        d.outcome_score,
		        CASE
		            WHEN d.confidence >= 0.85 THEN 'high'
		            WHEN d.confidence >= 0.5  THEN 'mid'
		            ELSE 'low'
		        END AS tier,
		        EXISTS (
		            SELECT 1 FROM decisions sup
		            WHERE sup.supersedes_id = d.id
		              AND sup.org_id = d.org_id
		              AND EXTRACT(EPOCH FROM (sup.valid_from - d.valid_from)) / 3600 < 48
		        ) AS revised
		    FROM decisions d
		    WHERE d.org_id = $1 AND d.valid_to IS NULL
		) sub
		GROUP BY tier
		ORDER BY tier`, orgID)
	if err != nil {
		return cal, fmt.Errorf("storage: confidence calibration tiers: %w", err)
	}
	defer rows.Close()

	tierMap := make(map[string]*ConfidenceTier, 3)
	for rows.Next() {
		var t ConfidenceTier
		var revisedCount int
		var avgOutcome *float64
		if err := rows.Scan(&t.Tier, &t.Total, &revisedCount, &t.AssessedCount, &avgOutcome); err != nil {
			return cal, fmt.Errorf("storage: scan calibration tier: %w", err)
		}
		if t.Total > 0 {
			t.RevisionRate = float64(revisedCount) / float64(t.Total) * 100
		}
		t.AvgOutcome = avgOutcome
		tierMap[t.Tier] = &t
		cal.Tiers = append(cal.Tiers, t)
	}
	if err := rows.Err(); err != nil {
		return cal, fmt.Errorf("storage: iterate calibration tiers: %w", err)
	}

	// Determine calibration state from outcome data or revision rates.
	cal.HasOutcomeData = false
	for _, t := range cal.Tiers {
		if t.AssessedCount > 0 {
			cal.HasOutcomeData = true
			break
		}
	}

	cal.Calibrated = ComputeCalibrated(tierMap, cal.HasOutcomeData)

	// Per-agent calibration: revision rate + outcome by agent.
	agentRows, err := db.pool.Query(ctx, `
		SELECT
		    d.agent_id,
		    COUNT(*)::int,
		    AVG(d.confidence),
		    COUNT(*) FILTER (WHERE EXISTS (
		        SELECT 1 FROM decisions sup
		        WHERE sup.supersedes_id = d.id
		          AND sup.org_id = d.org_id
		          AND EXTRACT(EPOCH FROM (sup.valid_from - d.valid_from)) / 3600 < 48
		    ))::int,
		    COUNT(*) FILTER (WHERE d.outcome_score IS NOT NULL)::int,
		    AVG(d.outcome_score) FILTER (WHERE d.outcome_score IS NOT NULL)
		FROM decisions d
		WHERE d.org_id = $1 AND d.valid_to IS NULL
		GROUP BY d.agent_id
		ORDER BY AVG(d.confidence) DESC`, orgID)
	if err != nil {
		return cal, fmt.Errorf("storage: confidence calibration by agent: %w", err)
	}
	defer agentRows.Close()

	for agentRows.Next() {
		var a AgentCalibration
		var revisedCount int
		var avgOutcome *float64
		if err := agentRows.Scan(&a.AgentID, &a.Total, &a.AvgConfidence, &revisedCount, &a.AssessedCount, &avgOutcome); err != nil {
			return cal, fmt.Errorf("storage: scan agent calibration: %w", err)
		}
		if a.Total > 0 {
			a.RevisionRate = float64(revisedCount) / float64(a.Total) * 100
		}
		a.AvgOutcome = avgOutcome
		cal.ByAgent = append(cal.ByAgent, a)
	}
	if err := agentRows.Err(); err != nil {
		return cal, fmt.Errorf("storage: iterate agent calibration: %w", err)
	}

	return cal, nil
}

// ComputeCalibrated determines whether confidence predicts outcomes.
// With outcome data: high-conf avg_outcome must be >= mid-conf avg_outcome.
// Without: high-conf revision rate must be <= mid-conf revision rate.
// Exported so the sqlite package can reuse it.
func ComputeCalibrated(tiers map[string]*ConfidenceTier, hasOutcomeData bool) bool {
	high, mid := tiers["high"], tiers["mid"]
	if high == nil || mid == nil {
		return true // insufficient data to determine miscalibration
	}

	if hasOutcomeData && high.AvgOutcome != nil && mid.AvgOutcome != nil {
		return *high.AvgOutcome >= *mid.AvgOutcome
	}

	// Temporal proxy: high-confidence decisions should not be revised more often.
	if high.Total >= 5 && mid.Total >= 5 {
		return high.RevisionRate <= mid.RevisionRate
	}

	return true // not enough data to call it miscalibrated
}

// bucketCount is a sql.Scanner adapter that appends a ConfidenceBucket to the
// distribution's Buckets slice when scanned. This avoids 10 temporary variables.
type bucketCount struct {
	dist *ConfidenceDistribution
	idx  int
}

var bucketLabels = [10]string{
	"0.0-0.1", "0.1-0.2", "0.2-0.3", "0.3-0.4", "0.4-0.5",
	"0.5-0.6", "0.6-0.7", "0.7-0.8", "0.8-0.9", "0.9-1.0",
}

func (b *bucketCount) Scan(src any) error {
	var count int
	switch v := src.(type) {
	case int64:
		count = int(v)
	case int32:
		count = int(v)
	case int:
		count = v
	default:
		return fmt.Errorf("bucketCount: unsupported type %T", src)
	}
	b.dist.Buckets = append(b.dist.Buckets, ConfidenceBucket{
		Bucket: bucketLabels[b.idx],
		Count:  count,
	})
	return nil
}
