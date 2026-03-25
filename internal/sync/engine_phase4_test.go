package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// newPhase4Engine creates a minimal engine with syncdispatch.DepGraph + syncdispatch.ScopeGate for
// testing Phase 4 methods. Uses a real syncstore.SyncStore (in-memory SQLite).
func newPhase4Engine(t *testing.T) *Engine {
	t.Helper()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	setupWatchEngine(t, eng)

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
	action := synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "test.txt",
		DriveID: driveid.New("drive1"),
	}
	ta := eng.depGraph.Add(&action, 1, nil)
	require.NotNil(t, ta, "action should be immediately ready")

	// Cascade-record it as scope-blocked.
	eng.cascadeRecordAndComplete(ctx, ta, synctypes.SKQuotaOwn)

	// Verify it was completed in the graph.
	assert.Equal(t, 0, eng.depGraph.InFlightCount())

	// Verify sync_failure was recorded with scope_key.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "test.txt", failures[0].Path)
	assert.Equal(t, synctypes.SKQuotaOwn, failures[0].ScopeKey)
	assert.Equal(t, int64(0), failures[0].NextRetryAt, "scope-blocked failure should have next_retry_at = 0 (NULL)")
}

func TestEngine_CascadeRecordAndComplete_WithDependents(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent action.
	parent := synctypes.Action{
		Type:    synctypes.ActionFolderCreate,
		Path:    "dir",
		DriveID: driveID,
	}
	parentTA := eng.depGraph.Add(&parent, 1, nil)
	require.NotNil(t, parentTA)

	// Add child that depends on parent.
	child := synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "dir/file.txt",
		DriveID: driveID,
	}
	childTA := eng.depGraph.Add(&child, 2, []int64{1})
	assert.Nil(t, childTA, "child should wait on parent")

	// Cascade-record parent → child should also be recorded.
	eng.cascadeRecordAndComplete(ctx, parentTA, synctypes.SKQuotaOwn)

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

// Validates: R-2.10.11
func TestEngine_OnScopeClear(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := synctypes.SKQuotaOwn

	// Create a scope block.
	require.NoError(t, eng.watch.scopeGate.SetScopeBlock(ctx, sk, &synctypes.ScopeBlock{
		Key:       sk,
		IssueType: synctypes.IssueQuotaExceeded,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	}))

	// Create scope-blocked failures.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "a.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "b.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, nil))

	// Clear the scope.
	eng.onScopeClear(ctx, sk)

	// Scope block should be gone.
	assert.False(t, eng.watch.scopeGate.IsScopeBlocked(sk))

	// Failures should now be retryable.
	now := eng.nowFn()
	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "scope-blocked failures should now be retryable")
}

// Validates: R-2.10.11
func TestEngine_OnScopeClear_SignalsImmediateRetrySweep(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()
	scopeKey := synctypes.SKQuotaOwn

	require.NoError(t, eng.watch.scopeGate.SetScopeBlock(ctx, scopeKey, &synctypes.ScopeBlock{
		Key:       scopeKey,
		IssueType: synctypes.IssueQuotaExceeded,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	}))

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "blocked.txt",
		DriveID:   driveid.New("drive1"),
		Direction: synctypes.DirectionUpload,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  scopeKey,
		ErrMsg:    "scope blocked",
	}, nil))

	eng.onScopeClear(ctx, scopeKey)

	select {
	case <-eng.watch.retryTimerCh:
	case <-time.After(time.Second):
		require.Fail(t, "onScopeClear should signal retryTimerCh for due-now failures")
	}
}

// ---------------------------------------------------------------------------
// admitReady — scope gate checks
// ---------------------------------------------------------------------------

func TestEngine_AdmitReady_NoScopeGate(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	// nil watch → one-shot mode, all actions pass through.
	eng.watch = nil

	action := synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "test.txt",
		DriveID: driveid.New("drive1"),
	}
	ta := eng.depGraph.Add(&action, 1, nil)

	dispatched := eng.admitReady(ctx, []*synctypes.TrackedAction{ta})
	assert.Len(t, dispatched, 1, "without scope gate, action should pass through")
}

