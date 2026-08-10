//go:build !lite && integration

package storage_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
)

// A binding conflict has no false-positive rate to estimate — it is a join, and
// its correctness is entirely in the scoping conditions. Each test here pins one
// of them, because a missing condition does not fail loudly: it produces a
// confident conflict that is wrong, which is worse than the judge being unsure.

func bindingDecision(t *testing.T, ctx context.Context, agentID, outcome string, project *string, bindings []model.Binding) model.Decision {
	t.Helper()
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// decisions.project is a generated column derived from agent_context
	// (migration 052), so project scoping is exercised by setting the context
	// rather than the column.
	agentCtx := map[string]any{}
	if project != nil {
		agentCtx["project"] = *project
	}
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "architecture", Outcome: outcome, Confidence: 0.8,
		AgentContext: agentCtx,
	})
	require.NoError(t, err)
	d.Project = project

	canon, err := model.CanonicalizeBindings(bindings)
	require.NoError(t, err)
	for i := range canon {
		canon[i].DecisionID = d.ID
		canon[i].OrgID = uuid.Nil
	}
	require.NoError(t, testDB.CreateBindingsBatch(ctx, canon))
	d.Bindings = canon
	return d
}

func bind(parameter, value string) model.Binding {
	return model.Binding{Parameter: parameter, Value: value}
}

func TestFindConflictingBindings_DifferentValueConflicts(t *testing.T) {
	ctx := context.Background()
	proj := "binding-test-" + uuid.New().String()[:8]
	param := "qdrant.create_field_index_on_startup." + uuid.New().String()[:8]

	first := bindingDecision(t, ctx, "planner", "run index creation at startup", &proj,
		[]model.Binding{bind(param, "true")})
	second := bindingDecision(t, ctx, "reviewer", "use versioned migrations instead", &proj,
		[]model.Binding{bind(param, "false")})

	got, err := testDB.FindConflictingBindings(ctx, second.ID, uuid.Nil, second.Bindings)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, first.ID, got[0].OtherDecisionID)
	assert.Equal(t, "true", got[0].OtherValue)
}

