package autoassess

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// fakeStore records CreateAssessment calls and returns configurable results.
type fakeStore struct {
	assessments []model.DecisionAssessment
	createErr   error
	summary     model.AssessmentSummary
	summaryErr  error
	scoreErr    error

	updateScoreCalls []updateScoreCall
}

type updateScoreCall struct {
	DecisionID uuid.UUID
	Score      *float32
}

func (f *fakeStore) CreateAssessment(_ context.Context, _ uuid.UUID, a model.DecisionAssessment) (model.DecisionAssessment, error) {
	if f.createErr != nil {
		return model.DecisionAssessment{}, f.createErr
	}
	a.ID = uuid.New()
	f.assessments = append(f.assessments, a)
	return a, nil
}

func (f *fakeStore) GetAssessmentSummary(_ context.Context, _ uuid.UUID, _ uuid.UUID) (model.AssessmentSummary, error) {
	return f.summary, f.summaryErr
}

func (f *fakeStore) UpdateOutcomeScore(_ context.Context, _ uuid.UUID, decisionID uuid.UUID, score *float32) error {
	f.updateScoreCalls = append(f.updateScoreCalls, updateScoreCall{decisionID, score})
	return f.scoreErr
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestOnSuperseded(t *testing.T) {
	store := &fakeStore{summary: model.AssessmentSummary{Total: 1, PartiallyCorrect: 1}}
	a := New(store, testLogger())

	supersededID := uuid.New()
	newID := uuid.New()
	a.OnSuperseded(context.Background(), uuid.Nil, supersededID, newID)

	require.Len(t, store.assessments, 1)
	assess := store.assessments[0]
	assert.Equal(t, supersededID, assess.DecisionID)
	assert.Equal(t, model.AssessmentPartiallyCorrect, assess.Outcome)
	assert.Equal(t, model.AssessmentSourceSupersession, assess.Source)
	assert.Equal(t, "system:supersession", assess.AssessorAgentID)
	assert.Contains(t, *assess.Notes, newID.String())

	require.Len(t, store.updateScoreCalls, 1)
	assert.Equal(t, supersededID, store.updateScoreCalls[0].DecisionID)
}

func TestOnConflictResolved(t *testing.T) {
	store := &fakeStore{summary: model.AssessmentSummary{Total: 1, Correct: 1}}
	a := New(store, testLogger())

	winnerID := uuid.New()
	loserID := uuid.New()
	a.OnConflictResolved(context.Background(), uuid.Nil, winnerID, loserID)

	require.Len(t, store.assessments, 2)

	// Winner assessed as correct.
	assert.Equal(t, winnerID, store.assessments[0].DecisionID)
	assert.Equal(t, model.AssessmentCorrect, store.assessments[0].Outcome)
	assert.Equal(t, model.AssessmentSourceConflict, store.assessments[0].Source)

	// Loser assessed as incorrect.
	assert.Equal(t, loserID, store.assessments[1].DecisionID)
	assert.Equal(t, model.AssessmentIncorrect, store.assessments[1].Outcome)
	assert.Equal(t, model.AssessmentSourceConflict, store.assessments[1].Source)
}

func TestOnCitationThreshold_BelowThreshold(t *testing.T) {
	store := &fakeStore{}
	a := New(store, testLogger())

	a.OnCitationThreshold(context.Background(), uuid.Nil, uuid.New(), CitationThreshold-1)
	assert.Empty(t, store.assessments, "should not create assessment below threshold")
}

func TestOnCitationThreshold_AtThreshold(t *testing.T) {
	store := &fakeStore{summary: model.AssessmentSummary{Total: 1, Correct: 1}}
	a := New(store, testLogger())

	decisionID := uuid.New()
	a.OnCitationThreshold(context.Background(), uuid.Nil, decisionID, CitationThreshold)

	require.Len(t, store.assessments, 1)
	assert.Equal(t, decisionID, store.assessments[0].DecisionID)
	assert.Equal(t, model.AssessmentCorrect, store.assessments[0].Outcome)
	assert.Equal(t, model.AssessmentSourceCitation, store.assessments[0].Source)
}

func TestOnCitationThreshold_AboveThreshold(t *testing.T) {
	store := &fakeStore{summary: model.AssessmentSummary{Total: 1, Correct: 1}}
	a := New(store, testLogger())

	decisionID := uuid.New()
	a.OnCitationThreshold(context.Background(), uuid.Nil, decisionID, CitationThreshold+5)

	require.Len(t, store.assessments, 1)
	assert.Equal(t, model.AssessmentCorrect, store.assessments[0].Outcome)
}

func TestAssess_CreateError_LogsAndContinues(t *testing.T) {
	store := &fakeStore{createErr: fmt.Errorf("db connection lost")}
	a := New(store, testLogger())

	// Should not panic.
	a.OnSuperseded(context.Background(), uuid.Nil, uuid.New(), uuid.New())
	assert.Empty(t, store.assessments)
	assert.Empty(t, store.updateScoreCalls, "should not update score on create failure")
}

func TestAssess_SummaryError_LogsAndContinues(t *testing.T) {
	store := &fakeStore{summaryErr: fmt.Errorf("summary query failed")}
	a := New(store, testLogger())

	a.OnSuperseded(context.Background(), uuid.Nil, uuid.New(), uuid.New())
	require.Len(t, store.assessments, 1, "assessment should still be created")
	assert.Empty(t, store.updateScoreCalls, "should not update score on summary failure")
}