func TestEngine_AdmitReady_ScopeBlocked(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	// Set up a scope block.
	require.NoError(t, eng.watch.scopeGate.SetScopeBlock(ctx, synctypes.SKQuotaOwn, &synctypes.ScopeBlock{
		Key:       synctypes.SKQuotaOwn,
		IssueType: synctypes.IssueQuotaExceeded,
		BlockedAt: eng.nowFn(),
	}))

	action := synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "test.txt",
		DriveID: eng.driveID, // own drive
	}
	ta := eng.depGraph.Add(&action, 1, nil)

	dispatched := eng.admitReady(ctx, []*synctypes.TrackedAction{ta})
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

	// Add parent + child to syncdispatch.DepGraph.
	parent := synctypes.Action{Type: synctypes.ActionUpload, Path: "parent.txt", DriveID: driveID}
	eng.depGraph.Add(&parent, 1, nil)

	child := synctypes.Action{Type: synctypes.ActionUpload, Path: "child.txt", DriveID: driveID}
	eng.depGraph.Add(&child, 2, []int64{1})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Simulate successful result for parent.
	r := &synctypes.WorkerResult{
		Path:       "parent.txt",
		DriveID:    driveID,
		ActionType: synctypes.ActionUpload,
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
	parent := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "dir", DriveID: driveID}
	eng.depGraph.Add(&parent, 1, nil)

	child := synctypes.Action{Type: synctypes.ActionUpload, Path: "dir/file.txt", DriveID: driveID}
	eng.depGraph.Add(&child, 2, []int64{1})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Simulate failed result for parent.
	r := &synctypes.WorkerResult{
		Path:       "dir",
		DriveID:    driveID,
		ActionType: synctypes.ActionFolderCreate,
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
// Grandchild cascade tests (Fix 1: BFS prevents grandchild stranding)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestCascadeFailAndComplete_Grandchildren(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// 3-level chain: A → B → C
	a := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a", DriveID: driveID}
	eng.depGraph.Add(&a, 1, nil)

	b := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a/b", DriveID: driveID}
	eng.depGraph.Add(&b, 2, []int64{1})

	c := synctypes.Action{Type: synctypes.ActionDownload, Path: "a/b/c.txt", DriveID: driveID, ItemID: "ic"}
	eng.depGraph.Add(&c, 3, []int64{2})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Fail parent A — B and C should both be cascade-failed and completed.
	r := &synctypes.WorkerResult{
		Path:       "a",
		DriveID:    driveID,
		ActionType: synctypes.ActionFolderCreate,
		Success:    false,
		ErrMsg:     "server error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := eng.processWorkerResult(ctx, r, bl)
	assert.Empty(t, dispatched, "no actions should be dispatched on failure")

	// All 3 actions should be completed — none stranded.
	assert.Equal(t, 0, eng.depGraph.InFlightCount(),
		"grandchild must not be stranded in DepGraph")

	// B and C should both have cascade sync_failures.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	// Parent's failure + B's cascade + C's cascade = 3.
	assert.GreaterOrEqual(t, len(failures), 3)
}

// Validates: R-6.8.9
func TestCompleteSubtree_Grandchildren(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// 3-level chain: A → B → C
	a := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a", DriveID: driveID}
	eng.depGraph.Add(&a, 1, nil)

	b := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a/b", DriveID: driveID}
	eng.depGraph.Add(&b, 2, []int64{1})

	c := synctypes.Action{Type: synctypes.ActionDownload, Path: "a/b/c.txt", DriveID: driveID, ItemID: "ic"}
	eng.depGraph.Add(&c, 3, []int64{2})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Shutdown parent A — B and C should be silently completed.
	r := &synctypes.WorkerResult{
		Path:       "a",
		DriveID:    driveID,
		ActionType: synctypes.ActionFolderCreate,
		Success:    false,
		Err:        context.Canceled,
		ActionID:   1,
	}

	dispatched := eng.processWorkerResult(ctx, r, bl)
	assert.Empty(t, dispatched)

	// All 3 actions should be completed.
	assert.Equal(t, 0, eng.depGraph.InFlightCount(),
		"grandchild must not be stranded on shutdown")

	// No cascade failures should be recorded (shutdown is not a failure).
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "shutdown should not record failures")
}

