package sync

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type blockingPermChecker struct {
	enterOnce sync.Once
	enterCh   chan struct{}
	releaseCh chan struct{}
	perms     []graph.Permission
}

func newBlockingPermChecker(perms []graph.Permission) *blockingPermChecker {
	return &blockingPermChecker{
		enterCh:   make(chan struct{}),
		releaseCh: make(chan struct{}),
		perms:     perms,
	}
}

func (c *blockingPermChecker) ListItemPermissions(_ context.Context, _ driveid.ID, _ string) ([]graph.Permission, error) {
	c.enterOnce.Do(func() {
		close(c.enterCh)
	})
	<-c.releaseCh
	return c.perms, nil
}

// Validates: R-2.1.2
func TestPhase0_RunWatch_BootstrapCompletesBeforeLocalObserverStarts(t *testing.T) {
	t.Parallel()

	uploadStarted := make(chan struct{})
	var uploadStartedOnce sync.Once
	allowUpload := make(chan struct{})

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadStartedOnce.Do(func() {
				close(uploadStarted)
			})
			<-allowUpload
			return &graph.Item{
				ID:           "uploaded-id",
				Name:         name,
				Size:         size,
				QuickXorHash: "upload-hash",
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	writeLocalFile(t, syncRoot, "local.txt", "bootstrap upload")
	recorder := attachDebugEventRecorder(eng)
	eng.localWatcherFactory = func() (syncobserve.FsWatcher, error) {
		return newEnospcWatcher(1 << 20), nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncUploadOnly, synctypes.WatchOpts{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()
	select {
	case <-uploadStarted:
	case <-time.After(2 * time.Second):
		require.Fail(t, "bootstrap upload should start before observers")
	}

	close(allowUpload)

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Note == engineDebugObserverLocal
	}, "local observer started")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not exit after cancellation")
	}
}

// Validates: R-2.1.2
func TestPhase0_RunWatch_BootstrapCompletesBeforeRemoteObserverStarts(t *testing.T) {
	t.Parallel()

	var deltaCalls atomic.Int32
	downloadStarted := make(chan struct{})
	var downloadStartedOnce sync.Once
	allowDownload := make(chan struct{})
	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			call := deltaCalls.Add(1)
			if call == 1 {
				return deltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{
						ID:           "bootstrap-item",
						Name:         "remote.txt",
						DriveID:      driveID,
						ParentID:     "root",
						Size:         12,
						QuickXorHash: "remote-hash",
					},
				}, "bootstrap-token"), nil
			}

			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "steady-state-token"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloadStartedOnce.Do(func() {
				close(downloadStarted)
			})
			<-allowDownload
			n, err := w.Write([]byte("remote-data"))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncDownloadOnly, synctypes.WatchOpts{
			PollInterval: 20 * time.Millisecond,
			Debounce:     5 * time.Millisecond,
		})
	}()
	select {
	case <-downloadStarted:
	case <-time.After(2 * time.Second):
		require.Fail(t, "bootstrap download should start before remote observer")
	}

	assert.Equal(t, int32(1), deltaCalls.Load(),
		"remote observer must not start polling until bootstrap has drained")

	close(allowDownload)

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Note == engineDebugObserverRemote
	}, "remote observer started")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not exit after cancellation")
	}
}

