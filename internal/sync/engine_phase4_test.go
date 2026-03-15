package sync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// newPhase4Engine creates a minimal engine with DepGraph + ScopeGate for
// testing Phase 4 methods. Uses a real SyncStore (in-memory SQLite).
func newPhase4Engine(t *testing.T) *Engine {
	t.Helper()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)

	// Initialize Phase 4 fields.
	eng.depGraph = NewDepGraph(eng.logger)
	eng.scopeGate = NewScopeGate(eng.baseline, eng.logger)
	eng.readyCh = make(chan *TrackedAction, 1024)
	eng.trialPending = make(map[string]trialEntry)
	eng.retryTimerCh = make(chan struct{}, 1)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }

	return eng
}

// ---------------------------------------------------------------------------
// cascadeRecordAndComplete
// ---------------------------------------------------------------------------

func TestEngine_CascadeRecordAndComplete_SingleAction(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	// Add a single action to the graph.
	action := Action{
		Type:    ActionUpload,
		Path:    "test.txt",
		DriveID: driveid.New("drive1"),
	}
	ta := eng.depGraph.Add(&action, 1, nil)
	require.NotNil(t, ta, "action should be immediately ready")

	// Cascade-record it as scope-blocked.
	eng.cascadeRecordAndComplete(ctx, ta, SKQuotaOwn)

	// Verify it was completed in the graph.
	assert.Equal(t, 0, eng.depGraph.InFlightCount())

	// Verify sync_failure was recorded with scope_key.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "test.txt", failures[0].Path)
	assert.Equal(t, SKQuotaOwn, failures[0].ScopeKey)
	assert.Equal(t, int64(0), failures[0].NextRetryAt, "scope-blocked failure should have next_retry_at = 0 (NULL)")
}

func TestEngine_CascadeRecordAndComplete_WithDependents(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent action.
	parent := Action{
		Type:    ActionFolderCreate,
		Path:    "dir",
		DriveID: driveID,
	}
	parentTA := eng.depGraph.Add(&parent, 1, nil)
	require.NotNil(t, parentTA)

	// Add child that depends on parent.
	child := Action{
		Type:    ActionUpload,
		Path:    "dir/file.txt",
		DriveID: driveID,
	}
	childTA := eng.depGraph.Add(&child, 2, []int64{1})
	assert.Nil(t, childTA, "child should wait on parent")

	// Cascade-record parent → child should also be recorded.
	eng.cascadeRecordAndComplete(ctx, parentTA, SKQuotaOwn)

	// Both should be completed.
	assert.Equal(t, 0, eng.depGraph.InFlightCount())

	// Both should have sync_failures.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, failures, 2)
}

// ---------------------------------------------------------------------------
// onScopeClear
// ---------------------------------------------------------------------------

func TestEngine_OnScopeClear(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := SKQuotaOwn

	// Create a scope block.
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, sk, &ScopeBlock{
		Key:       sk,
		IssueType: IssueQuotaExceeded,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	}))

	// Create scope-blocked failures.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "a.txt", DriveID: driveID, Direction: strUpload,
		Category: strTransient, ScopeKey: sk,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "b.txt", DriveID: driveID, Direction: strUpload,
		Category: strTransient, ScopeKey: sk,
	}, nil))

	// Clear the scope.
	eng.onScopeClear(ctx, sk)

	// Scope block should be gone.
	assert.False(t, eng.scopeGate.IsScopeBlocked(sk))

	// Failures should now be retryable.
	now := eng.nowFn()
	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "scope-blocked failures should now be retryable")
}

// ---------------------------------------------------------------------------
// admitReady — scope gate checks
// ---------------------------------------------------------------------------

func TestEngine_AdmitReady_NoScopeGate(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	// nil scope gate → all actions pass through.
	eng.scopeGate = nil

	action := Action{
		Type:    ActionUpload,
		Path:    "test.txt",
		DriveID: driveid.New("drive1"),
	}
	ta := eng.depGraph.Add(&action, 1, nil)

	dispatched := eng.admitReady(ctx, []*TrackedAction{ta})
	assert.Len(t, dispatched, 1, "without scope gate, action should pass through")
}