// Validates: R-2.10.5
func TestCascadeFailAndComplete_Diamond(t *testing.T) {
	t.Parallel()
	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Diamond: A → B, A → C, B → D, C → D
	a := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a", DriveID: driveID}
	eng.depGraph.Add(&a, 1, nil)

	b := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a/b", DriveID: driveID}
	eng.depGraph.Add(&b, 2, []int64{1})

	c := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a/c", DriveID: driveID}
	eng.depGraph.Add(&c, 3, []int64{1})

	d := synctypes.Action{Type: synctypes.ActionDownload, Path: "a/d.txt", DriveID: driveID, ItemID: "id"}
	eng.depGraph.Add(&d, 4, []int64{2, 3})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Fail parent A — B, C, and D should all be cascade-failed.
	r := &synctypes.WorkerResult{
		Path:       "a",
		DriveID:    driveID,
		ActionType: synctypes.ActionFolderCreate,
		Success:    false,
		ErrMsg:     "server error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := eng.processWorkerResult(ctx, r, bl)
	assert.Empty(t, dispatched)

	// All 4 actions should be completed — D completed exactly once.
	assert.Equal(t, 0, eng.depGraph.InFlightCount(),
		"diamond dependency must not strand any action")
}

// ---------------------------------------------------------------------------
// syncdispatch.DepGraph.Done
// ---------------------------------------------------------------------------

func TestDepGraph_DoneClosesWhenAllComplete(t *testing.T) {
	t.Parallel()
	dg := syncdispatch.NewDepGraph(testLogger(t))

	action1 := synctypes.Action{Type: synctypes.ActionUpload, Path: "a.txt"}
	action2 := synctypes.Action{Type: synctypes.ActionUpload, Path: "b.txt"}

	dg.Add(&action1, 1, nil)
	dg.Add(&action2, 2, nil)

	// Done should not be closed yet.
	select {
	case <-dg.Done():
		require.Fail(t, "Done should not be closed before all actions complete")
	default:
	}

	dg.Complete(1)

	// Still not done.
	select {
	case <-dg.Done():
		require.Fail(t, "Done should not be closed with 1 action remaining")
	default:
	}

	dg.Complete(2)

	// Now it should be closed.
	select {
	case <-dg.Done():
		// expected
	case <-time.After(time.Second):
		require.Fail(t, "Done should be closed when all actions are complete")
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
	eng.baseline.SetNowFunc(eng.nowFn)

	total := retryBatchSize + 5

	// Seed remote_state rows so createEventFromDB can build full events.
	// Each download failure needs a corresponding remote_state row.
	obs := make([]synctypes.ObservedItem, total)
	for i := range total {
		obs[i] = synctypes.ObservedItem{
			DriveID:  driveID,
			ItemID:   fmt.Sprintf("item-%d", i),
			Path:     fmt.Sprintf("file-%d.txt", i),
			ItemType: synctypes.ItemTypeFile,
			Hash:     fmt.Sprintf("hash-%d", i),
			Size:     int64(i * 100),
		}
	}

	require.NoError(t, eng.baseline.CommitObservation(ctx, obs, "", driveID))

	// Seed retryBatchSize + 5 sync_failures with past next_retry_at.
	// delayFn returns -1 minute so next_retry_at = now - 1m (in the past).
	for i := range total {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      fmt.Sprintf("file-%d.txt", i),
			DriveID:   driveID,
			Direction: synctypes.DirectionDownload,
			Category:  synctypes.CategoryTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	// Verify seeding — all rows should be retryable.
	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), total)

	eng.watch.buf = syncobserve.NewBuffer(eng.logger)

	eng.runRetrierSweep(ctx)

	// Should dispatch exactly retryBatchSize items.
	assert.Equal(t, retryBatchSize, eng.watch.buf.Len(),
		"sweep should be batch-limited to retryBatchSize")

	// retryTimerCh should have a signal for remaining items.
	select {
	case <-eng.watch.retryTimerCh:
		// Good — re-arm signal sent.
	default:
		require.Fail(t, "retryTimerCh should have a signal for remaining batch items")
	}
}

func TestRetrierSweep_SkipsInFlight(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Align store clock with engine clock.
	eng.baseline.SetNowFunc(eng.nowFn)

	names := []string{"a.txt", "b.txt", "c.txt"}

	// Seed remote_state rows so createEventFromDB can build full events.
	obs := make([]synctypes.ObservedItem, len(names))
	for i, name := range names {
		obs[i] = synctypes.ObservedItem{
			DriveID:  driveID,
			ItemID:   fmt.Sprintf("item-%s", name),
			Path:     name,
			ItemType: synctypes.ItemTypeFile,
			Hash:     fmt.Sprintf("hash-%s", name),
			Size:     int64(100 * (i + 1)),
		}
	}

	require.NoError(t, eng.baseline.CommitObservation(ctx, obs, "", driveID))

	// Seed 3 sync_failures.
	for _, name := range names {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      name,
			DriveID:   driveID,
			Direction: synctypes.DirectionDownload,
			Category:  synctypes.CategoryTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	// Add "b.txt" to the syncdispatch.DepGraph so it's in-flight.
	eng.depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "b.txt",
		DriveID: driveID,
		ItemID:  "in-flight-item",
	}, 1, nil)

	eng.watch.buf = syncobserve.NewBuffer(eng.logger)

	eng.runRetrierSweep(ctx)

	// Should dispatch 2 items (a.txt and c.txt), skipping b.txt.
	assert.Equal(t, 2, eng.watch.buf.Len(),
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
	sk := synctypes.SKQuotaOwn
	require.NoError(t, eng.watch.scopeGate.SetScopeBlock(ctx, sk, &synctypes.ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	}))

	// Do NOT seed any sync_failures for this scope — no candidates.
	eng.watch.buf = syncobserve.NewBuffer(eng.logger)

	eng.runTrialDispatch(ctx)

	// Scope should be cleared because there are no candidates.
	assert.False(t, eng.watch.scopeGate.IsScopeBlocked(sk),
		"scope should be cleared when no trial candidates exist")
}

// ---------------------------------------------------------------------------
// GetRemoteStateByPath
// ---------------------------------------------------------------------------

func TestGetRemoteStateByPath_Found(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Insert a remote_state row via CommitObservation.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-abc",
			Path:     "docs/report.pdf",
			ParentID: "parent-1",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "xorhash-abc",
			Size:     4096,
			Mtime:    1000000000,
			ETag:     "etag-1",
		},
	}, "", driveID))

	row, err := eng.baseline.GetRemoteStateByPath(ctx, "docs/report.pdf", driveID)
	require.NoError(t, err)
	require.NotNil(t, row, "should find the row")

	assert.Equal(t, "item-abc", row.ItemID)
	assert.Equal(t, "docs/report.pdf", row.Path)
	assert.Equal(t, "parent-1", row.ParentID)
	assert.Equal(t, "xorhash-abc", row.Hash)
	assert.Equal(t, int64(4096), row.Size)
	assert.Equal(t, int64(1000000000), row.Mtime)
	assert.Equal(t, "etag-1", row.ETag)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, row.SyncStatus)
}

func TestGetRemoteStateByPath_NotFound(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row, err := eng.baseline.GetRemoteStateByPath(ctx, "nonexistent.txt", driveID)
	require.NoError(t, err)
	assert.Nil(t, row, "should return nil for missing path")
}