// Validates: R-6.8.9
func TestPhase0_ExecutePlan_WaitsForDrainSideEffects(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-phase0"
	checker := newBlockingPermChecker([]graph.Permission{{ID: "p1", Roles: []string{"read"}}})
	shortcuts := []synctypes.Shortcut{{
		ItemID:       "shortcut-1",
		RemoteDrive:  remoteDriveID,
		RemoteItem:   "root-id",
		LocalPath:    "Shared/TeamDocs",
		Observation:  synctypes.ObservationDelta,
		DiscoveredAt: 1000,
	}}
	baselineEntries := []synctypes.Outcome{{
		Action:   synctypes.ActionDownload,
		Success:  true,
		Path:     "Shared/TeamDocs",
		DriveID:  driveid.New(remoteDriveID),
		ItemID:   "root-id",
		ParentID: "root",
		ItemType: synctypes.ItemTypeFolder,
	}}

	eng, bl, syncRoot := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	runner := newOneShotRunner(eng.Engine)
	runner.setShortcuts(shortcuts)
	writeLocalFile(t, syncRoot, "Shared/TeamDocs/file.txt", "phase0")

	uploadMock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.GraphError{
				StatusCode: http.StatusForbidden,
				Err:        graph.ErrForbidden,
				Message:    "read-only shortcut",
			}
		},
	}
	eng.execCfg.SetUploads(uploadMock)
	eng.execCfg.SetTransferMgr(driveops.NewTransferManager(
		eng.execCfg.Downloads(), eng.execCfg.Uploads(), nil, eng.logger,
	))

	plan := &synctypes.ActionPlan{
		Actions: []synctypes.Action{{
			Type:    synctypes.ActionUpload,
			Path:    "Shared/TeamDocs/file.txt",
			DriveID: driveid.New(remoteDriveID),
		}},
		Deps: [][]int{nil},
	}

	report := &synctypes.SyncReport{}
	done := make(chan error, 1)
	go func() {
		done <- runner.executePlan(t.Context(), plan, report, bl)
	}()

	select {
	case <-checker.enterCh:
	case <-time.After(2 * time.Second):
		require.Fail(t, "permission check did not start")
	}

	select {
	case err := <-done:
		require.NoError(t, err)
		require.Fail(t, "executePlan returned before drain side effects finished")
	case <-time.After(150 * time.Millisecond):
	}

	close(checker.releaseCh)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "executePlan did not finish after permission check released")
	}

	permIssues, err := eng.baseline.ListRemoteBlockedFailures(t.Context())
	require.NoError(t, err)
	require.Len(t, permIssues, 1, "executePlan should wait for the blocked write row to be recorded before returning")
	assert.Equal(t, "Shared/TeamDocs/file.txt", permIssues[0].Path)
	assert.Equal(t, synctypes.SKPermRemote("Shared/TeamDocs"), permIssues[0].ScopeKey)
	assert.GreaterOrEqual(t, report.Failed, 1)
}

// Validates: R-2.10.5
func TestPhase0_OneShotEngineLoop_TrialFailureKeepsBlockedScopeIsolated(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done
	rt := testWatchRuntime(t, eng)

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 30 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(30 * time.Millisecond),
	})

	ta := rt.depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "trial.txt",
		DriveID: driveid.New(engineTestDriveID),
		ItemID:  "trial-item",
	}, 99, nil)
	require.NotNil(t, ta)

	results <- synctypes.WorkerResult{
		ActionID:      99,
		Path:          "trial.txt",
		ActionType:    synctypes.ActionDownload,
		DriveID:       driveid.New(engineTestDriveID),
		Success:       false,
		HTTPStatus:    http.StatusInternalServerError,
		Err:           graph.ErrServerError,
		ErrMsg:        "trial failure",
		IsTrial:       true,
		TrialScopeKey: synctypes.SKService(),
	}

	require.Eventually(t, func() bool {
		block, ok := getTestScopeBlock(eng, synctypes.SKService())
		return ok && block.TrialInterval == 60*time.Millisecond
	}, time.Second, 10*time.Millisecond, "trial failure should only extend the active scope interval")

	assert.True(t, isTestScopeBlocked(eng, synctypes.SKService()),
		"trial failure must not clear the blocked scope via the normal result path")
}

