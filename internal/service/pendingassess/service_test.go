package pendingassess_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/service/pendingassess"
	"github.com/ashita-ai/akashi/internal/storage"
)

// captureStore is a storage.Store stub that records what ListPendingAssessments
// was called with so we can assert on the (decision_type, cutoff) windows the
// service builds. Every other Store method panics — the service only depends
// on ListPendingAssessments, and a misroute through an unrelated method should
// fail loudly rather than silently.
type captureStore struct {
	storage.Store // panics for unused methods via embedded nil interface

	gotOpts storage.ListPendingAssessmentsOpts
	gotOrg  uuid.UUID
	resp    []model.PendingAssessment
	err     error
}

func (c *captureStore) ListPendingAssessments(_ context.Context, orgID uuid.UUID, opts storage.ListPendingAssessmentsOpts) ([]model.PendingAssessment, error) {
	c.gotOrg = orgID
	c.gotOpts = opts
	return c.resp, c.err
}

// silence the unused-import warning for pgvector — the embedded storage.Store
// pulls it transitively but go vet wants it referenced.
var _ = pgvector.Vector{}

func TestListPending_DerivesPerTypeCutoffs(t *testing.T) {
	store := &captureStore{}
	windows := map[string]time.Duration{
		"architecture": 7 * 24 * time.Hour,
		"security":     7 * 24 * time.Hour,
		"code_review":  0, // disabled — must not appear in derived windows
	}
	svc := pendingassess.New(store, windows, 10)
	org := uuid.New()

	before := time.Now().UTC()
	_, err := svc.ListPending(context.Background(), org, pendingassess.ListInput{
		AgentIDs: []string{"agent-1"},
	})
	require.NoError(t, err)
	after := time.Now().UTC()

	assert.Equal(t, org, store.gotOrg)
	require.Len(t, store.gotOpts.Windows, 2, "code_review (window=0) must not contribute a row")

	// Each cutoff must equal now - window for its type. We don't know the
	// exact `now` the service used so we bound it to [before-window, after-window].
	expected := map[string]time.Duration{
		"architecture": 7 * 24 * time.Hour,
		"security":     7 * 24 * time.Hour,
	}
	for _, w := range store.gotOpts.Windows {
		dur, ok := expected[w.DecisionType]
		require.True(t, ok, "unexpected window for type %s", w.DecisionType)
		assert.False(t, w.Cutoff.Before(before.Add(-dur)),
			"cutoff for %s should not be earlier than before-window", w.DecisionType)
		assert.False(t, w.Cutoff.After(after.Add(-dur)),
			"cutoff for %s should not be later than after-window", w.DecisionType)
	}
}

func TestListPending_DecisionTypeNarrowsToSingleWindow(t *testing.T) {
	store := &captureStore{}
	svc := pendingassess.New(store, map[string]time.Duration{
		"architecture": 7 * 24 * time.Hour,
		"security":     7 * 24 * time.Hour,
	}, 10)

	_, err := svc.ListPending(context.Background(), uuid.New(), pendingassess.ListInput{
		DecisionType: "security",
		AgentIDs:     []string{"agent-1"},
	})
	require.NoError(t, err)
	require.Len(t, store.gotOpts.Windows, 1)
	assert.Equal(t, "security", store.gotOpts.Windows[0].DecisionType)
}

func TestListPending_UnconfiguredTypeReturnsEmptyWithoutDB(t *testing.T) {
	// A captureStore whose ListPendingAssessments would error if called.
	// We assert no DB call by checking that gotOrg stays at uuid.Nil.
	store := &captureStore{err: errors.New("DB MUST NOT BE CALLED")}
	svc := pendingassess.New(store, map[string]time.Duration{
		"architecture": 7 * 24 * time.Hour,
	}, 10)

	rows, err := svc.ListPending(context.Background(), uuid.New(), pendingassess.ListInput{
		DecisionType: "code_review", // not in windows map
	})
	require.NoError(t, err)
	assert.Nil(t, rows)
	assert.Equal(t, uuid.Nil, store.gotOrg, "service must short-circuit before storage")
}

func TestListPending_LimitDefaultsAndClamp(t *testing.T) {
	store := &captureStore{}
	svc := pendingassess.New(store, map[string]time.Duration{
		"architecture": 24 * time.Hour,
	}, 5)

	// Default fires when Limit <= 0.
	_, err := svc.ListPending(context.Background(), uuid.New(), pendingassess.ListInput{
		AgentIDs: []string{"a"},
	})
	require.NoError(t, err)
	assert.Equal(t, 5, store.gotOpts.Limit)

	// Caller-supplied limit passes through verbatim within range.
	_, err = svc.ListPending(context.Background(), uuid.New(), pendingassess.ListInput{
		Limit:    25,
		AgentIDs: []string{"a"},
	})
	require.NoError(t, err)
	assert.Equal(t, 25, store.gotOpts.Limit)

	// Limit > 100 is clamped to 100 (defense in depth — handlers already clamp).
	_, err = svc.ListPending(context.Background(), uuid.New(), pendingassess.ListInput{
		Limit:    500,
		AgentIDs: []string{"a"},
	})
	require.NoError(t, err)
	assert.Equal(t, 100, store.gotOpts.Limit)
}

func TestListPending_AllWindowsZero(t *testing.T) {
	store := &captureStore{err: errors.New("DB MUST NOT BE CALLED")}
	svc := pendingassess.New(store, map[string]time.Duration{
		"architecture": 0,
		"security":     0,
	}, 10)

	rows, err := svc.ListPending(context.Background(), uuid.New(), pendingassess.ListInput{
		AgentIDs: []string{"a"},
	})
	require.NoError(t, err)
	assert.Nil(t, rows)
}

func TestHasConfiguredType(t *testing.T) {
	svc := pendingassess.New(&captureStore{}, map[string]time.Duration{
		"architecture": 7 * 24 * time.Hour,
		"code_review":  0,
	}, 10)
	assert.True(t, svc.HasConfiguredType("architecture"))
	assert.False(t, svc.HasConfiguredType("code_review"))
	assert.False(t, svc.HasConfiguredType("unknown_type"))
}