func TestGetRemoteStateByPath_NullableFields(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Insert with minimal fields (no hash, no size, no mtime).
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-sparse",
			Path:     "folder/",
			ItemType: synctypes.ItemTypeFolder,
		},
	}, "", driveID))

	row, err := eng.baseline.GetRemoteStateByPath(ctx, "folder/", driveID)
	require.NoError(t, err)
	require.NotNil(t, row)

	assert.Equal(t, "", row.Hash, "hash should be empty string from NULL")
	assert.Equal(t, int64(0), row.Size, "size should be 0 from NULL")
	assert.Equal(t, int64(0), row.Mtime, "mtime should be 0 from NULL")
}

// ---------------------------------------------------------------------------
// remoteStateToChangeEvent
// ---------------------------------------------------------------------------

func TestRemoteStateToChangeEvent_Download(t *testing.T) {
	t.Parallel()

	rs := &synctypes.RemoteStateRow{
		DriveID:    driveid.New("drive1"),
		ItemID:     "item-42",
		Path:       "docs/file.txt",
		ParentID:   "parent-7",
		ItemType:   synctypes.ItemTypeFile,
		Hash:       "xorhash-42",
		Size:       8192,
		Mtime:      2000000000,
		ETag:       "etag-42",
		SyncStatus: synctypes.SyncStatusPendingDownload,
	}

	ev := remoteStateToChangeEvent(rs, "docs/file.txt")

	assert.Equal(t, synctypes.SourceRemote, ev.Source)
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, "docs/file.txt", ev.Path)
	assert.Equal(t, "item-42", ev.ItemID)
	assert.Equal(t, "parent-7", ev.ParentID)
	assert.Equal(t, driveid.New("drive1"), ev.DriveID)
	assert.Equal(t, synctypes.ItemTypeFile, ev.ItemType)
	assert.Equal(t, "file.txt", ev.Name)
	assert.Equal(t, int64(8192), ev.Size)
	assert.Equal(t, "xorhash-42", ev.Hash)
	assert.Equal(t, int64(2000000000), ev.Mtime)
	assert.Equal(t, "etag-42", ev.ETag)
	assert.False(t, ev.IsDeleted)
}

func TestRemoteStateToChangeEvent_Delete(t *testing.T) {
	t.Parallel()

	// Test all delete-family statuses.
	for _, status := range []synctypes.SyncStatus{synctypes.SyncStatusDeleted, synctypes.SyncStatusDeleting, synctypes.SyncStatusDeleteFailed, synctypes.SyncStatusPendingDelete} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()

			rs := &synctypes.RemoteStateRow{
				DriveID:    driveid.New("drive1"),
				ItemID:     "item-del",
				Path:       "trash/old.txt",
				SyncStatus: status,
				ItemType:   synctypes.ItemTypeFile,
			}

			ev := remoteStateToChangeEvent(rs, "trash/old.txt")

			assert.Equal(t, synctypes.ChangeDelete, ev.Type)
			assert.True(t, ev.IsDeleted)
			assert.Equal(t, "old.txt", ev.Name)
		})
	}
}

func TestRemoteStateToChangeEvent_Folder(t *testing.T) {
	t.Parallel()

	rs := &synctypes.RemoteStateRow{
		DriveID:    driveid.New("drive1"),
		ItemID:     "item-folder",
		Path:       "photos/vacation",
		SyncStatus: synctypes.SyncStatusPendingDownload,
		ItemType:   synctypes.ItemTypeFolder,
	}

	ev := remoteStateToChangeEvent(rs, "photos/vacation")

	assert.Equal(t, synctypes.ItemTypeFolder, ev.ItemType)
	assert.Equal(t, "vacation", ev.Name)
}

// ---------------------------------------------------------------------------
// createEventFromDB
// ---------------------------------------------------------------------------

func TestCreateEventFromDB_Upload_FileExists(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Create a real file in the sync root.
	syncRoot := eng.syncRoot
	testFile := "upload-test.txt"
	require.NoError(t, os.WriteFile(
		filepath.Join(syncRoot, testFile),
		[]byte("hello world"),
		0o644,
	))

	row := &synctypes.SyncFailureRow{
		Path:      testFile,
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	}

	ev := eng.createEventFromDB(ctx, row)

	require.NotNil(t, ev, "should create event for existing file")
	assert.Equal(t, synctypes.SourceLocal, ev.Source)
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, testFile, ev.Path)
	assert.Equal(t, "upload-test.txt", ev.Name)
	assert.Equal(t, synctypes.ItemTypeFile, ev.ItemType)
	assert.Greater(t, ev.Size, int64(0), "size should be populated")
	assert.NotEmpty(t, ev.Hash, "hash should be computed")
	assert.Greater(t, ev.Mtime, int64(0), "mtime should be populated")
}

// Validates: R-2.10.7
func TestCreateEventFromDB_Upload_ReusesBaselineHashWhenMetadataMatches(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	testFile := "upload-fast-path.txt"
	actualContent := []byte("actual data")
	cachedHash := "cached-local-hash"
	oldTime := eng.nowFn().Add(-2 * time.Second)

	require.NoError(t, os.WriteFile(filepath.Join(eng.syncRoot, testFile), actualContent, 0o644))
	require.NoError(t, os.Chtimes(filepath.Join(eng.syncRoot, testFile), oldTime, oldTime))

	info, err := os.Stat(filepath.Join(eng.syncRoot, testFile))
	require.NoError(t, err)

	actualHash, err := syncobserve.ComputeStableHash(filepath.Join(eng.syncRoot, testFile))
	require.NoError(t, err)
	require.NotEqual(t, actualHash, cachedHash, "test needs a distinct cached hash to prove reuse")

	require.NoError(t, eng.baseline.CommitOutcome(ctx, &synctypes.Outcome{
		Action:     synctypes.ActionUpload,
		Success:    true,
		Path:       testFile,
		DriveID:    driveID,
		ItemID:     "upload-fast-path-item",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  cachedHash,
		RemoteHash: cachedHash,
		Size:       info.Size(),
		Mtime:      info.ModTime().UnixNano(),
	}))

	ev := eng.createEventFromDB(ctx, &synctypes.SyncFailureRow{
		Path:      testFile,
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	})

	require.NotNil(t, ev)
	assert.Equal(t, cachedHash, ev.Hash, "matching metadata outside the racily-clean window should reuse the baseline hash")
}