// Validates: R-2.10.5, R-2.10.11
func TestPhase0_OneShotEngineLoop_TrialSuccessMakesFailuresRetryableAndReinjectableWithoutExternalObservation(t *testing.T) {
	t.Parallel()

	const blockedPath = "blocked.txt"

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "blocked-item",
		Path:     "blocked.txt",
		ItemType: synctypes.ItemTypeFile,
		Hash:     "blocked-hash",
		Size:     42,
	}}, "", driveID))

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount(),
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	})
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      blockedPath,
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ErrMsg:    "rate limited",
		ScopeKey:  synctypes.SKThrottleAccount(),
		ItemID:    "blocked-item",
	}, nil))

	ta := rt.depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "trial.txt",
		DriveID: driveID,
		ItemID:  "trial-item",
	}, 1, nil)
	require.NotNil(t, ta)

	results <- synctypes.WorkerResult{
		ActionID:      1,
		Path:          "trial.txt",
		ActionType:    synctypes.ActionUpload,
		DriveID:       driveID,
		Success:       true,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKThrottleAccount(),
	}

	require.Eventually(t, func() bool {
		return !isTestScopeBlocked(eng, synctypes.SKThrottleAccount())
	}, 5*time.Second, 10*time.Millisecond, "trial success should clear the scope block")

	var retried *synctypes.TrackedAction
	require.Eventually(t, func() bool {
		select {
		case retried = <-rt.dispatchCh:
			return retried != nil && retried.Action.Path == blockedPath
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "trial success should re-dispatch the held failure without external observation")

	require.NotNil(t, retried)
	assert.Equal(t, synctypes.ActionDownload, retried.Action.Type)
}

// Validates: R-2.10.13, R-2.10.11
func TestPhase0_RecheckLocalPermissions_ReleasesHeldFailuresImmediately(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.buf = syncobserve.NewBuffer(eng.logger)

	scopeKey := synctypes.SKPermDir("Private")
	accessibleDir := filepath.Join(syncRoot, "Private")
	require.NoError(t, os.MkdirAll(accessibleDir, 0o750))

	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{{
		DriveID:  eng.driveID,
		ItemID:   "private-item",
		Path:     "Private/doc.txt",
		ItemType: synctypes.ItemTypeFile,
		Hash:     "private-hash",
		Size:     64,
	}}, "", eng.driveID))

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "Private",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionDownload,
		Role:      synctypes.FailureRoleBoundary,
		IssueType: synctypes.IssueLocalPermissionDenied,
		Category:  synctypes.CategoryActionable,
		ErrMsg:    "directory not accessible",
		ScopeKey:  scopeKey,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "Private/doc.txt",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionDownload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ErrMsg:    "blocked by perm scope",
		ScopeKey:  scopeKey,
		ItemID:    "private-item",
	}, nil))
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:       scopeKey,
		IssueType: synctypes.IssueLocalPermissionDenied,
		BlockedAt: eng.nowFunc(),
	})

	decisions := applyLocalPermissionRecheck(t, eng, ctx)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)

	assert.False(t, isTestScopeBlocked(eng, scopeKey),
		"local permission recheck should clear the active scope block")

	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, eng.nowFunc())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Private/doc.txt", rows[0].Path)

	outbox := runTestRetrierSweep(t, eng, ctx)
	require.Len(t, outbox, 1, "released failure should be dispatchable immediately via the retrier")
	assert.Equal(t, "Private/doc.txt", outbox[0].Action.Path)
}

// Validates: R-2.10.5
func TestPhase0_ScopeBlockFailureDoesNotReadmitDependentEarly(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng := startDrainLoop(t)
	defer cancel()
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	parent := rt.depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "parent.txt",
		DriveID: driveID,
		ItemID:  "parent-item",
	}, 1, nil)
	require.NotNil(t, parent)

	child := rt.depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "child.txt",
		DriveID: driveID,
		ItemID:  "child-item",
	}, 2, []int64{1})
	require.Nil(t, child)

	rt.dispatchCh <- parent
	readReady(t, rt.dispatchCh)

	results <- synctypes.WorkerResult{
		ActionID:   1,
		Path:       "parent.txt",
		ActionType: synctypes.ActionUpload,
		DriveID:    driveID,
		Success:    false,
		HTTPStatus: http.StatusTooManyRequests,
		RetryAfter: 25 * time.Millisecond,
		Err:        graph.ErrThrottled,
		ErrMsg:     "rate limited",
	}

	select {
	case ta := <-rt.dispatchCh:
		require.Failf(t, "dependent dispatched early", "unexpected path %s", ta.Action.Path)
	case <-time.After(150 * time.Millisecond):
	}

	require.Eventually(t, func() bool {
		return isTestScopeBlocked(eng, synctypes.SKThrottleAccount())
	}, time.Second, 10*time.Millisecond, "scope block should be activated from the worker result")

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, failures, 2, "parent failure and child cascade failure should both be persisted")
}

// Validates: R-2.1.6
func TestPhase0_RunFullReconciliationAsync_UsesBufferHandoffInsteadOfDirectDispatch(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{
						ID:           "reconcile-item",
						Name:         "reconcile.txt",
						DriveID:      driveID,
						ParentID:     "root",
						Size:         21,
						QuickXorHash: "reconcile-hash",
					},
				},
				DeltaLink: "reconcile-token",
			}, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	ready := setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.buf = syncobserve.NewBuffer(eng.logger)

	rt.runFullReconciliationAsync(ctx, bl)
	waitForReconcileDone(t, eng)

	select {
	case ta := <-ready:
		require.Failf(t, "reconciliation dispatched directly", "unexpected action %s", ta.Action.Path)
	default:
	}

	batch := rt.buf.FlushImmediate()
	require.NotEmpty(t, batch)
	assert.Equal(t, "reconcile.txt", batch[0].Path)
}
