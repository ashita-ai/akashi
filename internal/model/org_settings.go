package model

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// AutoResolveWinner is the strategy for selecting the winning decision
// during auto-resolution of conflicts.
type AutoResolveWinner string

const (
	WinnerRecency    AutoResolveWinner = "recency"
	WinnerConfidence AutoResolveWinner = "confidence"
	WinnerConsensus  AutoResolveWinner = "consensus"
)

// ReopenedPolicy controls what happens when a conflict reopens a prior resolution.
type ReopenedPolicy string

const (
	ReopenEscalate ReopenedPolicy = "escalate"
)

// ConflictResolutionPolicy holds the auto-resolution policy for an org's conflicts.
type ConflictResolutionPolicy struct {
	AutoResolveAfterDays       int               `json:"auto_resolve_after_days"`
	AutoResolveWinner          AutoResolveWinner `json:"auto_resolve_winner"`
	AutoResolveMaxSeverity     string            `json:"auto_resolve_max_severity"`
	NeverAutoResolveSeverities []string          `json:"never_auto_resolve_severities"`
	ReopenedResolutionPolicy   ReopenedPolicy    `json:"reopened_resolution_policy"`
}

// Validate checks that the policy is well-formed.
func (p *ConflictResolutionPolicy) Validate() error {
	if p.AutoResolveAfterDays < 1 {
		return fmt.Errorf("auto_resolve_after_days must be >= 1")
	}
	switch p.AutoResolveWinner {
	case WinnerRecency, WinnerConfidence, WinnerConsensus:
	default:
		return fmt.Errorf("auto_resolve_winner must be one of: recency, confidence, consensus")
	}
	validSeverities := map[string]bool{"low": true, "medium": true, "high": true, "critical": true}
	if !validSeverities[p.AutoResolveMaxSeverity] {
		return fmt.Errorf("auto_resolve_max_severity must be one of: low, medium, high, critical")
	}
	for _, s := range p.NeverAutoResolveSeverities {
		if !validSeverities[s] {
			return fmt.Errorf("invalid severity in never_auto_resolve_severities: %s", s)
		}
	}
	switch p.ReopenedResolutionPolicy {
	case ReopenEscalate, "":
	default:
		return fmt.Errorf("reopened_resolution_policy must be: escalate")
	}
	return nil
}

// OrgSettingsData is the JSONB payload stored in org_settings.settings.
type OrgSettingsData struct {
	ConflictResolution *ConflictResolutionPolicy `json:"conflict_resolution,omitempty"`
}

// OrgSettings is a row from the org_settings table.
type OrgSettings struct {
	OrgID     uuid.UUID       `json:"org_id"`
	Settings  OrgSettingsData `json:"settings"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

// SeverityRank maps severity strings to ordinals for comparison.
// Higher rank = more severe.
func SeverityRank(s string) int {
	switch s {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "critical":
		return 4
	default:
		return 0
	}
}