func TestCreateEventFromDB_Upload_FileGone(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "nonexistent-upload.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	}

	ev := eng.createEventFromDB(ctx, row)

	require.NotNil(t, ev, "should create delete event for missing file")
	assert.Equal(t, synctypes.SourceLocal, ev.Source)
	assert.Equal(t, synctypes.ChangeDelete, ev.Type)
	assert.True(t, ev.IsDeleted)
}

func TestCreateEventFromDB_Download_RemoteStateExists(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Seed remote_state.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "dl-item",
			Path:     "download-test.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "dl-hash",
			Size:     1024,
			Mtime:    5000000000,
			ETag:     "dl-etag",
		},
	}, "", driveID))

	row := &synctypes.SyncFailureRow{
		Path:      "download-test.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
	}

	ev := eng.createEventFromDB(ctx, row)

	require.NotNil(t, ev, "should create event from remote_state")
	assert.Equal(t, synctypes.SourceRemote, ev.Source)
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, "download-test.txt", ev.Path)
	assert.Equal(t, "dl-item", ev.ItemID)
	assert.Equal(t, "dl-hash", ev.Hash)
	assert.Equal(t, int64(1024), ev.Size)
	assert.Equal(t, int64(5000000000), ev.Mtime)
	assert.Equal(t, "dl-etag", ev.ETag)
	assert.Equal(t, "download-test.txt", ev.Name)
}

func TestCreateEventFromDB_Download_RemoteStateGone(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// No remote_state seeded.
	row := &synctypes.SyncFailureRow{
		Path:      "no-remote.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
	}

	ev := eng.createEventFromDB(ctx, row)

	assert.Nil(t, ev, "should return nil when no remote_state")
}

// ---------------------------------------------------------------------------
// isFailureResolved
// ---------------------------------------------------------------------------

