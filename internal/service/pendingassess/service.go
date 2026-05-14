// Package pendingassess surfaces decisions that are past their per-type
// outcome-assessment window and have no recorded assessment from any source.
// It is the shared business-logic layer behind both
// GET /v1/decisions/pending-assessment and the akashi_pending_assessments
// MCP tool — both call ListPending to keep windowing and access-filter
// behavior identical across surfaces.
//
// Design rationale (issue #716, May 2026 field assessment):
//
//   - Manual outcome-assessment coverage was 2.3% across 1,292 traced decisions.
//     Without outcome labels, calibration metrics are meaningless. The fix is
//     a prompt loop: surface decisions past a per-type window so agents can
//     follow up via akashi_assess.
//
//   - "Any source counts as assessed" — supersession, conflict resolution, and
//     citation-threshold auto-assessments already produce verdicts. Re-prompting
//     would be redundant noise and risks the same dismissal pattern that hurt
//     the conflict detector at 76% FP.
//
//   - Per-type windows are configured at boot (see config.AssessmentWindows).
//     Types with window=0 are excluded from the surface entirely.
package pendingassess

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

// Service computes pending-assessment lists from configured windows.
type Service struct {
	db           storage.Store
	windows      map[string]time.Duration
	defaultLimit int
}

// New constructs a Service. windows maps decision_type → minimum age before
// a decision is eligible for prompting; entries with value 0 are ignored.
// defaultLimit applies when the caller does not specify one.
func New(db storage.Store, windows map[string]time.Duration, defaultLimit int) *Service {
	if defaultLimit <= 0 {
		defaultLimit = 10
	}
	return &Service{db: db, windows: windows, defaultLimit: defaultLimit}
}

// ListInput parameterizes a pending-assessment query.
//
// AgentIDs scopes results: nil means no agent-level filter (used by admin
// callers); a populated slice restricts results to those agent IDs; an
// explicit empty slice yields zero rows (caller has access to no agents).
//
// DecisionType, when non-empty, narrows to a single type. The type must
// have a window configured; types with window=0 always return zero rows.
//
// Project, when non-nil, filters by project (cross-project hygiene).
type ListInput struct {
	AgentIDs     []string
	DecisionType string
	Project      *string
	Limit        int
}

// ListPending returns pending-assessment rows visible to the caller.
//
// Returns (nil, nil) without hitting the DB when no configured windows match
// the requested DecisionType (or when no types are configured at all). This
// preserves the type opt-out semantics — an opted-out type is invisible, not
// an error.
func (s *Service) ListPending(ctx context.Context, orgID uuid.UUID, in ListInput) ([]model.PendingAssessment, error) {
	now := time.Now().UTC()
	windows := s.activeWindows(in.DecisionType, now)
	if len(windows) == 0 {
		return nil, nil
	}

	limit := in.Limit
	if limit <= 0 {
		limit = s.defaultLimit
	}
	if limit > 100 {
		limit = 100
	}

	opts := storage.ListPendingAssessmentsOpts{
		Windows:  windows,
		AgentIDs: in.AgentIDs,
		Project:  in.Project,
		Limit:    limit,
	}
	out, err := s.db.ListPendingAssessments(ctx, orgID, opts)
	if err != nil {
		return nil, fmt.Errorf("pendingassess: list: %w", err)
	}
	return out, nil
}

// HasConfiguredType reports whether a decision_type has a non-zero configured
// window. Used by handlers/tools that want to short-circuit a 400-style hint
// before issuing the query.
func (s *Service) HasConfiguredType(decisionType string) bool {
	d, ok := s.windows[decisionType]
	return ok && d > 0
}

// activeWindows derives the per-type cutoffs for the storage call. When
// onlyType is empty, all configured types with window>0 are included.
// Otherwise only that type is included (and only if its window is enabled).
func (s *Service) activeWindows(onlyType string, now time.Time) []storage.PendingAssessmentWindow {
	if onlyType != "" {
		d, ok := s.windows[onlyType]
		if !ok || d <= 0 {
			return nil
		}
		return []storage.PendingAssessmentWindow{{
			DecisionType: onlyType,
			Cutoff:       now.Add(-d),
		}}
	}
	out := make([]storage.PendingAssessmentWindow, 0, len(s.windows))
	for t, d := range s.windows {
		if d <= 0 {
			continue
		}
		out = append(out, storage.PendingAssessmentWindow{
			DecisionType: t,
			Cutoff:       now.Add(-d),
		})
	}
	return out
}