// Agreement is not conflict. Two agents independently setting a parameter the
// same way is the system working.
func TestFindConflictingBindings_SameValueIsNotAConflict(t *testing.T) {
	ctx := context.Background()
	proj := "binding-agree-" + uuid.New().String()[:8]
	param := "cache.ttl." + uuid.New().String()[:8]

	bindingDecision(t, ctx, "planner", "cache for five minutes", &proj, []model.Binding{bind(param, "5m")})
	second := bindingDecision(t, ctx, "reviewer", "confirmed five minute cache", &proj,
		[]model.Binding{bind(param, "5m")})

	got, err := testDB.FindConflictingBindings(ctx, second.ID, uuid.Nil, second.Bindings)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// Spelling variants of one parameter name must join, or two agents describing
// the same knob differently silently never conflict.
func TestFindConflictingBindings_SpellingVariantsJoin(t *testing.T) {
	ctx := context.Background()
	proj := "binding-spelling-" + uuid.New().String()[:8]
	suffix := uuid.New().String()[:8]

	bindingDecision(t, ctx, "planner", "camel spelling", &proj,
		[]model.Binding{bind("cacheEvictionPolicy"+suffix, "lru")})
	second := bindingDecision(t, ctx, "reviewer", "snake spelling", &proj,
		[]model.Binding{bind("cache_eviction_policy"+suffix, "lfu")})

	got, err := testDB.FindConflictingBindings(ctx, second.ID, uuid.Nil, second.Bindings)
	require.NoError(t, err)
	assert.Len(t, got, 1, "camelCase and snake_case spellings of one parameter must join")
}

// Two services can legitimately set a parameter of the same name to different
// values. Cross-project collision was the leading false-positive source before
// projects were recorded, and it must not come back through this path.
func TestFindConflictingBindings_DifferentProjectsDoNotConflict(t *testing.T) {
	ctx := context.Background()
	param := "log.level." + uuid.New().String()[:8]
	projA := "svc-a-" + uuid.New().String()[:8]
	projB := "svc-b-" + uuid.New().String()[:8]

	bindingDecision(t, ctx, "planner", "service A logs at debug", &projA, []model.Binding{bind(param, "debug")})
	second := bindingDecision(t, ctx, "planner", "service B logs at info", &projB, []model.Binding{bind(param, "info")})

	got, err := testDB.FindConflictingBindings(ctx, second.ID, uuid.Nil, second.Bindings)
	require.NoError(t, err)
	assert.Empty(t, got, "the same parameter name in two projects is not one parameter")
}

// A superseded decision no longer binds anything. Without the valid_to filter a
// decision would conflict with the very decision that replaced it, which is the
// most confusing possible output: the fix reported as the problem.
func TestFindConflictingBindings_SupersededDecisionsExcluded(t *testing.T) {
	ctx := context.Background()
	proj := "binding-superseded-" + uuid.New().String()[:8]
	param := "retry.max." + uuid.New().String()[:8]

	first := bindingDecision(t, ctx, "planner", "retry three times", &proj, []model.Binding{bind(param, "3")})
	_, err := testDB.Pool().Exec(ctx, `UPDATE decisions SET valid_to = now() WHERE id = $1`, first.ID)
	require.NoError(t, err)

	second := bindingDecision(t, ctx, "planner", "retry five times", &proj, []model.Binding{bind(param, "5")})
	got, err := testDB.FindConflictingBindings(ctx, second.ID, uuid.Nil, second.Bindings)
	require.NoError(t, err)
	assert.Empty(t, got, "a retired decision must not conflict with its own replacement")
}

// Re-tracing a decision must not make it conflict with itself.
func TestFindConflictingBindings_ExcludesSelf(t *testing.T) {
	ctx := context.Background()
	proj := "binding-self-" + uuid.New().String()[:8]
	param := "flag." + uuid.New().String()[:8]

	d := bindingDecision(t, ctx, "planner", "set the flag", &proj, []model.Binding{bind(param, "on")})
	got, err := testDB.FindConflictingBindings(ctx, d.ID, uuid.Nil, d.Bindings)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// Decisions with no project recorded form their own scope; they must not match
// project-scoped decisions, since "unknown project" is not a wildcard.
func TestFindConflictingBindings_NullProjectIsItsOwnScope(t *testing.T) {
	ctx := context.Background()
	param := "timeout." + uuid.New().String()[:8]
	proj := "known-" + uuid.New().String()[:8]

	bindingDecision(t, ctx, "planner", "no project recorded", nil, []model.Binding{bind(param, "10s")})
	withProject := bindingDecision(t, ctx, "planner", "project recorded", &proj, []model.Binding{bind(param, "30s")})

	got, err := testDB.FindConflictingBindings(ctx, withProject.ID, uuid.Nil, withProject.Bindings)
	require.NoError(t, err)
	assert.Empty(t, got, "an unrecorded project must not act as a wildcard")

	// But two project-less decisions do share a scope.
	otherNull := bindingDecision(t, ctx, "reviewer", "also no project", nil, []model.Binding{bind(param, "60s")})
	got, err = testDB.FindConflictingBindings(ctx, otherNull.ID, uuid.Nil, otherNull.Bindings)
	require.NoError(t, err)
	assert.Len(t, got, 1, "two project-less decisions share the project-less scope")
}

// The unique constraint is what stops a decision contradicting itself in one
// trace; CanonicalizeBindings refuses it earlier, so this pins the backstop.
func TestCreateBindingsBatch_RejectsDuplicateParameterPerDecision(t *testing.T) {
	ctx := context.Background()
	proj := "binding-dup-" + uuid.New().String()[:8]
	d := bindingDecision(t, ctx, "planner", "first binding", &proj,
		[]model.Binding{bind("dup.param."+uuid.New().String()[:8], "a")})

	dup := d.Bindings[0]
	dup.ID = uuid.New()
	dup.Value, dup.ValueKey = "b", "b"
	err := testDB.CreateBindingsBatch(ctx, []model.Binding{dup})
	assert.Error(t, err, "the unique constraint must reject a second value for one parameter on one decision")
}

// A binding written without canonical keys would be stored blank and never
// join, failing silently. The writer refuses it instead.
func TestCreateBindingsBatch_RefusesUncanonicalizedBinding(t *testing.T) {
	ctx := context.Background()
	err := testDB.CreateBindingsBatch(ctx, []model.Binding{{
		ID: uuid.New(), DecisionID: uuid.New(), OrgID: uuid.Nil,
		Parameter: "p", Value: "v", // no ParameterKey / ValueKey
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canonical key")
}