func TestIsFailureResolved_Download_Synced(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Seed remote_state and set it to synced (simulates a download that
	// completed through the normal pipeline).
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "resolved-item",
			Path:     "resolved.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "resolved-hash",
			Size:     512,
		},
	}, "", driveID))

	_, err := eng.baseline.DB().ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE item_id = ?`,
		synctypes.SyncStatusSynced, "resolved-item",
	)
	require.NoError(t, err)

	// Seed a sync_failure for this path.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "resolved.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Category:  synctypes.CategoryTransient,
	}, nil))

	row := &synctypes.SyncFailureRow{
		Path:      "resolved.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
	}

	assert.True(t, eng.isFailureResolved(ctx, row),
		"download with synced remote_state should be resolved")

	// The sync_failure should have been cleared.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "resolved failure should be cleared from DB")
}

func TestIsFailureResolved_Download_NoRemoteState(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "deleted-remotely.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
	}

	assert.True(t, eng.isFailureResolved(ctx, row),
		"download with no remote_state should be resolved")
}

func TestIsFailureResolved_Download_StillPending(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Seed remote_state with pending_download.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "pending-item",
			Path:     "still-pending.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "pending-hash",
		},
	}, "", driveID))

	row := &synctypes.SyncFailureRow{
		Path:      "still-pending.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
	}

	assert.False(t, eng.isFailureResolved(ctx, row),
		"download with pending_download remote_state should NOT be resolved")
}

func TestIsFailureResolved_Upload_FileGone(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "gone-upload.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	}

	assert.True(t, eng.isFailureResolved(ctx, row),
		"upload for non-existent file should be resolved")
}

func TestIsFailureResolved_Upload_FileExists(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Create a real file.
	require.NoError(t, os.WriteFile(
		filepath.Join(eng.syncRoot, "still-here.txt"),
		[]byte("content"),
		0o644,
	))

	row := &synctypes.SyncFailureRow{
		Path:      "still-here.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	}

	assert.False(t, eng.isFailureResolved(ctx, row),
		"upload for existing file should NOT be resolved")
}

func TestIsFailureResolved_Delete_NoBaseline(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "already-deleted.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDelete,
	}

	assert.True(t, eng.isFailureResolved(ctx, row),
		"delete with no baseline entry should be resolved")
}

func TestIsFailureResolved_Delete_BaselineExists(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Create a baseline entry via a successful download outcome.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "baseline-item",
			Path:     "still-in-baseline.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "bl-hash",
			Size:     100,
		},
	}, "", driveID))

	require.NoError(t, eng.baseline.CommitOutcome(ctx, &synctypes.Outcome{
		Action:     synctypes.ActionDownload,
		Success:    true,
		Path:       "still-in-baseline.txt",
		DriveID:    driveID,
		ItemID:     "baseline-item",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "bl-hash",
		RemoteHash: "bl-hash",
		Size:       100,
	}))

	row := &synctypes.SyncFailureRow{
		Path:      "still-in-baseline.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDelete,
	}

	assert.False(t, eng.isFailureResolved(ctx, row),
		"delete with baseline entry should NOT be resolved")
}

// ---------------------------------------------------------------------------
// reobserve
// ---------------------------------------------------------------------------

func TestReobserve_Remote_200(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{
				ID:           "live-item",
				Name:         "live-file.txt",
				DriveID:      driveid.New("drive1"),
				ParentID:     "live-parent",
				Size:         2048,
				QuickXorHash: "live-hash",
				ETag:         "live-etag",
				ModifiedAt:   time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.depGraph = syncdispatch.NewDepGraph(eng.logger)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "live-file.txt",
		DriveID:   driveID,
		ItemID:    "live-item",
		Direction: synctypes.DirectionDownload,
	}

	ev, retryAfter := eng.reobserve(ctx, row)

	require.NotNil(t, ev)
	assert.Equal(t, time.Duration(0), retryAfter, "200 should have zero RetryAfter")
	assert.Equal(t, synctypes.SourceRemote, ev.Source)
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, "live-item", ev.ItemID)
	assert.Equal(t, "live-hash", ev.Hash)
	assert.Equal(t, int64(2048), ev.Size)
	assert.Equal(t, "live-etag", ev.ETag)
	assert.Equal(t, "live-file.txt", ev.Name)
}

func TestReobserve_Remote_404(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{
				StatusCode: 404,
				Err:        graph.ErrNotFound,
				Message:    "item not found",
			}
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.depGraph = syncdispatch.NewDepGraph(eng.logger)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "deleted-remotely.txt",
		DriveID:   driveID,
		ItemID:    "deleted-item",
		Direction: synctypes.DirectionDownload,
	}

	ev, retryAfter := eng.reobserve(ctx, row)

	require.NotNil(t, ev)
	assert.Equal(t, time.Duration(0), retryAfter, "404 should have zero RetryAfter")
	assert.Equal(t, synctypes.ChangeDelete, ev.Type)
	assert.True(t, ev.IsDeleted)
	assert.Equal(t, "deleted-item", ev.ItemID)
}

func TestReobserve_Remote_429(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{
				StatusCode: 429,
				Err:        graph.ErrThrottled,
				Message:    "too many requests",
			}
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.depGraph = syncdispatch.NewDepGraph(eng.logger)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "throttled.txt",
		DriveID:   driveID,
		ItemID:    "throttled-item",
		Direction: synctypes.DirectionDownload,
	}

	ev, retryAfter := eng.reobserve(ctx, row)

	assert.Nil(t, ev, "should return nil when scope condition persists (429)")
	assert.Equal(t, time.Duration(0), retryAfter, "429 without Retry-After header should return 0")
}

func TestReobserve_Remote_429_WithRetryAfter(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{
				StatusCode: 429,
				Err:        graph.ErrThrottled,
				Message:    "too many requests",
				RetryAfter: 90 * time.Second,
			}
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.depGraph = syncdispatch.NewDepGraph(eng.logger)
	ctx := context.Background()

	row := &synctypes.SyncFailureRow{
		Path:      "throttled-retry.txt",
		DriveID:   driveid.New("drive1"),
		ItemID:    "throttled-retry-item",
		Direction: synctypes.DirectionDownload,
	}

	ev, retryAfter := eng.reobserve(ctx, row)

	assert.Nil(t, ev, "should return nil when scope condition persists (429)")
	assert.Equal(t, 90*time.Second, retryAfter,
		"should forward RetryAfter from GraphError")
}

func TestReobserve_Remote_507_WithRetryAfter(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{
				StatusCode: 507,
				Err:        graph.ErrServerError,
				Message:    "insufficient storage",
				RetryAfter: 120 * time.Second,
			}
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.depGraph = syncdispatch.NewDepGraph(eng.logger)
	ctx := context.Background()

	row := &synctypes.SyncFailureRow{
		Path:      "storage-full.txt",
		DriveID:   driveid.New("drive1"),
		ItemID:    "storage-full-item",
		Direction: synctypes.DirectionDownload,
	}

	ev, retryAfter := eng.reobserve(ctx, row)

	assert.Nil(t, ev, "should return nil when scope condition persists (507)")
	assert.Equal(t, 120*time.Second, retryAfter,
		"should forward RetryAfter from 507 GraphError")
}

func TestReobserve_Local_Exists(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, syncRoot := newTestEngine(t, mock)
	eng.depGraph = syncdispatch.NewDepGraph(eng.logger)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Create a real file.
	require.NoError(t, os.WriteFile(
		filepath.Join(syncRoot, "local-exists.txt"),
		[]byte("local content"),
		0o644,
	))

	row := &synctypes.SyncFailureRow{
		Path:      "local-exists.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	}

	ev, retryAfter := eng.reobserve(ctx, row)

	require.NotNil(t, ev)
	assert.Equal(t, time.Duration(0), retryAfter, "local reobserve should have zero RetryAfter")
	assert.Equal(t, synctypes.SourceLocal, ev.Source)
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.NotEmpty(t, ev.Hash)
	assert.Greater(t, ev.Size, int64(0))
}

// Validates: R-2.10.7
func TestReobserve_Local_ReusesBaselineHashWhenMetadataMatches(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.depGraph = syncdispatch.NewDepGraph(eng.logger)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	testFile := "local-fast-path.txt"
	actualContent := []byte("actual data")
	cachedHash := "cached-local-hash"
	oldTime := eng.nowFn().Add(-2 * time.Second)

	require.NoError(t, os.WriteFile(filepath.Join(eng.syncRoot, testFile), actualContent, 0o644))
	require.NoError(t, os.Chtimes(filepath.Join(eng.syncRoot, testFile), oldTime, oldTime))

	info, err := os.Stat(filepath.Join(eng.syncRoot, testFile))
	require.NoError(t, err)

	actualHash, err := syncobserve.ComputeStableHash(filepath.Join(eng.syncRoot, testFile))
	require.NoError(t, err)
	require.NotEqual(t, actualHash, cachedHash, "test needs a distinct cached hash to prove reuse")

	require.NoError(t, eng.baseline.CommitOutcome(ctx, &synctypes.Outcome{
		Action:     synctypes.ActionUpload,
		Success:    true,
		Path:       testFile,
		DriveID:    driveID,
		ItemID:     "local-fast-path-item",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  cachedHash,
		RemoteHash: cachedHash,
		Size:       info.Size(),
		Mtime:      info.ModTime().UnixNano(),
	}))

	ev, retryAfter := eng.reobserve(ctx, &synctypes.SyncFailureRow{
		Path:      testFile,
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	})

	require.NotNil(t, ev)
	assert.Zero(t, retryAfter)
	assert.Equal(t, cachedHash, ev.Hash, "local reobserve should share the same safe metadata fast path as the scanner")
}

func TestReobserve_Local_Gone(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.depGraph = syncdispatch.NewDepGraph(eng.logger)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "gone-local.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	}

	ev, retryAfter := eng.reobserve(ctx, row)

	require.NotNil(t, ev)
	assert.Equal(t, time.Duration(0), retryAfter, "local gone should have zero RetryAfter")
	assert.Equal(t, synctypes.ChangeDelete, ev.Type)
	assert.True(t, ev.IsDeleted)
}

// ---------------------------------------------------------------------------
// Integration: D-9 — retrier sweep creates full-fidelity events
// ---------------------------------------------------------------------------

// Validates: R-2.10.7
func TestRetrierSweep_FullFidelityEvents_D9(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	eng.baseline.SetNowFunc(eng.nowFn)

	// Seed remote_state with full metadata.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "d9-item",
			Path:     "d9-test.txt",
			ParentID: "d9-parent",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "d9-hash",
			Size:     9999,
			Mtime:    7777777777,
			ETag:     "d9-etag",
		},
	}, "", driveID))

	// Seed a sync_failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "d9-test.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Category:  synctypes.CategoryTransient,
	}, func(_ int) time.Duration {
		return -time.Minute
	}))

	eng.watch.buf = syncobserve.NewBuffer(eng.logger)
	eng.runRetrierSweep(ctx)

	// Verify the buffer contains a full-fidelity event.
	result := eng.watch.buf.FlushImmediate()
	require.Len(t, result, 1)
	require.Len(t, result[0].RemoteEvents, 1)

	ev := result[0].RemoteEvents[0]
	assert.Equal(t, "d9-test.txt", ev.Path)
	assert.Equal(t, "d9-item", ev.ItemID)
	assert.Equal(t, "d9-hash", ev.Hash, "D-9: hash must be populated")
	assert.Equal(t, int64(9999), ev.Size, "D-9: size must be populated")
	assert.Equal(t, int64(7777777777), ev.Mtime, "D-9: mtime must be populated")
	assert.Equal(t, "d9-etag", ev.ETag, "D-9: etag must be populated")
	assert.Equal(t, "d9-test.txt", ev.Name, "D-9: name must be populated")
}

// ---------------------------------------------------------------------------
// Integration: D-11 — retrier sweep skips resolved failures
// ---------------------------------------------------------------------------

// Validates: R-2.10.7
func TestRetrierSweep_SkipsResolvedFailures_D11(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	eng.baseline.SetNowFunc(eng.nowFn)

	// Seed remote_state: d11-synced will be set to synctypes.SyncStatusSynced (resolved),
	// d11-pending stays at synctypes.SyncStatusPendingDownload (not resolved).
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "synced-item",
			Path:     "d11-synced.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "synced-hash",
			Size:     100,
		},
		{
			DriveID:  driveID,
			ItemID:   "pending-item",
			Path:     "d11-pending.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "pending-hash",
			Size:     200,
		},
	}, "", driveID))

	// Directly set d11-synced to synced status (simulates a completed download
	// through the normal pipeline). The full download lifecycle
	// (pending_download → downloading → synced) isn't needed for this test.
	_, err := eng.baseline.DB().ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE item_id = ?`,
		synctypes.SyncStatusSynced, "synced-item",
	)
	require.NoError(t, err)

	// Seed sync_failures for both.
	for _, path := range []string{"d11-synced.txt", "d11-pending.txt"} {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      path,
			DriveID:   driveID,
			Direction: synctypes.DirectionDownload,
			Category:  synctypes.CategoryTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	eng.watch.buf = syncobserve.NewBuffer(eng.logger)
	eng.runRetrierSweep(ctx)

	// Only d11-pending should be dispatched (d11-synced is resolved).
	result := eng.watch.buf.FlushImmediate()
	require.Len(t, result, 1, "D-11: resolved failure should be skipped")
	assert.Equal(t, "d11-pending.txt", result[0].Path)

	// The resolved failure should have been cleared from the DB.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)

	// Only d11-pending should remain.
	require.Len(t, failures, 1)
	assert.Equal(t, "d11-pending.txt", failures[0].Path)
}