func TestEngine_AdmitReady_ScopeBlocked(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	// Set up a scope block.
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKQuotaOwn, &ScopeBlock{
		Key:       SKQuotaOwn,
		IssueType: IssueQuotaExceeded,
		BlockedAt: eng.nowFn(),
	}))

	action := Action{
		Type:    ActionUpload,
		Path:    "test.txt",
		DriveID: eng.driveID, // own drive
	}
	ta := eng.depGraph.Add(&action, 1, nil)

	dispatched := eng.admitReady(ctx, []*TrackedAction{ta})
	assert.Empty(t, dispatched, "scope-blocked action should not be dispatched")

	// Action should be completed in graph (cascade).
	assert.Equal(t, 0, eng.depGraph.InFlightCount())

	// sync_failure should exist.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, failures, 1)
}

// ---------------------------------------------------------------------------
// processWorkerResult — success path
// ---------------------------------------------------------------------------

func TestEngine_ProcessAndRoute_Success(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent + child to DepGraph.
	parent := Action{Type: ActionUpload, Path: "parent.txt", DriveID: driveID}
	eng.depGraph.Add(&parent, 1, nil)

	child := Action{Type: ActionUpload, Path: "child.txt", DriveID: driveID}
	eng.depGraph.Add(&child, 2, []int64{1})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Simulate successful result for parent.
	r := &WorkerResult{
		Path:       "parent.txt",
		DriveID:    driveID,
		ActionType: ActionUpload,
		Success:    true,
		ActionID:   1,
	}

	dispatched := eng.processWorkerResult(ctx, r, bl)

	// Child should be returned as ready (no scope gate → dispatched).
	assert.Len(t, dispatched, 1)
	assert.Equal(t, "child.txt", dispatched[0].Action.Path)

	// Succeeded counter should increment.
	assert.Equal(t, int32(1), eng.succeeded.Load())
}

func TestEngine_ProcessAndRoute_FailureCascadesChildren(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent + child.
	parent := Action{Type: ActionFolderCreate, Path: "dir", DriveID: driveID}
	eng.depGraph.Add(&parent, 1, nil)

	child := Action{Type: ActionUpload, Path: "dir/file.txt", DriveID: driveID}
	eng.depGraph.Add(&child, 2, []int64{1})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Simulate failed result for parent.
	r := &WorkerResult{
		Path:       "dir",
		DriveID:    driveID,
		ActionType: ActionFolderCreate,
		Success:    false,
		ErrMsg:     "network error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := eng.processWorkerResult(ctx, r, bl)

	// Child should NOT be dispatched — it's cascade-recorded.
	assert.Empty(t, dispatched)

	// Both actions should be completed.
	assert.Equal(t, 0, eng.depGraph.InFlightCount())

	// Child should have a cascade sync_failure.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	// At least the child's cascade failure + parent's failure = 2.
	assert.GreaterOrEqual(t, len(failures), 2)
}

// ---------------------------------------------------------------------------
// DepGraph.Done
// ---------------------------------------------------------------------------

func TestDepGraph_DoneClosesWhenAllComplete(t *testing.T) {
	t.Parallel()
	dg := NewDepGraph(testLogger(t))

	action1 := Action{Type: ActionUpload, Path: "a.txt"}
	action2 := Action{Type: ActionUpload, Path: "b.txt"}

	dg.Add(&action1, 1, nil)
	dg.Add(&action2, 2, nil)

	// Done should not be closed yet.
	select {
	case <-dg.Done():
		t.Fatal("Done should not be closed before all actions complete")
	default:
	}

	dg.Complete(1)

	// Still not done.
	select {
	case <-dg.Done():
		t.Fatal("Done should not be closed with 1 action remaining")
	default:
	}

	dg.Complete(2)

	// Now it should be closed.
	select {
	case <-dg.Done():
		// expected
	case <-time.After(time.Second):
		t.Fatal("Done should be closed when all actions are complete")
	}
}

// ---------------------------------------------------------------------------
// synthesizeFailureEvent
// ---------------------------------------------------------------------------

func TestSynthesizeFailureEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		direction     string
		wantSource    ChangeSource
		wantType      ChangeType
		wantItemID    bool
		wantIsDeleted bool
	}{
		{
			name:       "upload → SourceLocal ChangeModify",
			direction:  strUpload,
			wantSource: SourceLocal,
			wantType:   ChangeModify,
		},
		{
			name:       "download → SourceRemote ChangeModify",
			direction:  strDownload,
			wantSource: SourceRemote,
			wantType:   ChangeModify,
			wantItemID: true,
		},
		{
			name:          "delete → SourceRemote ChangeDelete",
			direction:     strDelete,
			wantSource:    SourceRemote,
			wantType:      ChangeDelete,
			wantItemID:    true,
			wantIsDeleted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			row := &SyncFailureRow{
				Path:      "test/file.txt",
				DriveID:   driveid.New("drive1"),
				ItemID:    "item42",
				Direction: tt.direction,
			}

			ev := synthesizeFailureEvent(row)

			assert.Equal(t, tt.wantSource, ev.Source)
			assert.Equal(t, tt.wantType, ev.Type)
			assert.Equal(t, "test/file.txt", ev.Path)

			if tt.wantItemID {
				assert.Equal(t, "item42", ev.ItemID)
				assert.Equal(t, driveid.New("drive1"), ev.DriveID)
			}

			assert.Equal(t, tt.wantIsDeleted, ev.IsDeleted)
		})
	}
}

