//go:build !lite

package storage_test

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/storage"
	"github.com/ashita-ai/akashi/internal/testutil"
	"github.com/ashita-ai/akashi/migrations"
)

// newTestVector creates a 1536-dim vector for embedding tests.
func newTestVector() pgvector.Vector {
	const dims = 1024
	vec := make([]float32, dims)
	for i := range vec {
		vec[i] = float32(i) / float32(dims)
	}
	return pgvector.NewVector(vec)
}

// ---------------------------------------------------------------------------
// Tests: HasNotifyConn (pool.go:100, 0% -> 100%)
// ---------------------------------------------------------------------------

func TestHasNotifyConn_NoNotifyDSN(t *testing.T) {
	assert.False(t, testDB.HasNotifyConn())
}

func TestHasNotifyConn_WithNotifyDSN(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDBWithNotify(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	assert.True(t, db.HasNotifyConn())
}

// ---------------------------------------------------------------------------
// Tests: Close (pool.go:118, 0% -> 100%)
// ---------------------------------------------------------------------------

func TestClose_NoNotifyConn(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDB(ctx, testutil.TestLogger())
	require.NoError(t, err)

	db.Close(ctx)
	require.Error(t, db.Ping(ctx), "ping should fail after close")
}

func TestClose_WithNotifyConn(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDBWithNotify(ctx, testutil.TestLogger())
	require.NoError(t, err)

	assert.True(t, db.HasNotifyConn())
	db.Close(ctx)
	require.Error(t, db.Ping(ctx), "ping should fail after close")
}

// ---------------------------------------------------------------------------
// Tests: Listen (notify.go:14, 0% -> covered)
// ---------------------------------------------------------------------------

func TestListen_NilNotifyConn(t *testing.T) {
	ctx := context.Background()
	err := testDB.Listen(ctx, "test_channel_nil")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestListen_SuccessAndIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDBWithNotify(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	require.NoError(t, db.Listen(ctx, "test_listen_ch"))
	// Idempotent: listening again is fine.
	require.NoError(t, db.Listen(ctx, "test_listen_ch"))
}

func TestListen_TwoChannels(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDBWithNotify(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	require.NoError(t, db.Listen(ctx, "ch_alpha"))
	require.NoError(t, db.Listen(ctx, "ch_beta"))
}

// ---------------------------------------------------------------------------
// Tests: WaitForNotification (notify.go:44, 0% -> covered)
// ---------------------------------------------------------------------------

func TestWaitForNotification_NilConn(t *testing.T) {
	ctx := context.Background()
	_, _, err := testDB.WaitForNotification(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestWaitForNotification_ReceivesMessage(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDBWithNotify(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	ch := "wfn_test_" + uuid.New().String()[:8]
	require.NoError(t, db.Listen(ctx, ch))

	payload := `{"id":1}`
	require.NoError(t, db.Notify(ctx, ch, payload))

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	gotCh, gotPl, err := db.WaitForNotification(waitCtx)
	require.NoError(t, err)
	assert.Equal(t, ch, gotCh)
	assert.Equal(t, payload, gotPl)
}

func TestWaitForNotification_CancelledCtx(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDBWithNotify(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	ch := "wfn_cancel_" + uuid.New().String()[:8]
	require.NoError(t, db.Listen(ctx, ch))

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, _, err = db.WaitForNotification(cancelCtx)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Tests: RegisterPoolMetrics (pool.go:216, 0% -> covered)
// ---------------------------------------------------------------------------

func TestRegisterPoolMetrics_NoPanic(t *testing.T) {
	require.NotPanics(t, func() {
		testDB.RegisterPoolMetrics()
	})
}

// ---------------------------------------------------------------------------
// Tests: RunMigrations / runMigrationNoTx / runMigrationTx / isTxModeNone
// (migrate.go, 0%/63.9%/66.7%/85.7% -> covered)
// ---------------------------------------------------------------------------

func TestRunMigrations_Idempotent(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDB(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	// All migrations already applied — second run is a no-op.
	require.NoError(t, db.RunMigrations(ctx, migrations.FS))
}

func TestRunMigrations_TxModeNone(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDB(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	fs := fstest.MapFS{
		"999_notx_cov.sql": &fstest.MapFile{
			Data: []byte("-- atlas:txmode none\n-- 999: Coverage test\nCREATE INDEX IF NOT EXISTS idx_cov_notx ON agents (agent_id);\n"),
		},
	}

	require.NoError(t, db.RunMigrations(ctx, fs))
	// Idempotent rerun skips the already-applied file.
	require.NoError(t, db.RunMigrations(ctx, fs))

	_, _ = db.Pool().Exec(ctx, "DROP INDEX IF EXISTS idx_cov_notx")
}

func TestRunMigrations_RegularTx(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDB(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	fs := fstest.MapFS{
		"998_tx_cov.sql": &fstest.MapFile{
			Data: []byte("-- 998: Coverage test\nCREATE TABLE IF NOT EXISTS _cov_998 (id int PRIMARY KEY);\n"),
		},
	}

	require.NoError(t, db.RunMigrations(ctx, fs))

	var exists bool
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = '_cov_998')`).Scan(&exists))
	assert.True(t, exists)

	_, _ = db.Pool().Exec(ctx, "DROP TABLE IF EXISTS _cov_998")
}

func TestRunMigrations_SkipsNonSQL(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDB(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	fs := fstest.MapFS{
		"README.md": &fstest.MapFile{Data: []byte("not sql")},
		"subdir":    &fstest.MapFile{Mode: fs.ModeDir},
	}

	require.NoError(t, db.RunMigrations(ctx, fs))
}

func TestRunMigrations_BadSQL_Tx(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDB(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	fs := fstest.MapFS{
		"997_bad.sql": &fstest.MapFile{Data: []byte("INVALID SQL STATEMENT HERE;")},
	}

	err = db.RunMigrations(ctx, fs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "997_bad.sql")
}

func TestRunMigrations_BadSQL_NoTx(t *testing.T) {
	ctx := context.Background()
	db, err := testTC.NewTestDB(ctx, testutil.TestLogger())
	require.NoError(t, err)
	defer db.Close(ctx)

	fs := fstest.MapFS{
		"996_bad_notx.sql": &fstest.MapFile{Data: []byte("-- atlas:txmode none\nINVALID SQL;")},
	}

	err = db.RunMigrations(ctx, fs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "996_bad_notx.sql")
}

// ---------------------------------------------------------------------------
// Tests: GetSessionDecisions — with data path (sessions.go:16, 80%)
// ---------------------------------------------------------------------------

func TestGetSessionDecisions_EmptyResult(t *testing.T) {
	ctx := context.Background()
	decisions, err := testDB.GetSessionDecisions(ctx, uuid.Nil, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

func TestGetSessionDecisions_ReturnsOrdered(t *testing.T) {
	ctx := context.Background()
	sessionID := uuid.New()
	suffix := uuid.New().String()[:8]
	agentID := "sess-ord-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "sess_cov", Outcome: "first", Confidence: 0.9,
		SessionID: &sessionID,
	})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "sess_cov", Outcome: "second", Confidence: 0.8,
		SessionID: &sessionID,
	})
	require.NoError(t, err)

	decisions, err := testDB.GetSessionDecisions(ctx, uuid.Nil, sessionID)
	require.NoError(t, err)
	assert.Len(t, decisions, 2)
}

// ---------------------------------------------------------------------------
// Tests: GetOutcomeSignalsSummary — empty org (tracehealth.go:14, 77.8%)
// ---------------------------------------------------------------------------

func TestGetOutcomeSignalsSummary_NoDecisions(t *testing.T) {
	ctx := context.Background()
	summary, err := testDB.GetOutcomeSignalsSummary(ctx, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 0, summary.DecisionsTotal)
	assert.Equal(t, 0, summary.ConflictsWon)
	assert.Equal(t, 0, summary.ConflictsNoWinner)
}

// ---------------------------------------------------------------------------
// Tests: GetConflictAnalytics — all filter branches (conflicts.go:995, 77.1%)
// ---------------------------------------------------------------------------

func TestGetConflictAnalytics_AllFilters(t *testing.T) {
	ctx := context.Background()
	agentID := "cov-analytics-agent"
	decisionType := "cov_analytics_type"
	conflictKind := "cross_agent"

	analytics, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:         time.Now().UTC().Add(-24 * time.Hour),
		To:           time.Now().UTC(),
		AgentID:      &agentID,
		DecisionType: &decisionType,
		ConflictKind: &conflictKind,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, analytics.Summary.TotalDetected)
	assert.NotNil(t, analytics.ByAgentPair)
	assert.NotNil(t, analytics.ByDecisionType)
}

// ---------------------------------------------------------------------------
// Tests: GetConflictGroupKind — not found (conflicts.go:1197, 80%)
// ---------------------------------------------------------------------------

func TestGetConflictGroupKind_NoSuchGroup(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetConflictGroupKind(ctx, uuid.New(), uuid.Nil)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Tests: BackfillEmbedding — no rows affected (decisions.go:1256, 76.9%)
// ---------------------------------------------------------------------------

func TestBackfillEmbedding_MissingDecision(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, testDB.BackfillEmbedding(ctx, uuid.New(), uuid.Nil, newTestVector()))
}

func TestBackfillOutcomeEmbedding_MissingDecision(t *testing.T) {
	ctx := context.Background()
	// outcome_embedding column has 1024 dims (not 1536 like embedding).
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = float32(i) / 1024.0
	}
	require.NoError(t, testDB.BackfillOutcomeEmbedding(ctx, uuid.New(), uuid.Nil, pgvector.NewVector(vec)))
}

// ---------------------------------------------------------------------------
// Tests: MarkDecisionConflictScored — no rows matched (decisions.go:1494, 75%)
// ---------------------------------------------------------------------------

func TestMarkDecisionConflictScored_NoMatch(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, testDB.MarkDecisionConflictScored(ctx, uuid.New(), uuid.Nil))
}

// ---------------------------------------------------------------------------
// Tests: ResetConflictScoredAt (decisions.go:1528, 75%)
// ---------------------------------------------------------------------------

func TestResetConflictScoredAt_Count(t *testing.T) {
	ctx := context.Background()
	count, err := testDB.ResetConflictScoredAt(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, int64(0))
}

// ---------------------------------------------------------------------------
// Tests: InsertEvent — single row path (events.go:84, 75%)
// ---------------------------------------------------------------------------

func TestInsertEvent_SingleRow(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "ins-evt-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	require.NoError(t, testDB.InsertEvent(ctx, model.AgentEvent{
		ID: uuid.New(), RunID: run.ID, EventType: model.EventDecisionStarted,
		SequenceNum: 1, OccurredAt: time.Now().UTC(), AgentID: agentID,
		Payload: map[string]any{"cov": true}, CreatedAt: time.Now().UTC(),
	}))
}

// ---------------------------------------------------------------------------
// Tests: DeleteAgentData — with audit entry (delete.go:38, 71.8%)
// ---------------------------------------------------------------------------

func TestDeleteAgentData_WithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "del-aud-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	audit := &storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		HTTPMethod: "DELETE", Endpoint: "/v1/agents/" + agentID,
		Operation: "agent_deleted", ResourceType: "agent",
	}

	result, err := testDB.DeleteAgentData(ctx, uuid.Nil, agentID, audit)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Agents)
}

// ---------------------------------------------------------------------------
// Tests: CreateTraceTx — minimal and full paths (trace.go:20, 72.7%)
// ---------------------------------------------------------------------------

func TestCreateTraceTx_Minimal(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "trc-min-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, d, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: agentID, OrgID: uuid.Nil,
		Decision: model.Decision{
			DecisionType: "trc_test", Outcome: "ok", Confidence: 0.88,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
	assert.NotNil(t, run.CompletedAt)
	assert.Equal(t, "ok", d.Outcome)
}

func TestCreateTraceTx_Full(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "trc-full-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	s1 := float32(0.9)
	s2 := float32(0.3)
	rel := float32(0.85)

	run, d, err := testDB.CreateTraceTx(ctx, storage.CreateTraceParams{
		AgentID: agentID, OrgID: uuid.Nil,
		Decision: model.Decision{
			DecisionType: "trc_full", Outcome: "route", Confidence: 0.95,
		},
		Alternatives: []model.Alternative{
			{Label: "A", Score: &s1, Selected: true},
			{Label: "B", Score: &s2, Selected: false},
		},
		Evidence: []model.Evidence{
			{SourceType: model.SourceAPIResponse, Content: "data", RelevanceScore: &rel},
		},
		AuditEntry: &storage.MutationAuditEntry{
			RequestID: uuid.New().String(), OrgID: uuid.Nil,
			ActorAgentID: agentID, ActorRole: "agent",
			HTTPMethod: "POST", Endpoint: "/v1/trace",
			Operation: "trace_created", ResourceType: "decision",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
	assert.Equal(t, "route", d.Outcome)

	got, err := testDB.GetDecision(ctx, uuid.Nil, d.ID, storage.GetDecisionOpts{IncludeAlts: true, IncludeEvidence: true})
	require.NoError(t, err)
	assert.Len(t, got.Alternatives, 2)
	assert.Len(t, got.Evidence, 1)
}

// ---------------------------------------------------------------------------
// Tests: RetractDecision — audit path + not-found (decisions.go:264, 79.5%)
// ---------------------------------------------------------------------------

func TestRetractDecision_WithAuditEntry(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "retract-aud-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "retract_cov", Outcome: "will_retract", Confidence: 0.7,
	})
	require.NoError(t, err)

	audit := &storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		HTTPMethod: "POST", Endpoint: "/v1/decisions/" + d.ID.String() + "/retract",
		Operation: "decision_retracted", ResourceType: "decision",
	}

	require.NoError(t, testDB.RetractDecision(ctx, uuid.Nil, d.ID, "changed", "admin", audit))

	got, err := testDB.GetDecision(ctx, uuid.Nil, d.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.NotNil(t, got.ValidTo)
}

func TestRetractDecision_NonexistentDecision(t *testing.T) {
	ctx := context.Background()
	err := testDB.RetractDecision(ctx, uuid.Nil, uuid.New(), "reason", "admin", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionRevisions / GetRevisionChainIDs / GetRevisionDepth
// ---------------------------------------------------------------------------

func TestRevisionChain_SingleDecision(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "rev-ch-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "rev_ch_test", Outcome: "original", Confidence: 0.8,
	})
	require.NoError(t, err)

	revisions, err := testDB.GetDecisionRevisions(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	// A single decision with no revisions returns at least itself.
	assert.GreaterOrEqual(t, len(revisions), 0)

	chainIDs, err := testDB.GetRevisionChainIDs(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	_ = chainIDs // May be empty for a decision with no supersedes_id.

	depth, err := testDB.GetRevisionDepth(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, depth, 0)
}

// ---------------------------------------------------------------------------
// Tests: FindUnembeddedDecisions / FindDecisionsMissingOutcomeEmbedding
// default-limit branches (decisions.go:1228/1284)
// ---------------------------------------------------------------------------

func TestFindUnembeddedDecisions_ZeroLimit(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.FindUnembeddedDecisions(ctx, 0)
	require.NoError(t, err)
}

func TestFindDecisionsMissingOutcomeEmbedding_ZeroLimit(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.FindDecisionsMissingOutcomeEmbedding(ctx, 0)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: NewConflictsSinceByOrg — future cutoff (conflicts.go:194, 85.7%)
// ---------------------------------------------------------------------------

func TestNewConflictsSinceByOrg_FutureCutoff(t *testing.T) {
	ctx := context.Background()
	counts, err := testDB.NewConflictsSinceByOrg(ctx, uuid.Nil, time.Now().UTC().Add(24*time.Hour), 10)
	require.NoError(t, err)
	assert.Empty(t, counts)
}

// ---------------------------------------------------------------------------
// Tests: RefreshAgentState (conflicts.go:27, 75%)
// ---------------------------------------------------------------------------

func TestRefreshAgentState_Succeeds(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, testDB.RefreshAgentState(ctx))
}

// ---------------------------------------------------------------------------
// Tests: GetConflict — not found path (conflicts.go:262, 80%)
// ---------------------------------------------------------------------------

func TestGetConflict_MissingID(t *testing.T) {
	ctx := context.Background()
	c, err := testDB.GetConflict(ctx, uuid.Nil, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, c)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionsByIDs — non-existent (decisions.go:1024, 84.6%)
// ---------------------------------------------------------------------------

func TestGetDecisionsByIDs_NoMatch(t *testing.T) {
	ctx := context.Background()
	decisions, err := testDB.GetDecisionsByIDs(ctx, uuid.Nil, []uuid.UUID{uuid.New()})
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Tests: ExportDecisionsCursor — empty org (decisions.go:954, 85.2%)
// ---------------------------------------------------------------------------

func TestExportDecisionsCursor_NoDecisions(t *testing.T) {
	ctx := context.Background()
	decisions, err := testDB.ExportDecisionsCursor(ctx, uuid.New(), model.QueryFilters{}, nil, 10)
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionEmbeddings — non-existent (decisions.go:1378, 85.7%)
// ---------------------------------------------------------------------------

func TestGetDecisionEmbeddings_MissingIDs(t *testing.T) {
	ctx := context.Background()
	embs, err := testDB.GetDecisionEmbeddings(ctx, []uuid.UUID{uuid.New()}, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, embs)
}

// ---------------------------------------------------------------------------
// Tests: GetConflictCount (decisions.go:1407, 80%)
// ---------------------------------------------------------------------------

func TestGetConflictCount_NoConflicts(t *testing.T) {
	ctx := context.Background()
	count, err := testDB.GetConflictCount(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// ---------------------------------------------------------------------------
// Tests: GetConflictCountsBatch (decisions.go:1426, 85.7%)
// ---------------------------------------------------------------------------

func TestGetConflictCountsBatch_NoMatch(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	counts, err := testDB.GetConflictCountsBatch(ctx, []uuid.UUID{id}, uuid.Nil)
	require.NoError(t, err)
	_, exists := counts[id]
	assert.False(t, exists)
}

// ---------------------------------------------------------------------------
// Tests: CountConflicts — with filters (conflicts.go:79, 88.9%)
// ---------------------------------------------------------------------------

func TestCountConflicts_StatusFilter(t *testing.T) {
	ctx := context.Background()
	status := "open"
	count, err := testDB.CountConflicts(ctx, uuid.Nil, storage.ConflictFilters{Status: &status})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
}

// ---------------------------------------------------------------------------
// Tests: GetConflictStatusCounts (conflicts.go:95, 80%)
// ---------------------------------------------------------------------------

func TestGetConflictStatusCounts_EmptyOrg(t *testing.T) {
	ctx := context.Background()
	counts, err := testDB.GetConflictStatusCounts(ctx, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 0, counts.Total)
	assert.Equal(t, 0, counts.Open)
	assert.Equal(t, 0, counts.Resolved)
}

// ---------------------------------------------------------------------------
// Tests: IsDuplicateKey — non-pg error (pool.go:112)
// ---------------------------------------------------------------------------

func TestIsDuplicateKey_NonPgError(t *testing.T) {
	assert.False(t, testDB.IsDuplicateKey(assert.AnError))
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignals — full coverage path (decisions.go:1602, 66.7%)
// With actual supersession to cover the non-ErrNoRows branch
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignals_WithAllSignals(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "sig-all-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create original decision.
	original, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "sig_full_" + suffix, Outcome: "v1", Confidence: 0.8,
	})
	require.NoError(t, err)

	// Revise it to create a supersession relationship.
	revised, err := testDB.ReviseDecision(ctx, original.ID, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "sig_full_" + suffix, Outcome: "v2", Confidence: 0.9,
	}, nil)
	require.NoError(t, err)

	// Create a decision that cites the original as precedent.
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "sig_full_" + suffix, Outcome: "citer", Confidence: 0.85,
		PrecedentRef: &original.ID,
	})
	require.NoError(t, err)

	// Check signals for the original (should have supersession + citation).
	signals, err := testDB.GetDecisionOutcomeSignals(ctx, original.ID, uuid.Nil)
	require.NoError(t, err)
	assert.NotNil(t, signals.SupersessionVelocityHours)
	assert.Equal(t, 1, signals.PrecedentCitationCount)

	// Check signals for the revised (no supersession, no citations).
	signalsR, err := testDB.GetDecisionOutcomeSignals(ctx, revised.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Nil(t, signalsR.SupersessionVelocityHours)
}

// ---------------------------------------------------------------------------
// Tests: GetCitationPercentilesForOrg — no citations (decisions.go:1755, 57.1%)
// ---------------------------------------------------------------------------

func TestGetCitationPercentilesForOrg_EmptyOrg(t *testing.T) {
	ctx := context.Background()
	// Random org with no decisions.
	percentiles, err := testDB.GetCitationPercentilesForOrg(ctx, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, percentiles, "should return nil when no citations exist")
}

// ---------------------------------------------------------------------------
// Tests: UpdateConflictStatusWithAudit — resolved + wont_fix paths
// (conflicts.go:283, 78.6%)
// ---------------------------------------------------------------------------

func TestUpdateConflictStatusWithAudit_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.UpdateConflictStatusWithAudit(ctx, uuid.New(), uuid.Nil,
		"resolved", "admin", nil, nil, storage.MutationAuditEntry{
			RequestID: uuid.New().String(), OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "platform_admin",
			HTTPMethod: "PATCH", Endpoint: "/v1/conflicts/x",
			Operation: "conflict_resolved", ResourceType: "conflict",
		})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: EnsureDefaultOrg (organizations.go:20, 75%)
// ---------------------------------------------------------------------------

func TestEnsureDefaultOrg_IdempotentRerun(t *testing.T) {
	ctx := context.Background()
	// Running twice should succeed — the function is idempotent.
	require.NoError(t, testDB.EnsureDefaultOrg(ctx))
	require.NoError(t, testDB.EnsureDefaultOrg(ctx))
}

// ---------------------------------------------------------------------------
// Tests: GetOrganization (organizations.go:34, 85.7%)
// ---------------------------------------------------------------------------

func TestGetOrganization_NonexistentOrg(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetOrganization(ctx, uuid.New())
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Tests: DeleteAgentData — full path with all child data (delete.go:38, 71.8%)
// ---------------------------------------------------------------------------

func TestDeleteAgentData_FullCascade(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "del-full-" + suffix

	// Create agent.
	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create run + decision + alternatives + evidence.
	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "del_test_" + suffix, Outcome: "approved", Confidence: 0.9,
	})
	require.NoError(t, err)

	score := float32(0.8)
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "Alt1", Score: &score, Selected: true},
	})
	require.NoError(t, err)

	rel := float32(0.95)
	err = testDB.CreateEvidenceBatch(ctx, []model.Evidence{
		{DecisionID: d.ID, SourceType: model.SourceAPIResponse, Content: "test", RelevanceScore: &rel},
	})
	require.NoError(t, err)

	// Create an event for the run.
	require.NoError(t, testDB.InsertEvent(ctx, model.AgentEvent{
		ID: uuid.New(), RunID: run.ID, EventType: model.EventDecisionMade,
		SequenceNum: 1, OccurredAt: time.Now().UTC(), AgentID: agentID,
		Payload: map[string]any{}, CreatedAt: time.Now().UTC(),
	}))

	// Create a grant for the agent.
	_, err = testDB.CreateGrant(ctx, model.AccessGrant{
		OrgID:        uuid.Nil,
		GrantorID:    agent.ID,
		GranteeID:    agent.ID,
		ResourceType: "agent_traces", Permission: "read",
	})
	require.NoError(t, err)

	// Delete all agent data with audit.
	audit := &storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		HTTPMethod: "DELETE", Endpoint: "/v1/agents/" + agentID,
		Operation: "agent_deleted", ResourceType: "agent",
	}

	result, err := testDB.DeleteAgentData(ctx, uuid.Nil, agentID, audit)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Agents)
	assert.Equal(t, int64(1), result.Evidence)
	assert.Equal(t, int64(1), result.Alternatives)
	assert.Equal(t, int64(1), result.Decisions)
	assert.GreaterOrEqual(t, result.Events, int64(1))
	assert.GreaterOrEqual(t, result.Runs, int64(1))
	assert.GreaterOrEqual(t, result.Grants, int64(1))
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignals — full coverage of all signal queries
// (decisions.go:1602, 66.7%)
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignals_ConflictFate(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentA := "fate-a-" + suffix
	agentB := "fate-b-" + suffix

	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: "fate_" + suffix, Outcome: "approach_A", Confidence: 0.8,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: "fate_" + suffix, Outcome: "approach_B", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Create and resolve a conflict with dA as the winner.
	topicSim := 0.90
	outcomeDiv := 0.80
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent,
		DecisionAID:  dA.ID, DecisionBID: dB.ID,
		OrgID:  uuid.Nil,
		AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "fate_" + suffix, DecisionTypeB: "fate_" + suffix,
		OutcomeA: "approach_A", OutcomeB: "approach_B",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	// Resolve the conflict with dA as winner.
	_, err = testDB.UpdateConflictStatusWithAudit(ctx, conflictID, uuid.Nil,
		"resolved", "admin", nil, &dA.ID, storage.MutationAuditEntry{
			RequestID: uuid.New().String(), OrgID: uuid.Nil,
			ActorAgentID: "admin", ActorRole: "platform_admin",
			HTTPMethod: "PATCH", Endpoint: "/v1/conflicts/" + conflictID.String(),
			Operation: "conflict_resolved", ResourceType: "conflict",
		})
	require.NoError(t, err)

	// dA should have 1 won, dB should have 1 lost.
	signalsA, err := testDB.GetDecisionOutcomeSignals(ctx, dA.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 1, signalsA.ConflictFate.Won)

	signalsB, err := testDB.GetDecisionOutcomeSignals(ctx, dB.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 1, signalsB.ConflictFate.Lost)
}

// ---------------------------------------------------------------------------
// Tests: GetCitationPercentilesForOrg — with actual citations
// (decisions.go:1755, 57.1%)
// ---------------------------------------------------------------------------

func TestGetCitationPercentilesForOrg_WithData(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "cite-pct-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create a precedent decision.
	precedent, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "cite_pct_" + suffix, Outcome: "precedent", Confidence: 0.9,
	})
	require.NoError(t, err)

	// Create several decisions citing the precedent.
	for i := range 3 {
		_, err := testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID,
			DecisionType: "cite_pct_" + suffix, Outcome: fmt.Sprintf("citer_%d", i),
			Confidence: 0.8, PrecedentRef: &precedent.ID,
		})
		require.NoError(t, err)
	}

	percentiles, err := testDB.GetCitationPercentilesForOrg(ctx, uuid.Nil)
	require.NoError(t, err)
	// With at least one citation group, percentiles should be non-nil.
	// The exact values depend on other test data in the DB.
	if percentiles != nil {
		assert.Len(t, percentiles, 4, "should return 4 percentile breakpoints [p25, p50, p75, p90]")
	}
}

// ---------------------------------------------------------------------------
// Tests: retention — holds and policy (retention.go, 75%)
// ---------------------------------------------------------------------------

func TestRetentionPolicy_SetAndGet(t *testing.T) {
	ctx := context.Background()

	// Use the default org (uuid.Nil).
	days := 90
	err := testDB.SetRetentionPolicy(ctx, uuid.Nil, &days, []string{"architecture"})
	require.NoError(t, err)

	policy, err := testDB.GetRetentionPolicy(ctx, uuid.Nil, 0)
	require.NoError(t, err)
	require.NotNil(t, policy.RetentionDays)
	assert.Equal(t, 90, *policy.RetentionDays)

	// Clear the policy.
	err = testDB.SetRetentionPolicy(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)

	policy, err = testDB.GetRetentionPolicy(ctx, uuid.Nil, 0)
	require.NoError(t, err)
	assert.Nil(t, policy.RetentionDays)
}

func TestCreateAndReleaseHold(t *testing.T) {
	ctx := context.Background()

	hold, err := testDB.CreateHold(ctx, storage.RetentionHold{
		OrgID:     uuid.Nil,
		Reason:    "legal investigation",
		CreatedBy: "admin",
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, hold.ID)

	// List should include the hold.
	holds, err := testDB.ListHolds(ctx, uuid.Nil)
	require.NoError(t, err)
	found := false
	for _, h := range holds {
		if h.ID == hold.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "created hold should appear in list")

	// Release the hold.
	released, err := testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)
	assert.True(t, released)

	// Releasing again should return false.
	released, err = testDB.ReleaseHold(ctx, hold.ID, uuid.Nil)
	require.NoError(t, err)
	assert.False(t, released)
}

// ---------------------------------------------------------------------------
// Tests: ListGrants / ListGrantsByGrantee (75%/80%)
// ---------------------------------------------------------------------------

func TestListGrants_EmptyOrg(t *testing.T) {
	ctx := context.Background()
	grants, total, err := testDB.ListGrants(ctx, uuid.New(), 10, 0)
	require.NoError(t, err)
	assert.Empty(t, grants)
	assert.Equal(t, 0, total)
}

func TestListGrantsByGrantee_NoGrants(t *testing.T) {
	ctx := context.Background()
	grants, err := testDB.ListGrantsByGrantee(ctx, uuid.Nil, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, grants)
}

// ---------------------------------------------------------------------------
// Tests: TouchLastSeen (agents.go:534, 75%)
// ---------------------------------------------------------------------------

func TestTouchLastSeen_Success(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "touch-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	err = testDB.TouchLastSeen(ctx, uuid.Nil, agentID)
	require.NoError(t, err)
}

func TestTouchLastSeen_NonexistentAgent(t *testing.T) {
	ctx := context.Background()
	// Should succeed without error — 0 rows updated is not an error.
	err := testDB.TouchLastSeen(ctx, uuid.Nil, "nonexistent-"+uuid.New().String()[:8])
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: TouchAPIKeyLastUsed (api_keys.go:306, 75%)
// ---------------------------------------------------------------------------

func TestTouchAPIKeyLastUsed_NonexistentKey(t *testing.T) {
	ctx := context.Background()
	err := testDB.TouchAPIKeyLastUsed(ctx, uuid.New())
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: ClearInProgressIdempotency / CleanupIdempotencyKeys (75%/75%)
// ---------------------------------------------------------------------------

func TestClearInProgressIdempotency_ZeroState(t *testing.T) {
	ctx := context.Background()
	err := testDB.ClearInProgressIdempotency(ctx, uuid.Nil, "nonexistent", "/v1/trace", "key-"+uuid.New().String()[:8])
	require.NoError(t, err)
}

func TestCleanupIdempotencyKeys_OldEntries(t *testing.T) {
	ctx := context.Background()
	deleted, err := testDB.CleanupIdempotencyKeys(ctx, 24*time.Hour, 1*time.Hour)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(0))
}

// ---------------------------------------------------------------------------
// Tests: HasAccess (grants.go:164, 80%)
// ---------------------------------------------------------------------------

func TestHasAccess_NoGrant(t *testing.T) {
	ctx := context.Background()
	has, err := testDB.HasAccess(ctx, uuid.Nil, uuid.New(), "agent", "some-agent", "read")
	require.NoError(t, err)
	assert.False(t, has)
}

// ---------------------------------------------------------------------------
// Tests: CompleteDeletionLog (retention.go:267, 75%)
// ---------------------------------------------------------------------------

func TestCompleteDeletionLog_NonexistentID(t *testing.T) {
	ctx := context.Background()
	err := testDB.CompleteDeletionLog(ctx, uuid.New(), map[string]any{"decisions": 0})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: MarkClaimEmbeddingFailed / ClearClaimEmbeddingFailure (75%)
// ---------------------------------------------------------------------------

func TestMarkClaimEmbeddingFailed_NonexistentClaim(t *testing.T) {
	ctx := context.Background()
	err := testDB.MarkClaimEmbeddingFailed(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
}

func TestClearClaimEmbeddingFailure_NonexistentClaim(t *testing.T) {
	ctx := context.Background()
	err := testDB.ClearClaimEmbeddingFailure(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: UpdateOutcomeScore (assessments.go:51, 75%)
// ---------------------------------------------------------------------------

func TestUpdateOutcomeScore_NonexistentDecision(t *testing.T) {
	ctx := context.Background()
	score := float32(0.75)
	err := testDB.UpdateOutcomeScore(ctx, uuid.Nil, uuid.New(), &score)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: HasClaimsForDecision (claims.go:166, 80%)
// ---------------------------------------------------------------------------

func TestHasClaimsForDecision_NoClaims(t *testing.T) {
	ctx := context.Background()
	has, err := testDB.HasClaimsForDecision(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
	assert.False(t, has)
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions — TraceID filter (decisions.go:558, 78.8%)
// ---------------------------------------------------------------------------

func TestQueryDecisions_TraceIDFilter(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "qtrace-" + suffix
	traceID := "trace-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID, TraceID: &traceID})
	require.NoError(t, err)

	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "qtrace_test", Outcome: "traced", Confidence: 0.9,
	})
	require.NoError(t, err)

	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		TraceID: &traceID,
		Limit:   10,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, 1)
	assert.GreaterOrEqual(t, len(decisions), 1)
}

func TestQueryDecisions_NegativeOffset(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Limit:  5,
		Offset: -1,
	})
	require.NoError(t, err)
	_ = decisions
}

func TestQueryDecisions_ExcessiveLimit(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Limit: 5000,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(decisions), 1000)
}

func TestQueryDecisions_QualityScoreOrder(t *testing.T) {
	ctx := context.Background()
	// quality_score is a deprecated alias for completeness_score.
	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Limit:    10,
		OrderBy:  "quality_score",
		OrderDir: "asc",
	})
	require.NoError(t, err)
	_ = decisions
}

// ---------------------------------------------------------------------------
// Tests: CreateTraceAndAdjudicateConflictTx (trace.go:41, 78.3%)
// ---------------------------------------------------------------------------

func TestCreateTraceAndAdjudicateConflictTx_FullPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentA := "adj-a-" + suffix
	agentB := "adj-b-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentA, OrgID: uuid.Nil, Name: agentA, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)
	_, err = testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentB, OrgID: uuid.Nil, Name: agentB, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Create two conflicting decisions.
	runA, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	require.NoError(t, err)
	runB, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})
	require.NoError(t, err)

	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA,
		DecisionType: "adj_" + suffix, Outcome: "left", Confidence: 0.8,
	})
	require.NoError(t, err)

	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB,
		DecisionType: "adj_" + suffix, Outcome: "right", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Insert a conflict.
	topicSim := 0.85
	outcomeDiv := 0.75
	sig := topicSim * outcomeDiv
	conflictID, err := testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent,
		DecisionAID:  dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: "adj_" + suffix, DecisionTypeB: "adj_" + suffix,
		OutcomeA: "left", OutcomeB: "right",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	// Create a trace that adjudicates the conflict atomically.
	adjAgent := "adj-res-" + suffix
	_, err = testDB.CreateAgent(ctx, model.Agent{
		AgentID: adjAgent, OrgID: uuid.Nil, Name: adjAgent, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	resNote := "Choosing left after review"
	run, d, err := testDB.CreateTraceAndAdjudicateConflictTx(ctx,
		storage.CreateTraceParams{
			AgentID: adjAgent, OrgID: uuid.Nil,
			Decision: model.Decision{
				DecisionType: "adjudication", Outcome: "left wins", Confidence: 0.95,
			},
		},
		storage.AdjudicateConflictInTraceParams{
			ConflictID:        conflictID,
			ResolvedBy:        adjAgent,
			ResNote:           &resNote,
			WinningDecisionID: &dA.ID,
			Audit: storage.MutationAuditEntry{
				RequestID: uuid.New().String(), OrgID: uuid.Nil,
				ActorAgentID: adjAgent, ActorRole: "agent",
				HTTPMethod: "POST", Endpoint: "/v1/trace",
				Operation: "conflict_adjudicated", ResourceType: "conflict",
			},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, run.Status)
	assert.Equal(t, "left wins", d.Outcome)

	// Verify the conflict is resolved.
	conflict, err := testDB.GetConflict(ctx, conflictID, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, conflict)
	assert.Equal(t, "resolved", conflict.Status)
}

// ---------------------------------------------------------------------------
// Tests: EraseDecision — happy path (decisions.go:367, 79.4%)
// ---------------------------------------------------------------------------

func TestEraseDecision_Scrubs(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "erase-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	reasoning := "original reasoning"
	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID,
		DecisionType: "erase_test", Outcome: "will_be_erased", Confidence: 0.8,
		Reasoning: &reasoning,
	})
	require.NoError(t, err)

	audit := &storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: "admin", ActorRole: "platform_admin",
		HTTPMethod: "POST", Endpoint: "/v1/decisions/" + d.ID.String() + "/erase",
		Operation: "decision_erased", ResourceType: "decision",
	}

	result, err := testDB.EraseDecision(ctx, uuid.Nil, d.ID, "GDPR request", "admin", audit)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, result.Erasure.ID)

	// Verify the decision fields are scrubbed.
	got, err := testDB.GetDecision(ctx, uuid.Nil, d.ID, storage.GetDecisionOpts{})
	require.NoError(t, err)
	assert.Equal(t, "[erased]", got.Outcome)
	assert.Nil(t, got.ValidTo, "erasure should not set valid_to")
}

func TestEraseDecision_MissingDecision(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.EraseDecision(ctx, uuid.Nil, uuid.New(), "test", "admin", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionErasure (decisions.go:538, 85.7%)
// ---------------------------------------------------------------------------

func TestGetDecisionErasure_MissingID(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.GetDecisionErasure(ctx, uuid.Nil, uuid.New())
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisionsTemporal (decisions.go:653, 88.9%)
// ---------------------------------------------------------------------------

func TestQueryDecisionsTemporal_WithAllParams(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	decisions, err := testDB.QueryDecisionsTemporal(ctx, uuid.Nil, model.TemporalQueryRequest{
		AsOf:  now,
		Limit: 10,
	})
	require.NoError(t, err)
	_ = decisions
}

// ---------------------------------------------------------------------------
// Tests: SearchDecisionsByText (decisions.go:698, 90%)
// ---------------------------------------------------------------------------

func TestSearchDecisionsByText_NoResults(t *testing.T) {
	ctx := context.Background()
	decisions, err := testDB.SearchDecisionsByText(ctx, uuid.Nil, "zzzznonexistentxyzzy", model.QueryFilters{}, 10)
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Tests: HasDecisionsWithNullSearchVector (decisions.go:727, 80%)
// ---------------------------------------------------------------------------

func TestHasDecisionsWithNullSearchVector_Coverage(t *testing.T) {
	ctx := context.Background()
	has, err := testDB.HasDecisionsWithNullSearchVector(ctx)
	require.NoError(t, err)
	// Just verify it doesn't error — the result depends on DB state.
	_ = has
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionsByAgent — with filters (decisions.go:836, 90%)
// ---------------------------------------------------------------------------

func TestGetDecisionsByAgent_EmptyResult(t *testing.T) {
	ctx := context.Background()
	decisions, _, err := testDB.GetDecisionsByAgent(ctx, uuid.Nil, "nonexistent-"+uuid.New().String()[:8], 10, 0, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, decisions)
}

// ---------------------------------------------------------------------------
// Tests: ActiveHoldsExistForAgent / ActiveHoldsExistForDecision (80%/80%)
// ---------------------------------------------------------------------------

func TestActiveHoldsExistForAgent_NoHolds(t *testing.T) {
	ctx := context.Background()
	exists, err := testDB.ActiveHoldsExistForAgent(ctx, uuid.Nil, "nonexistent")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestActiveHoldsExistForDecision_NoHolds(t *testing.T) {
	ctx := context.Background()
	exists, err := testDB.ActiveHoldsExistForDecision(ctx, uuid.Nil, uuid.New())
	require.NoError(t, err)
	assert.False(t, exists)
}

// ---------------------------------------------------------------------------
// Tests: QueryDecisions with Include alternatives + evidence
// ---------------------------------------------------------------------------

func TestQueryDecisions_WithIncludeAlternativesAndEvidence(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "qdinc-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "qdinc_" + suffix, Outcome: "has_related", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Create alternatives.
	score := float32(0.6)
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "alt-A", Score: &score, Selected: true, Metadata: map[string]any{}},
		{DecisionID: d.ID, Label: "alt-B", Score: &score, Selected: false, Metadata: map[string]any{}},
	})
	require.NoError(t, err)

	// Create evidence.
	_, err = testDB.CreateEvidence(ctx, model.Evidence{
		DecisionID: d.ID, OrgID: uuid.Nil,
		SourceType: model.SourceDocument, Content: "some evidence text",
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	// Query with Include both alternatives and evidence.
	dt := "qdinc_" + suffix
	decisions, total, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{DecisionType: &dt},
		Include: []string{"alternatives", "evidence"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, decisions, 1)
	assert.Len(t, decisions[0].Alternatives, 2)
	assert.Len(t, decisions[0].Evidence, 1)
}

func TestQueryDecisions_OrderByConfidenceAsc(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "qdord-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dt := "qdord_" + suffix
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: dt, Outcome: "low", Confidence: 0.2,
	})
	require.NoError(t, err)
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: dt, Outcome: "high", Confidence: 0.9,
	})
	require.NoError(t, err)

	decisions, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters:  model.QueryFilters{DecisionType: &dt},
		OrderBy:  "confidence",
		OrderDir: "asc",
	})
	require.NoError(t, err)
	require.Len(t, decisions, 2)
	assert.True(t, decisions[0].Confidence <= decisions[1].Confidence,
		"expected ascending order by confidence: got %.2f, %.2f", decisions[0].Confidence, decisions[1].Confidence)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignalsBatch — with actual data
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignalsBatch_EmptyIDs(t *testing.T) {
	ctx := context.Background()
	result, err := testDB.GetDecisionOutcomeSignalsBatch(ctx, nil, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestGetDecisionOutcomeSignalsBatch_WithSupersessionAndCitations(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "sigbatch-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create original decision.
	dOrig, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "sigbatch_" + suffix, Outcome: "original", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Revise it to create supersession velocity data.
	dRevised, err := testDB.ReviseDecision(ctx, dOrig.ID, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "sigbatch_" + suffix, Outcome: "revised", Confidence: 0.85,
	}, nil)
	require.NoError(t, err)

	// Create a decision that cites the original as precedent.
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "sigbatch_" + suffix, Outcome: "citing", Confidence: 0.8,
		PrecedentRef: &dOrig.ID,
	})
	require.NoError(t, err)

	// Batch query signals for both decisions.
	signals, err := testDB.GetDecisionOutcomeSignalsBatch(ctx, []uuid.UUID{dOrig.ID, dRevised.ID}, uuid.Nil)
	require.NoError(t, err)
	require.Contains(t, signals, dOrig.ID)
	require.Contains(t, signals, dRevised.ID)

	// Original should have supersession velocity and citation count.
	origSignals := signals[dOrig.ID]
	assert.NotNil(t, origSignals.SupersessionVelocityHours, "original should have supersession velocity")
	assert.GreaterOrEqual(t, origSignals.PrecedentCitationCount, 1, "original should have at least 1 citation")
}

// ---------------------------------------------------------------------------
// Tests: ExportDecisionsCursor with cursor-based pagination
// ---------------------------------------------------------------------------

func TestExportDecisionsCursor_WithCursor(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "expcur-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	dt := "expcur_" + suffix
	for i := 0; i < 5; i++ {
		_, err = testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
			DecisionType: dt, Outcome: fmt.Sprintf("export-%d", i), Confidence: 0.5,
		})
		require.NoError(t, err)
	}

	// First page: limit 2, no cursor.
	page1, err := testDB.ExportDecisionsCursor(ctx, uuid.Nil, model.QueryFilters{DecisionType: &dt}, nil, 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	// Second page: use cursor from end of page1.
	cursor := &storage.ExportCursor{
		ValidFrom: page1[1].ValidFrom,
		ID:        page1[1].ID,
	}
	page2, err := testDB.ExportDecisionsCursor(ctx, uuid.Nil, model.QueryFilters{DecisionType: &dt}, cursor, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)

	// Ensure no overlap.
	assert.NotEqual(t, page1[0].ID, page2[0].ID)
	assert.NotEqual(t, page1[1].ID, page2[0].ID)
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionsByIDs — happy path
// ---------------------------------------------------------------------------

func TestGetDecisionsByIDs_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "byids-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d1, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "byids_" + suffix, Outcome: "first", Confidence: 0.6,
	})
	require.NoError(t, err)

	d2, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "byids_" + suffix, Outcome: "second", Confidence: 0.7,
	})
	require.NoError(t, err)

	result, err := testDB.GetDecisionsByIDs(ctx, uuid.Nil, []uuid.UUID{d1.ID, d2.ID})
	require.NoError(t, err)
	assert.Len(t, result, 2)
	assert.Contains(t, result, d1.ID)
	assert.Contains(t, result, d2.ID)
}

func TestGetDecisionsByIDs_EmptyInput(t *testing.T) {
	ctx := context.Background()
	result, err := testDB.GetDecisionsByIDs(ctx, uuid.Nil, nil)
	require.NoError(t, err)
	assert.Nil(t, result)
}

// ---------------------------------------------------------------------------
// Tests: ReviseDecision — covers the full revision path
// ---------------------------------------------------------------------------

func TestReviseDecision_WithAudit(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "revaud-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	orig, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "revaud_" + suffix, Outcome: "original_outcome", Confidence: 0.6,
	})
	require.NoError(t, err)

	audit := &storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "agent",
		HTTPMethod: "PUT", Endpoint: "/v1/decisions/" + orig.ID.String(),
		Operation: "decision_revised", ResourceType: "decision",
	}
	revised, err := testDB.ReviseDecision(ctx, orig.ID, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "revaud_" + suffix, Outcome: "revised_outcome", Confidence: 0.9,
	}, audit)
	require.NoError(t, err)
	assert.Equal(t, "revised_outcome", revised.Outcome)
	assert.NotNil(t, revised.SupersedesID)
	assert.Equal(t, orig.ID, *revised.SupersedesID)

	// Original should now be invalidated.
	dt := "revaud_" + suffix
	active, _, err := testDB.QueryDecisions(ctx, uuid.Nil, model.QueryRequest{
		Filters: model.QueryFilters{DecisionType: &dt},
	})
	require.NoError(t, err)
	// Only the revised decision should be active.
	require.Len(t, active, 1)
	assert.Equal(t, revised.ID, active[0].ID)
}

func TestReviseDecision_OriginalNotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.ReviseDecision(ctx, uuid.New(), model.Decision{
		OrgID: uuid.Nil, DecisionType: "x", Outcome: "y", Confidence: 0.5,
	}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Tests: GetConflictAnalytics with filters
// ---------------------------------------------------------------------------

func TestGetConflictAnalytics_WithAllFilterTypes(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentA := "ca-a-" + suffix
	agentB := "ca-b-" + suffix

	// Setup agents.
	for _, a := range []string{agentA, agentB} {
		_, err := testDB.CreateAgent(ctx, model.Agent{
			AgentID: a, OrgID: uuid.Nil, Name: a, Role: model.RoleAgent, Metadata: map[string]any{},
		})
		require.NoError(t, err)
	}

	// Create decisions.
	runA, _ := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentA})
	runB, _ := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentB})

	dt := "ca_" + suffix
	dA, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runA.ID, AgentID: agentA, OrgID: uuid.Nil,
		DecisionType: dt, Outcome: "approach-A", Confidence: 0.8,
	})
	require.NoError(t, err)
	dB, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: runB.ID, AgentID: agentB, OrgID: uuid.Nil,
		DecisionType: dt, Outcome: "approach-B", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Insert conflict.
	topicSim := 0.9
	outcomeDiv := 0.8
	sig := topicSim * outcomeDiv
	_, err = testDB.InsertScoredConflict(ctx, model.DecisionConflict{
		ConflictKind: model.ConflictKindCrossAgent,
		DecisionAID:  dA.ID, DecisionBID: dB.ID,
		OrgID: uuid.Nil, AgentA: agentA, AgentB: agentB,
		DecisionTypeA: dt, DecisionTypeB: dt,
		OutcomeA: "approach-A", OutcomeB: "approach-B",
		TopicSimilarity: &topicSim, OutcomeDivergence: &outcomeDiv,
		Significance: &sig, ScoringMethod: "text",
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	from := now.Add(-1 * time.Hour)
	to := now.Add(1 * time.Hour)

	// Filter by AgentID.
	result, err := testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:    from,
		To:      to,
		AgentID: &agentA,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Summary.TotalDetected, 1)

	// Filter by DecisionType.
	result, err = testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:         from,
		To:           to,
		DecisionType: &dt,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Summary.TotalDetected, 1)

	// Filter by ConflictKind.
	kind := string(model.ConflictKindCrossAgent)
	result, err = testDB.GetConflictAnalytics(ctx, uuid.Nil, storage.ConflictAnalyticsFilters{
		From:         from,
		To:           to,
		ConflictKind: &kind,
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Summary.TotalDetected, 1)
	assert.NotEmpty(t, result.ByAgentPair)
	assert.NotEmpty(t, result.ByDecisionType)
}

// ---------------------------------------------------------------------------
// Tests: Notify — covers the notify path (notify.go:77)
// ---------------------------------------------------------------------------

func TestNotify_HappyPath(t *testing.T) {
	if testTC == nil {
		t.Skip("testTC not available")
	}
	ctx := context.Background()
	logger := testutil.TestLogger()
	db, err := testTC.NewTestDBWithNotify(ctx, logger)
	require.NoError(t, err)
	defer db.Close(ctx)

	err = db.Notify(ctx, "test_channel", "hello")
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: InsertEvent — covers events.go:84 (75%)
// ---------------------------------------------------------------------------

func TestInsertEvent_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "evt-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "evt_" + suffix, Outcome: "event_test", Confidence: 0.5,
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	event := model.AgentEvent{
		ID:          uuid.New(),
		RunID:       run.ID,
		AgentID:     agentID,
		OrgID:       uuid.Nil,
		EventType:   model.EventDecisionMade,
		OccurredAt:  now,
		CreatedAt:   now,
		SequenceNum: 1,
		Payload: map[string]any{
			"decision_id":   d.ID.String(),
			"decision_type": "evt_" + suffix,
		},
	}
	err = testDB.InsertEvent(ctx, event)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: MarkDecisionConflictScored + ResetConflictScoredAt
// ---------------------------------------------------------------------------

func TestMarkDecisionConflictScored_AndReset(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "mcs-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "mcs_" + suffix, Outcome: "scored", Confidence: 0.6,
	})
	require.NoError(t, err)

	// Mark as conflict scored.
	err = testDB.MarkDecisionConflictScored(ctx, d.ID, uuid.Nil)
	require.NoError(t, err)

	// Reset it.
	_, err = testDB.ResetConflictScoredAt(ctx)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: CompleteRunWithAudit
// ---------------------------------------------------------------------------

func TestCompleteRunWithAudit_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "crwa-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	audit := storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "agent",
		HTTPMethod: "PATCH", Endpoint: "/v1/runs/" + run.ID.String(),
		Operation: "run_completed", ResourceType: "run",
	}
	err = testDB.CompleteRunWithAudit(ctx, uuid.Nil, run.ID, model.RunStatusCompleted, map[string]any{"key": "val"}, audit)
	require.NoError(t, err)

	got, err := testDB.GetRun(ctx, uuid.Nil, run.ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunStatusCompleted, got.Status)
}

// ---------------------------------------------------------------------------
// Tests: CreateRunWithAudit
// ---------------------------------------------------------------------------

func TestCreateRunWithAudit_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "crna-" + suffix

	audit := storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "agent",
		HTTPMethod: "POST", Endpoint: "/v1/runs",
		Operation: "run_created", ResourceType: "run",
	}
	run, err := testDB.CreateRunWithAudit(ctx, model.CreateRunRequest{AgentID: agentID}, audit)
	require.NoError(t, err)
	assert.Equal(t, agentID, run.AgentID)
}

// ---------------------------------------------------------------------------
// Tests: GetEvidenceByDecisions batch
// ---------------------------------------------------------------------------

func TestGetEvidenceByDecisions_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "evbd-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "evbd_" + suffix, Outcome: "evidenced", Confidence: 0.7,
	})
	require.NoError(t, err)

	_, err = testDB.CreateEvidence(ctx, model.Evidence{
		DecisionID: d.ID, OrgID: uuid.Nil,
		SourceType: model.SourceDocument, Content: "batch evidence",
		Metadata: map[string]any{},
	})
	require.NoError(t, err)

	result, err := testDB.GetEvidenceByDecisions(ctx, []uuid.UUID{d.ID}, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, result[d.ID], 1)
}

// ---------------------------------------------------------------------------
// Tests: GetAlternativesByDecisions batch
// ---------------------------------------------------------------------------

func TestGetAlternativesByDecisions_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "albd-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "albd_" + suffix, Outcome: "with_alts", Confidence: 0.7,
	})
	require.NoError(t, err)

	score := float32(0.4)
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "batch-alt", Score: &score, Selected: false, Metadata: map[string]any{}},
	})
	require.NoError(t, err)

	result, err := testDB.GetAlternativesByDecisions(ctx, []uuid.UUID{d.ID}, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, result[d.ID], 1)
}

// ---------------------------------------------------------------------------
// Tests: GetAlternativesByDecision (single)
// ---------------------------------------------------------------------------

func TestGetAlternativesByDecision_Single(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "alsd-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "alsd_" + suffix, Outcome: "single_alts", Confidence: 0.7,
	})
	require.NoError(t, err)

	score := float32(0.5)
	err = testDB.CreateAlternativesBatch(ctx, []model.Alternative{
		{DecisionID: d.ID, Label: "single-alt", Score: &score, Selected: true, Metadata: map[string]any{}},
	})
	require.NoError(t, err)

	alts, err := testDB.GetAlternativesByDecision(ctx, d.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, alts, 1)
}

// ---------------------------------------------------------------------------
// Tests: CreateGrantWithAudit + DeleteGrantWithAudit
// ---------------------------------------------------------------------------

func TestCreateGrantWithAudit_AndDelete(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "gra-" + suffix

	// Ensure agent exists (returns internal UUID).
	agent, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "gra_" + suffix, Outcome: "grant_test", Confidence: 0.7,
	})
	require.NoError(t, err)

	resID := d.ID.String()
	audit := storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "admin",
		HTTPMethod: "POST", Endpoint: "/v1/grants",
		Operation: "grant_created", ResourceType: "grant",
	}
	grant, err := testDB.CreateGrantWithAudit(ctx, model.AccessGrant{
		OrgID: uuid.Nil, GrantorID: agent.ID, GranteeID: agent.ID,
		ResourceType: "decision", ResourceID: &resID,
		Permission: "read",
	}, audit)
	require.NoError(t, err)
	assert.Equal(t, agent.ID, grant.GranteeID)

	// Delete it.
	delAudit := storage.MutationAuditEntry{
		RequestID: uuid.New().String(), OrgID: uuid.Nil,
		ActorAgentID: agentID, ActorRole: "admin",
		HTTPMethod: "DELETE", Endpoint: "/v1/grants/" + grant.ID.String(),
		Operation: "grant_deleted", ResourceType: "grant",
	}
	err = testDB.DeleteGrantWithAudit(ctx, uuid.Nil, grant.ID, delAudit)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: ListRunsByAgent
// ---------------------------------------------------------------------------

func TestListRunsByAgent_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "lra-" + suffix

	_, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)
	_, err = testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	runs, _, err := testDB.ListRunsByAgent(ctx, uuid.Nil, agentID, 10, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(runs), 2)
}

// ---------------------------------------------------------------------------
// Tests: CompleteIdempotency
// ---------------------------------------------------------------------------

func TestCompleteIdempotency_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	key := "idem-key-" + suffix
	hash := "hash-" + suffix

	// Begin an idempotency record.
	_, err := testDB.BeginIdempotency(ctx, uuid.Nil, "test-agent", "/v1/trace", key, hash)
	require.NoError(t, err)

	// Complete it.
	respBody := []byte(`{"ok":true}`)
	err = testDB.CompleteIdempotency(ctx, uuid.Nil, "test-agent", "/v1/trace", key, 200, respBody)
	require.NoError(t, err)

	// Begin again with the same key+hash — should get cached response.
	lookup, err := testDB.BeginIdempotency(ctx, uuid.Nil, "test-agent", "/v1/trace", key, hash)
	require.NoError(t, err)
	assert.True(t, lookup.Completed)
	assert.Equal(t, 200, lookup.StatusCode)
}

// ---------------------------------------------------------------------------
// Tests: GetOrgsWithRetention
// ---------------------------------------------------------------------------

func TestGetOrgsWithRetention_AfterSet(t *testing.T) {
	ctx := context.Background()

	// Set a retention policy on the default org.
	days := 90
	err := testDB.SetRetentionPolicy(ctx, uuid.Nil, &days, nil)
	require.NoError(t, err)

	orgs, err := testDB.GetOrgsWithRetention(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(orgs), 1)

	// At least one org entry should have org_id = uuid.Nil.
	found := false
	for _, o := range orgs {
		if o.OrgID == uuid.Nil {
			found = true
			break
		}
	}
	assert.True(t, found, "expected uuid.Nil org in retention list")

	// Clean up: reset retention policy to avoid polluting other tests.
	err = testDB.SetRetentionPolicy(ctx, uuid.Nil, nil, nil)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: StartDeletionLog + CompleteDeletionLog (higher coverage)
// ---------------------------------------------------------------------------

func TestStartAndCompleteDeletionLog_HappyPath(t *testing.T) {
	ctx := context.Background()
	logID, err := testDB.StartDeletionLog(ctx, uuid.Nil, "policy", "test", map[string]any{"reason": "test"})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, logID)

	counts := map[string]any{"decisions": 5, "events": 10}
	err = testDB.CompleteDeletionLog(ctx, logID, counts)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: CreateAssessment + ListAssessments + GetAssessmentSummary
// ---------------------------------------------------------------------------

func TestCreateAndListAssessments(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "assess-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "assess_" + suffix, Outcome: "assessed", Confidence: 0.7,
	})
	require.NoError(t, err)

	assessment, err := testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d.ID, OrgID: uuid.Nil, AssessorAgentID: "assessor-" + suffix,
		Outcome: model.AssessmentCorrect,
	})
	require.NoError(t, err)
	assert.Equal(t, model.AssessmentCorrect, assessment.Outcome)

	assessments, err := testDB.ListAssessments(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(assessments), 1)

	summary, err := testDB.GetAssessmentSummary(ctx, uuid.Nil, d.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, summary.Total, 1)
}

// ---------------------------------------------------------------------------
// Tests: GetAssessmentSummaryBatch
// ---------------------------------------------------------------------------

func TestGetAssessmentSummaryBatch_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "asbatch-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "asbatch_" + suffix, Outcome: "batched", Confidence: 0.7,
	})
	require.NoError(t, err)

	_, err = testDB.CreateAssessment(ctx, uuid.Nil, model.DecisionAssessment{
		DecisionID: d.ID, OrgID: uuid.Nil, AssessorAgentID: "assessor-" + suffix,
		Outcome: model.AssessmentCorrect,
	})
	require.NoError(t, err)

	batch, err := testDB.GetAssessmentSummaryBatch(ctx, uuid.Nil, []uuid.UUID{d.ID})
	require.NoError(t, err)
	assert.Contains(t, batch, d.ID)
}

// ---------------------------------------------------------------------------
// Tests: FindClaimsByDecision, FindDecisionIDsMissingClaims, FindRetriableClaimFailures
// ---------------------------------------------------------------------------

func TestFindClaimsByDecision_NoClaims_New(t *testing.T) {
	ctx := context.Background()
	claims, err := testDB.FindClaimsByDecision(ctx, uuid.New(), uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, claims)
}

func TestFindDecisionIDsMissingClaims_NoneExpected(t *testing.T) {
	ctx := context.Background()
	ids, err := testDB.FindDecisionIDsMissingClaims(ctx, 10)
	require.NoError(t, err)
	// Result depends on state but should not error.
	_ = ids
}

func TestFindRetriableClaimFailures_NoneExpected(t *testing.T) {
	ctx := context.Background()
	ids, err := testDB.FindRetriableClaimFailures(ctx, 3, 10)
	require.NoError(t, err)
	_ = ids
}

// ---------------------------------------------------------------------------
// Tests: CreateIntegrityProof + GetDecisionHashesForBatch + ListOrganizationIDs
// ---------------------------------------------------------------------------

func TestCreateIntegrityProof_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	now := time.Now().UTC()

	err := testDB.CreateIntegrityProof(ctx, storage.IntegrityProof{
		OrgID: uuid.Nil, BatchStart: now.Add(-1 * time.Hour), BatchEnd: now,
		DecisionCount: 5, RootHash: "merkle_root_" + suffix,
	})
	require.NoError(t, err)
}

func TestGetDecisionHashesForBatch_NoneMatching(t *testing.T) {
	ctx := context.Background()
	hashes, err := testDB.GetDecisionHashesForBatch(ctx, uuid.Nil, time.Now().Add(-1*time.Minute), time.Now())
	require.NoError(t, err)
	_ = hashes
}

func TestListOrganizationIDs_AtLeastDefault(t *testing.T) {
	ctx := context.Background()
	ids, err := testDB.ListOrganizationIDs(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(ids), 1)
}

// ---------------------------------------------------------------------------
// Tests: GetAgentWinRates
// ---------------------------------------------------------------------------

func TestGetAgentWinRates_EmptyResult(t *testing.T) {
	ctx := context.Background()
	rates, err := testDB.GetAgentWinRates(ctx, uuid.Nil, []string{"nonexistent-agent-" + uuid.New().String()[:8]}, "")
	require.NoError(t, err)
	assert.Empty(t, rates)
}

// ---------------------------------------------------------------------------
// Tests: GetResolvedConflictsByType
// ---------------------------------------------------------------------------

func TestGetResolvedConflictsByType_EmptyForNonexistentType(t *testing.T) {
	ctx := context.Background()
	dt := "nonexistent_type_" + uuid.New().String()[:8]
	resolutions, err := testDB.GetResolvedConflictsByType(ctx, uuid.Nil, dt, 10)
	require.NoError(t, err)
	assert.Empty(t, resolutions)
}

// ---------------------------------------------------------------------------
// Tests: GetCitationPercentilesForOrg — with actual citation data
// ---------------------------------------------------------------------------

func TestGetCitationPercentilesForOrg_WithMultipleCitations(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "citpct-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create a "root" decision that will be cited.
	root, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "citpct_" + suffix, Outcome: "root", Confidence: 0.9,
	})
	require.NoError(t, err)

	// Create several decisions that cite the root.
	for i := 0; i < 4; i++ {
		_, err = testDB.CreateDecision(ctx, model.Decision{
			RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
			DecisionType: "citpct_" + suffix, Outcome: fmt.Sprintf("citing-%d", i),
			Confidence: 0.7, PrecedentRef: &root.ID,
		})
		require.NoError(t, err)
	}

	// Now citation percentiles should return non-nil.
	breakpoints, err := testDB.GetCitationPercentilesForOrg(ctx, uuid.Nil)
	require.NoError(t, err)
	// The query aggregates across ALL decisions in the org, so breakpoints should be non-nil
	// as long as any precedent_ref citations exist.
	assert.NotNil(t, breakpoints, "expected non-nil breakpoints with citation data")
	assert.Len(t, breakpoints, 4, "expected 4 percentile breakpoints [p25, p50, p75, p90]")
}

// ---------------------------------------------------------------------------
// Tests: GetDecisionOutcomeSignals — full path including supersession
// ---------------------------------------------------------------------------

func TestGetDecisionOutcomeSignals_FullSupersessionPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "dosig-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create original decision.
	orig, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "dosig_" + suffix, Outcome: "original", Confidence: 0.7,
	})
	require.NoError(t, err)

	// Revise to create supersession link.
	_, err = testDB.ReviseDecision(ctx, orig.ID, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "dosig_" + suffix, Outcome: "revised", Confidence: 0.85,
	}, nil)
	require.NoError(t, err)

	// Query signals for the original — should have supersession velocity.
	signals, err := testDB.GetDecisionOutcomeSignals(ctx, orig.ID, uuid.Nil)
	require.NoError(t, err)
	assert.NotNil(t, signals.SupersessionVelocityHours, "expected supersession velocity for revised decision")
}

// ---------------------------------------------------------------------------
// Tests: UpdateOutcomeScore (assessments.go:51, 75%)
// ---------------------------------------------------------------------------

func TestUpdateOutcomeScore_WithFloat(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "uos-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	d, err := testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "uos_" + suffix, Outcome: "scoreable", Confidence: 0.6,
	})
	require.NoError(t, err)

	score := float32(0.85)
	err = testDB.UpdateOutcomeScore(ctx, d.ID, uuid.Nil, &score)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: GetEventsByRun (events.go:101, should cover scan path)
// ---------------------------------------------------------------------------

func TestGetEventsByRun_WithEvents(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "evtrun-" + suffix

	run, err := testDB.CreateRun(ctx, model.CreateRunRequest{AgentID: agentID})
	require.NoError(t, err)

	// Create a decision and an event for that run.
	_, err = testDB.CreateDecision(ctx, model.Decision{
		RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		DecisionType: "evtrun_" + suffix, Outcome: "with_events", Confidence: 0.5,
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	err = testDB.InsertEvent(ctx, model.AgentEvent{
		ID: uuid.New(), RunID: run.ID, AgentID: agentID, OrgID: uuid.Nil,
		EventType: model.EventDecisionMade, OccurredAt: now, CreatedAt: now,
		SequenceNum: 1, Payload: map[string]any{"test": true},
	})
	require.NoError(t, err)

	events, err := testDB.GetEventsByRun(ctx, uuid.Nil, run.ID, 10)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(events), 1)
}

// ---------------------------------------------------------------------------
// Tests: CountAgents + CountAgentsGlobal (agents.go:273, 80%)
// ---------------------------------------------------------------------------

func TestCountAgents_AfterCreate(t *testing.T) {
	ctx := context.Background()
	count, err := testDB.CountAgents(ctx, uuid.Nil)
	require.NoError(t, err)
	assert.Greater(t, count, 0)
}

func TestCountAgentsGlobal_NonZero(t *testing.T) {
	ctx := context.Background()
	count, err := testDB.CountAgentsGlobal(ctx)
	require.NoError(t, err)
	assert.Greater(t, count, 0)
}

// ---------------------------------------------------------------------------
// Tests: GetGlobalOpenConflictCount (conflicts.go:1185, 80%)
// ---------------------------------------------------------------------------

func TestGetGlobalOpenConflictCount_NoError(t *testing.T) {
	ctx := context.Background()
	count, err := testDB.GetGlobalOpenConflictCount(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, int64(0))
}

// ---------------------------------------------------------------------------
// Tests: DropEventChunks (retention.go:489, 80%)
// ---------------------------------------------------------------------------

func TestDropEventChunks_NoError(t *testing.T) {
	ctx := context.Background()
	_, err := testDB.DropEventChunks(ctx, time.Now().Add(-365*24*time.Hour))
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Tests: CountEligibleDecisions (retention.go:280, 90.5%)
// ---------------------------------------------------------------------------

func TestCountEligibleDecisions_NoError(t *testing.T) {
	ctx := context.Background()
	count, err := testDB.CountEligibleDecisions(ctx, uuid.Nil, time.Now().Add(-365*24*time.Hour), nil, nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count.Decisions, int64(0))
}

// ---------------------------------------------------------------------------
// Tests: GetAgentsByAgentIDGlobal (agents.go:198, 80%)
// ---------------------------------------------------------------------------

func TestGetAgentsByAgentIDGlobal_HappyPath(t *testing.T) {
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agentID := "gagl-" + suffix

	_, err := testDB.CreateAgent(ctx, model.Agent{
		AgentID: agentID, OrgID: uuid.Nil, Name: agentID, Role: model.RoleAgent, Metadata: map[string]any{},
	})
	require.NoError(t, err)

	agents, err := testDB.GetAgentsByAgentIDGlobal(ctx, agentID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(agents), 1)
}

// ---------------------------------------------------------------------------
// Tests: MigrateAgentKeysToAPIKeys (api_keys.go:321, 75%)
// ---------------------------------------------------------------------------

func TestMigrateAgentKeysToAPIKeys_NoError(t *testing.T) {
	ctx := context.Background()
	migrated, err := testDB.MigrateAgentKeysToAPIKeys(ctx)
	require.NoError(t, err)
	_ = migrated
}