// ---------------------------------------------------------------------------
// Integration: D-9 — trial dispatch uses reobserve
// ---------------------------------------------------------------------------

func TestTrialDispatch_UsesReobserve(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{
				ID:           "trial-item",
				Name:         "trial.txt",
				DriveID:      driveid.New("drive1"),
				ParentID:     "trial-parent",
				Size:         4096,
				QuickXorHash: "trial-hash",
				ETag:         "trial-etag",
				ModifiedAt:   time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
			}, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	setupWatchEngine(t, eng)
	eng.watch.buf = syncobserve.NewBuffer(eng.logger)

	ctx := context.Background()
	driveID := driveid.New("drive1")
	now := eng.nowFn()

	sk := synctypes.SKQuotaOwn

	// Set up a scope block with NextTrialAt in the past.
	require.NoError(t, eng.watch.scopeGate.SetScopeBlock(ctx, sk, &synctypes.ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	}))

	// Seed a scope-blocked failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "trial.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  sk,
		ItemID:    "trial-item",
	}, nil))

	// Capture the scope block's TrialInterval before dispatch.
	blockBefore, ok := eng.watch.scopeGate.GetScopeBlock(sk)
	require.True(t, ok)
	intervalBefore := blockBefore.TrialInterval

	eng.runTrialDispatch(ctx)

	// The buffer should have a full-fidelity event from reobserve (not synthesized).
	result := eng.watch.buf.FlushImmediate()
	require.Len(t, result, 1)
	require.Len(t, result[0].RemoteEvents, 1)

	ev := result[0].RemoteEvents[0]
	assert.Equal(t, "trial-hash", ev.Hash, "D-9: reobserve should populate hash")
	assert.Equal(t, int64(4096), ev.Size, "D-9: reobserve should populate size")
	assert.Equal(t, "trial-etag", ev.ETag, "D-9: reobserve should populate etag")

	// trialPending should have an entry.
	eng.watch.trialMu.Lock()
	_, hasTrial := eng.watch.trialPending["trial.txt"]
	eng.watch.trialMu.Unlock()

	assert.True(t, hasTrial, "trial should be registered in trialPending")

	// After successful dispatch, the scope block's TrialInterval should NOT
	// be extended — interval stays unmutated until the worker result arrives.
	blockAfter, ok := eng.watch.scopeGate.GetScopeBlock(sk)
	require.True(t, ok)
	assert.Equal(t, intervalBefore, blockAfter.TrialInterval,
		"trial interval should NOT be extended after successful dispatch")
}

