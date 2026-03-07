package model

import "time"

// ConflictAnalytics is the response for GET /v1/conflicts/analytics.
// It aggregates conflict data over a time period into summary stats,
// breakdowns by agent pair / decision type / severity, and a daily trend.
type ConflictAnalytics struct {
	Period         TimePeriod                  `json:"period"`
	Summary        ConflictAnalyticsSummary    `json:"summary"`
	ByAgentPair    []AgentPairConflictStats    `json:"by_agent_pair"`
	ByDecisionType []DecisionTypeConflictStats `json:"by_decision_type"`
	BySeverity     []SeverityConflictStats     `json:"by_severity"`
	Trend          []ConflictTrendPoint        `json:"trend"`
}

// TimePeriod defines the start and end of an analytics window.
type TimePeriod struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// ConflictAnalyticsSummary holds top-level aggregate metrics for the period.
type ConflictAnalyticsSummary struct {
	TotalDetected             int      `json:"total_detected"`
	TotalResolved             int      `json:"total_resolved"`
	MeanTimeToResolutionHours *float64 `json:"mean_time_to_resolution_hours"`
	FalsePositiveRate         float64  `json:"false_positive_rate"`
}

// AgentPairConflictStats shows conflict counts for one agent pair.
type AgentPairConflictStats struct {
	AgentA   string `json:"agent_a"`
	AgentB   string `json:"agent_b"`
	Count    int    `json:"count"`
	Open     int    `json:"open"`
	Resolved int    `json:"resolved"`
}

// DecisionTypeConflictStats shows conflict counts and average significance for one decision type.
type DecisionTypeConflictStats struct {
	DecisionType    string  `json:"decision_type"`
	Count           int     `json:"count"`
	AvgSignificance float64 `json:"avg_significance"`
}

// SeverityConflictStats shows the number of conflicts at a given severity level.
type SeverityConflictStats struct {
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

// ConflictTrendPoint holds detected and resolved counts for a single day.
type ConflictTrendPoint struct {
	Date     string `json:"date"` // YYYY-MM-DD
	Detected int    `json:"detected"`
	Resolved int    `json:"resolved"`
}
