//go:build !lite

package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

func TestBuildDecisionWhereClause_CurrentOnly(t *testing.T) {
	orgID := uuid.New()

	t.Run("currentOnly=true includes valid_to IS NULL", func(t *testing.T) {
		where, args := buildDecisionWhereClause(orgID, model.QueryFilters{}, 1, true)
		assert.Contains(t, where, "valid_to IS NULL")
		require.Len(t, args, 1)
		assert.Equal(t, orgID, args[0])
	})

	t.Run("currentOnly=false omits valid_to IS NULL", func(t *testing.T) {
		where, args := buildDecisionWhereClause(orgID, model.QueryFilters{}, 1, false)
		assert.NotContains(t, where, "valid_to IS NULL")
		require.Len(t, args, 1)
		assert.Equal(t, orgID, args[0])
	})
}

func TestBuildDecisionWhereClause_ToolFilter(t *testing.T) {
	orgID := uuid.New()
	tool := "claude-code"
	filters := model.QueryFilters{Tool: &tool}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	// tool is a generated column — filter uses simple equality, not COALESCE.
	assert.Contains(t, where, "tool = $2")
	assert.NotContains(t, where, "COALESCE")
	require.Len(t, args, 2) // org_id + tool
	assert.Equal(t, "claude-code", args[1])
}

func TestBuildDecisionWhereClause_ModelFilter(t *testing.T) {
	orgID := uuid.New()
	model_ := "claude-opus-4-6"
	filters := model.QueryFilters{Model: &model_}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	// model is a generated column — filter uses simple equality, not COALESCE.
	assert.Contains(t, where, "model = $2")
	assert.NotContains(t, where, "COALESCE")
	require.Len(t, args, 2)
	assert.Equal(t, "claude-opus-4-6", args[1])
}

func TestBuildDecisionWhereClause_RepoFilter(t *testing.T) {
	orgID := uuid.New()
	repo := "ashita-ai/akashi"
	filters := model.QueryFilters{Project: &repo}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	// project is a generated column — filter uses simple equality, not COALESCE.
	assert.Contains(t, where, "project = $2")
	assert.NotContains(t, where, "COALESCE")
	require.Len(t, args, 2)
	assert.Equal(t, "ashita-ai/akashi", args[1])
}

func TestBuildDecisionWhereClause_AllFilters(t *testing.T) {
	orgID := uuid.New()
	runID := uuid.New()
	decType := "architecture"
	confMin := float32(0.7)
	outcome := "chose Redis"
	sessionID := uuid.New()
	tool := "claude-code"
	mdl := "claude-opus-4-6"
	repo := "ashita-ai/akashi"

	filters := model.QueryFilters{
		AgentIDs:      []string{"planner", "coder"},
		RunID:         &runID,
		DecisionType:  &decType,
		ConfidenceMin: &confMin,
		Outcome:       &outcome,
		SessionID:     &sessionID,
		Tool:          &tool,
		Model:         &mdl,
		Project:       &repo,
	}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	// org_id + agent_ids + run_id + decision_type + confidence_min + outcome
	// + session_id + tool + model + project = 10 args
	require.Len(t, args, 10)

	// Verify all conditions are present.
	assert.Contains(t, where, "org_id = $1")
	assert.Contains(t, where, "valid_to IS NULL")
	assert.Contains(t, where, "agent_id = ANY($2)")
	assert.Contains(t, where, "run_id = $3")
	assert.Contains(t, where, "decision_type = $4")
	assert.Contains(t, where, "confidence >= $5")
	assert.Contains(t, where, "outcome = $6")
	assert.Contains(t, where, "session_id = $7")
	assert.Contains(t, where, "tool = $8")
	assert.Contains(t, where, "model = $9")
	assert.Contains(t, where, "project = $10")
}

func TestBuildDecisionWhereClause_ArgIndexing(t *testing.T) {
	// Verify that startArgIdx=3 shifts all parameter indices correctly.
	orgID := uuid.New()
	tool := "cursor"
	filters := model.QueryFilters{Tool: &tool}

	where, args := buildDecisionWhereClause(orgID, filters, 3, false)

	assert.Contains(t, where, "org_id = $3")
	assert.Contains(t, where, "tool = $4")
	require.Len(t, args, 2)
}

