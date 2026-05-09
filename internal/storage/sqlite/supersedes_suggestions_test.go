package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
)

func TestInsertSupersedesSuggestion_BasicAndIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	a := createTestDecision(t, db, orgID, "claude-code", "ARD-958 layer-2 implementation")
	b := createTestDecision(t, db, orgID, "claude-code", "ARD-958 layer-3 refinement")

	conf := float32(0.82)
	first := storage.SupersedesSuggestionInsert{
		OrgID:         orgID,
		SupersedingID: b.ID,
		SupersededID:  a.ID,
		SuggestedBy:   "detector:same_agent_same_ticket",
		Confidence:    &conf,
		Reason:        `same agent "claude-code", same ticket "ARD-958"`,
	}
	require.NoError(t, db.InsertSupersedesSuggestion(ctx, first))

	got, err := db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{b.ID})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, b.ID, got[0].SupersedingID)
	assert.Equal(t, a.ID, got[0].SupersededID)
	assert.Equal(t, "detector:same_agent_same_ticket", got[0].SuggestedBy)
	require.NotNil(t, got[0].Confidence)
	assert.InDelta(t, 0.82, *got[0].Confidence, 0.001)
	assert.Equal(t, `same agent "claude-code", same ticket "ARD-958"`, got[0].Reason)

	// Re-insert is a no-op (ON CONFLICT DO NOTHING). Different reason on
	// retry is intentionally discarded — the first detector observation wins.
	second := first
	second.Reason = "noop on retry"
	require.NoError(t, db.InsertSupersedesSuggestion(ctx, second))

	got, err = db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{b.ID})
	require.NoError(t, err)
	require.Len(t, got, 1, "duplicate insert must not create a second row")
	assert.Equal(t, `same agent "claude-code", same ticket "ARD-958"`, got[0].Reason,
		"first observation's reason is preserved on retry")
}

func TestInsertSupersedesSuggestion_RejectsEmptySource(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	a := createTestDecision(t, db, orgID, "agent", "first")
	b := createTestDecision(t, db, orgID, "agent", "second")

	err := db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID:         orgID,
		SupersedingID: b.ID,
		SupersededID:  a.ID,
		SuggestedBy:   "",
	})
	require.Error(t, err, "empty suggested_by must be rejected")
}

func TestInsertSupersedesSuggestion_RejectsSelfReference(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	a := createTestDecision(t, db, orgID, "agent", "only")

	err := db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID:         orgID,
		SupersedingID: a.ID,
		SupersededID:  a.ID,
		SuggestedBy:   "detector:test",
	})
	require.Error(t, err, "self-supersession must be rejected")
}

func TestListSupersedesSuggestionsForDecisions_FiltersByOrgAndIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	a := createTestDecision(t, db, orgID, "agent", "a")
	b := createTestDecision(t, db, orgID, "agent", "b")
	c := createTestDecision(t, db, orgID, "agent", "c")

	require.NoError(t, db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID: orgID, SupersedingID: b.ID, SupersededID: a.ID,
		SuggestedBy: "detector:t",
	}))
	require.NoError(t, db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID: orgID, SupersedingID: c.ID, SupersededID: a.ID,
		SuggestedBy: "detector:t",
	}))

	// Empty input → nil, no error.
	out, err := db.ListSupersedesSuggestionsForDecisions(ctx, orgID, nil)
	require.NoError(t, err)
	assert.Empty(t, out)

	// Single ID returns its suggestion only.
	out, err = db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{c.ID})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, c.ID, out[0].SupersedingID)

	// Multiple IDs — both rows returned.
	out, err = db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{b.ID, c.ID})
	require.NoError(t, err)
	require.Len(t, out, 2)

	// Unknown ID returns no results without error.
	out, err = db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{uuid.New()})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestListSupersedesSuggestions_ExcludesConfirmedRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	a := createTestDecision(t, db, orgID, "agent", "first")
	// Confirmed link via supersedes_id triggers the migration-104 trigger,
	// which inserts a 'supersedes' row, not a 'suggested' one.
	supersedesA := a.ID
	_, b, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "agent",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "test-type",
			Outcome:      "explicit supersession",
			Confidence:   0.9,
			SupersedesID: &supersedesA,
		},
	})
	require.NoError(t, err)

	got, err := db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{b.ID})
	require.NoError(t, err)
	assert.Empty(t, got, "confirmed 'supersedes' rows must not appear in the suggestion list")
}