// ---------------------------------------------------------------------------
// runRetrierSweep
// ---------------------------------------------------------------------------

func TestRetrierSweep_BatchLimit(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	now := eng.nowFn()

	// Align store clock with engine clock so next_retry_at is computed
	// relative to the same fixed time.
	eng.baseline.nowFunc = eng.nowFn

	// Seed retryBatchSize + 5 sync_failures with past next_retry_at.
	// delayFn returns -1 minute so next_retry_at = now - 1m (in the past).
	for i := range retryBatchSize + 5 {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      fmt.Sprintf("file-%d.txt", i),
			DriveID:   driveID,
			Direction: strDownload,
			Category:  strTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	// Verify seeding — all rows should be retryable.
	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), retryBatchSize+5)

	eng.buf = NewBuffer(eng.logger)

	eng.runRetrierSweep(ctx)

	// Should dispatch exactly retryBatchSize items.
	assert.Equal(t, retryBatchSize, eng.buf.Len(),
		"sweep should be batch-limited to retryBatchSize")

	// retryTimerCh should have a signal for remaining items.
	select {
	case <-eng.retryTimerCh:
		// Good — re-arm signal sent.
	default:
		t.Fatal("retryTimerCh should have a signal for remaining batch items")
	}
}

func TestRetrierSweep_SkipsInFlight(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Align store clock with engine clock.
	eng.baseline.nowFunc = eng.nowFn

	// Seed 3 sync_failures.
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      name,
			DriveID:   driveID,
			Direction: strDownload,
			Category:  strTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	// Add "b.txt" to the DepGraph so it's in-flight.
	eng.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "b.txt",
		DriveID: driveID,
		ItemID:  "in-flight-item",
	}, 1, nil)

	eng.buf = NewBuffer(eng.logger)

	eng.runRetrierSweep(ctx)

	// Should dispatch 2 items (a.txt and c.txt), skipping b.txt.
	assert.Equal(t, 2, eng.buf.Len(),
		"sweep should skip in-flight items")
}

// ---------------------------------------------------------------------------
// runTrialDispatch
// ---------------------------------------------------------------------------

func TestTrialDispatch_NoCandidates_ClearsScope(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()
	now := eng.nowFn()

	// Set a scope block with NextTrialAt in the past.
	sk := SKQuotaOwn
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, sk, &ScopeBlock{
		Key:           sk,
		IssueType:     IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	}))

	// Do NOT seed any sync_failures for this scope — no candidates.
	eng.buf = NewBuffer(eng.logger)

	eng.runTrialDispatch(ctx)

	// Scope should be cleared because there are no candidates.
	assert.False(t, eng.scopeGate.IsScopeBlocked(sk),
		"scope should be cleared when no trial candidates exist")
}

func TestTrialDispatch_CleansStaleTrialPending(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()
	now := eng.nowFn()

	// Insert a stale trial entry (older than trialPendingTTL).
	eng.trialMu.Lock()
	eng.trialPending["stale.txt"] = trialEntry{
		scopeKey: SKQuotaOwn,
		created:  now.Add(-2 * trialPendingTTL),
	}
	eng.trialMu.Unlock()

	eng.buf = NewBuffer(eng.logger)

	// No due scopes needed — stale cleanup runs first in runTrialDispatch.
	eng.runTrialDispatch(ctx)

	eng.trialMu.Lock()
	remaining := len(eng.trialPending)
	eng.trialMu.Unlock()

	assert.Equal(t, 0, remaining,
		"stale trial entries should be cleaned up")
}