func TestBuildDecisionWhereClause_OrgIsolationAlwaysFirst(t *testing.T) {
	orgID := uuid.New()

	where, _ := buildDecisionWhereClause(orgID, model.QueryFilters{}, 1, false)

	// Org_id should be the first condition in the WHERE clause.
	assert.True(t, strings.HasPrefix(where, " WHERE org_id = $1"),
		"org_id should be the first condition, got: %s", where)
}

func TestBuildDecisionWhereClause_TimeRangeFromOnly(t *testing.T) {
	orgID := uuid.New()
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	filters := model.QueryFilters{
		TimeRange: &model.TimeRange{From: &from},
	}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "valid_from >= $2")
	assert.NotContains(t, where, "valid_from <=")
	require.Len(t, args, 2) // org_id + from
}

func TestBuildDecisionWhereClause_TimeRangeToOnly(t *testing.T) {
	orgID := uuid.New()
	to := time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC)
	filters := model.QueryFilters{
		TimeRange: &model.TimeRange{To: &to},
	}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "valid_from <= $2")
	assert.NotContains(t, where, "valid_from >= $")
	require.Len(t, args, 2) // org_id + to
}

func TestBuildDecisionWhereClause_TimeRangeBothFromAndTo(t *testing.T) {
	orgID := uuid.New()
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC)
	filters := model.QueryFilters{
		TimeRange: &model.TimeRange{From: &from, To: &to},
	}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "valid_from >= $2")
	assert.Contains(t, where, "valid_from <= $3")
	require.Len(t, args, 3) // org_id + from + to
}

func TestBuildDecisionWhereClause_OutcomeFilter(t *testing.T) {
	orgID := uuid.New()
	outcome := "chose Postgres"
	filters := model.QueryFilters{Outcome: &outcome}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "outcome = $2")
	require.Len(t, args, 2)
	assert.Equal(t, "chose Postgres", args[1])
}

func TestBuildDecisionWhereClause_SessionIDFilter(t *testing.T) {
	orgID := uuid.New()
	sessionID := uuid.New()
	filters := model.QueryFilters{SessionID: &sessionID}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "session_id = $2")
	require.Len(t, args, 2)
	assert.Equal(t, sessionID, args[1])
}

func TestBuildDecisionWhereClause_ConfidenceMinFilter(t *testing.T) {
	orgID := uuid.New()
	confMin := float32(0.75)
	filters := model.QueryFilters{ConfidenceMin: &confMin}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "confidence >= $2")
	require.Len(t, args, 2)
	assert.Equal(t, float32(0.75), args[1])
}

func TestBuildDecisionWhereClause_RunIDFilter(t *testing.T) {
	orgID := uuid.New()
	runID := uuid.New()
	filters := model.QueryFilters{RunID: &runID}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "run_id = $2")
	require.Len(t, args, 2)
	assert.Equal(t, runID, args[1])
}

func TestBuildDecisionWhereClause_DecisionTypeFilter(t *testing.T) {
	orgID := uuid.New()
	dt := "library-choice"
	filters := model.QueryFilters{DecisionType: &dt}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "decision_type = $2")
	require.Len(t, args, 2)
	assert.Equal(t, "library-choice", args[1])
}

func TestBuildDecisionWhereClause_AgentIDsFilter(t *testing.T) {
	orgID := uuid.New()
	filters := model.QueryFilters{AgentIDs: []string{"agent-a", "agent-b"}}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "agent_id = ANY($2)")
	require.Len(t, args, 2)
}

func TestBuildDecisionWhereClause_EmptyFilters(t *testing.T) {
	orgID := uuid.New()
	filters := model.QueryFilters{}

	where, args := buildDecisionWhereClause(orgID, filters, 1, true)

	assert.Contains(t, where, "org_id = $1")
	assert.Contains(t, where, "valid_to IS NULL")
	// No extra filter conditions, just org_id.
	require.Len(t, args, 1)
}

func TestContainsStr(t *testing.T) {
	assert.True(t, containsStr([]string{"a", "b", "c"}, "b"))
	assert.False(t, containsStr([]string{"a", "b", "c"}, "d"))
	assert.False(t, containsStr(nil, "a"))
	assert.False(t, containsStr([]string{}, "a"))
}
