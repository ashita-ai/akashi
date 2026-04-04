// Package autoassess generates outcome assessments automatically from
// observable signals in the decision trail. This closes the feedback loop
// that was missing (zero manual assessments across 824 decisions).
//
// Three signal sources:
//   - Supersession: when decision B sets supersedes_id=A, A is assessed as
//     partially_correct (useful enough to build on, but needed revision).
//   - Conflict resolution: when a conflict is resolved, the winner is assessed
//     as correct and the loser as incorrect.
//   - Citation threshold: when a decision accumulates >= 3 precedent citations,
//     it is assessed as correct (other agents found it useful).
package autoassess

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/quality"
)

// CitationThreshold is the number of precedent citations required to trigger
// an auto-assessment of "correct".
const CitationThreshold = 3

// Store is the subset of storage.Store used by the auto-assessor.
type Store interface {
	CreateAssessment(ctx context.Context, orgID uuid.UUID, a model.DecisionAssessment) (model.DecisionAssessment, error)
	UpdateOutcomeScore(ctx context.Context, orgID, decisionID uuid.UUID, score *float32) error
	GetAssessmentSummary(ctx context.Context, orgID, decisionID uuid.UUID) (model.AssessmentSummary, error)
}

// Assessor generates auto-assessments from observable signals.
type Assessor struct {
	db     Store
	logger *slog.Logger
}

// New creates an Assessor.
func New(db Store, logger *slog.Logger) *Assessor {
	return &Assessor{db: db, logger: logger}
}

// OnSuperseded records that a decision was superseded by a newer one.
// The superseded decision is assessed as partially_correct.
func (a *Assessor) OnSuperseded(ctx context.Context, orgID, supersededID, newID uuid.UUID) {
	notes := fmt.Sprintf("Auto-assessed: superseded by decision %s", newID)
	a.assess(ctx, orgID, supersededID, model.AssessmentPartiallyCorrect, notes, model.AssessmentSourceSupersession)
}

// OnConflictResolved records the outcome of a conflict resolution.
// The winner is assessed as correct; the loser as incorrect.
func (a *Assessor) OnConflictResolved(ctx context.Context, orgID, winnerID, loserID uuid.UUID) {
	winNotes := fmt.Sprintf("Auto-assessed: won conflict resolution against decision %s", loserID)
	a.assess(ctx, orgID, winnerID, model.AssessmentCorrect, winNotes, model.AssessmentSourceConflict)

	loseNotes := fmt.Sprintf("Auto-assessed: lost conflict resolution to decision %s", winnerID)
	a.assess(ctx, orgID, loserID, model.AssessmentIncorrect, loseNotes, model.AssessmentSourceConflict)
}

// OnCitationThreshold checks whether a decision has reached the citation
// threshold and, if so, assesses it as correct.
func (a *Assessor) OnCitationThreshold(ctx context.Context, orgID, decisionID uuid.UUID, citationCount int) {
	if citationCount < CitationThreshold {
		return
	}
	notes := fmt.Sprintf("Auto-assessed: cited as precedent %d times (threshold: %d)", citationCount, CitationThreshold)
	a.assess(ctx, orgID, decisionID, model.AssessmentCorrect, notes, model.AssessmentSourceCitation)
}

// assess creates an assessment and updates the outcome score. All errors are
// logged and swallowed — auto-assessment is non-fatal.
func (a *Assessor) assess(ctx context.Context, orgID, decisionID uuid.UUID, outcome model.AssessmentOutcome, notes, source string) {
	_, err := a.db.CreateAssessment(ctx, orgID, model.DecisionAssessment{
		DecisionID:      decisionID,
		OrgID:           orgID,
		AssessorAgentID: "system:" + source,
		Outcome:         outcome,
		Notes:           &notes,
		Source:          source,
	})
	if err != nil {
		a.logger.Warn("autoassess: failed to create assessment",
			"decision_id", decisionID, "source", source, "error", err)
		return
	}

	// Update the outcome score on the decision.
	summary, err := a.db.GetAssessmentSummary(ctx, orgID, decisionID)
	if err != nil {
		a.logger.Warn("autoassess: failed to get assessment summary",
			"decision_id", decisionID, "error", err)
		return
	}
	score := quality.ComputeOutcomeScore(summary)
	if err := a.db.UpdateOutcomeScore(ctx, orgID, decisionID, score); err != nil {
		a.logger.Warn("autoassess: failed to update outcome score",
			"decision_id", decisionID, "error", err)
	}
}