// TestTrialDispatch_ForwardsRetryAfter verifies that when reobserve returns
// a RetryAfter duration from a 429 response, runTrialDispatch forwards it
// to extendTrialInterval, which uses the server value instead of doubling.
func TestTrialDispatch_ForwardsRetryAfter(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{
				StatusCode: 429,
				Err:        graph.ErrThrottled,
				Message:    "too many requests",
				RetryAfter: 90 * time.Second,
			}
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	setupWatchEngine(t, eng)
	eng.watch.buf = syncobserve.NewBuffer(eng.logger)

	ctx := context.Background()
	driveID := driveid.New("drive1")
	now := eng.nowFn()

	sk := synctypes.SKQuotaOwn

	// Set up a scope block with a small TrialInterval and NextTrialAt in the past.
	require.NoError(t, eng.watch.scopeGate.SetScopeBlock(ctx, sk, &synctypes.ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	}))

	// Seed a scope-blocked failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "throttled.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  sk,
		ItemID:    "throttled-item",
	}, nil))

	eng.runTrialDispatch(ctx)

	// syncobserve.Buffer should be empty — reobserve returned nil (scope persists).
	result := eng.watch.buf.FlushImmediate()
	assert.Empty(t, result, "no event should be dispatched when scope condition persists")

	// The scope block's TrialInterval should now be 90s (from server's
	// Retry-After), not 20s (doubled from 10s).
	blockAfter, ok := eng.watch.scopeGate.GetScopeBlock(sk)
	require.True(t, ok)
	assert.Equal(t, 90*time.Second, blockAfter.TrialInterval,
		"trial interval should use server's Retry-After (90s), not exponential backoff (20s)")
}

func TestTrialDispatch_CleansStaleTrialPending(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := context.Background()
	now := eng.nowFn()

	// Insert a stale trial entry (older than trialPendingTTL).
	eng.watch.trialMu.Lock()
	eng.watch.trialPending["stale.txt"] = trialEntry{
		scopeKey: synctypes.SKQuotaOwn,
		created:  now.Add(-2 * trialPendingTTL),
	}
	eng.watch.trialMu.Unlock()

	eng.watch.buf = syncobserve.NewBuffer(eng.logger)

	// No due scopes needed — stale cleanup runs first in runTrialDispatch.
	eng.runTrialDispatch(ctx)

	eng.watch.trialMu.Lock()
	remaining := len(eng.watch.trialPending)
	eng.watch.trialMu.Unlock()

	assert.Equal(t, 0, remaining,
		"stale trial entries should be cleaned up")
}