func TestDeleteOldSupersedesSuggestions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	a := createTestDecision(t, db, orgID, "agent", "a")
	b := createTestDecision(t, db, orgID, "agent", "b")

	require.NoError(t, db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID: orgID, SupersedingID: b.ID, SupersededID: a.ID,
		SuggestedBy: "detector:t",
	}))

	// Cutoff in the past — no rows pruned.
	n, err := db.DeleteOldSupersedesSuggestions(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	out, err := db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{b.ID})
	require.NoError(t, err)
	require.Len(t, out, 1)

	// Cutoff in the future — row pruned.
	n, err = db.DeleteOldSupersedesSuggestions(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	out, err = db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{b.ID})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestDeleteOldSupersedesSuggestions_DoesNotTouchConfirmedRows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	a := createTestDecision(t, db, orgID, "agent", "first")
	supersedesA := a.ID
	_, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "agent",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "test-type",
			Outcome:      "explicit supersession",
			Confidence:   0.9,
			SupersedesID: &supersedesA,
		},
	})
	require.NoError(t, err)

	// Wide cutoff — would prune everything if the WHERE clause was wrong.
	n, err := db.DeleteOldSupersedesSuggestions(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "confirmed rows must be untouched by the suggestion-only delete")
}

// TestTrigger_RetiresMatchingSuggestionOnConfirm verifies the SQLite mirror
// of migration 106's PG trigger extension: when an agent confirms a
// supersedes_id link, the matching latent suggestion from the same agent is
// dropped atomically with the confirmed-row insert.
func TestTrigger_RetiresMatchingSuggestionOnConfirm(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	earlier := createTestDecision(t, db, orgID, "claude-code", "ARD-958 layer-2")
	latent := createTestDecision(t, db, orgID, "claude-code", "ARD-958 layer-3")

	require.NoError(t, db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID:         orgID,
		SupersedingID: latent.ID,
		SupersededID:  earlier.ID,
		SuggestedBy:   "detector:same_agent_same_ticket",
	}))

	got, err := db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{latent.ID})
	require.NoError(t, err)
	require.Len(t, got, 1, "precondition: suggestion visible before confirm")

	supersedesEarlier := earlier.ID
	_, _, err = db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "claude-code",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "implementation",
			Outcome:      "ARD-958 explicit supersession",
			Confidence:   0.9,
			SupersedesID: &supersedesEarlier,
		},
	})
	require.NoError(t, err)

	got, err = db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{latent.ID})
	require.NoError(t, err)
	assert.Empty(t, got, "trigger must retire the latent suggestion on confirm")
}

// TestTrigger_RetireSuggestionScopedByAgent verifies the SQLite trigger only
// retires suggestions from the SAME agent that confirmed — suggestions from
// other agents pointing at the same predecessor must survive.
func TestTrigger_RetireSuggestionScopedByAgent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.EnsureDefaultOrg(ctx))
	orgID := uuid.Nil

	earlier := createTestDecision(t, db, orgID, "agent-A", "shared predecessor")
	latentA := createTestDecision(t, db, orgID, "agent-A", "agent A latent")
	latentB := createTestDecision(t, db, orgID, "agent-B", "agent B latent")

	require.NoError(t, db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID: orgID, SupersedingID: latentA.ID, SupersededID: earlier.ID,
		SuggestedBy: "detector:same_agent_same_ticket",
	}))
	require.NoError(t, db.InsertSupersedesSuggestion(ctx, storage.SupersedesSuggestionInsert{
		OrgID: orgID, SupersedingID: latentB.ID, SupersededID: earlier.ID,
		SuggestedBy: "detector:same_agent_same_ticket",
	}))

	supersedesEarlier := earlier.ID
	_, _, err := db.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: "agent-A",
		OrgID:   orgID,
		Decision: model.Decision{
			DecisionType: "implementation",
			Outcome:      "agent A explicit supersession",
			Confidence:   0.9,
			SupersedesID: &supersedesEarlier,
		},
	})
	require.NoError(t, err)

	gotA, err := db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{latentA.ID})
	require.NoError(t, err)
	assert.Empty(t, gotA, "agent A's suggestion retired by their own confirm")

	gotB, err := db.ListSupersedesSuggestionsForDecisions(ctx, orgID, []uuid.UUID{latentB.ID})
	require.NoError(t, err)
	assert.Len(t, gotB, 1, "agent B's suggestion must survive — A's confirm is not B's")
}
