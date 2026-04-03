package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// Observation-time validation: path too long is caught by shouldObserve
// ---------------------------------------------------------------------------

func TestRunOnce_PathTooLong_RecordsIssue(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Create a deeply nested file >400 chars total, each component <255 chars.
	// shouldObserve catches this at observation time — the file never enters
	// the planner. recordSkippedItems writes it to sync_failures.
	deepPath := ""
	for range 51 {
		deepPath = filepath.Join(deepPath, "abcdefgh")
	}
	deepPath = filepath.Join(deepPath, "file.txt")
	require.Greater(t, len(deepPath), 400)

	writeLocalFile(t, syncRoot, deepPath, "test content")

	report, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.NoError(t, err)

	// The upload should NOT have been attempted — caught at observation time.
	assert.Equal(t, 0, report.Uploads, "path-too-long file should not reach planner")

	// The sync_failures table should have an entry from recordSkippedItems.
	issues, issErr := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, issErr)
	require.NotEmpty(t, issues, "sync_failures should have an entry for path too long")

	found := false
	for _, iss := range issues {
		if iss.IssueType == synctypes.IssuePathTooLong {
			found = true

			break
		}
	}

	assert.True(t, found, "expected synctypes.IssuePathTooLong issue in sync_failures")
}

// ---------------------------------------------------------------------------
// Issue #10: one-shot engine-loop upload failure recording
// ---------------------------------------------------------------------------

// Validates: R-6.8.9
func TestOneShotEngineLoop_ClosedResultsStillProcessBufferedSideEffects(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = syncdispatch.NewDepGraph(eng.logger)
	runner.readyCh = make(chan *synctypes.TrackedAction, 16)
	for _, id := range []int64{1, 2, 3} {
		runner.depGraph.Add(&synctypes.Action{Path: fmt.Sprintf("action-%d", id), Type: synctypes.ActionUpload}, id, nil)
	}

	results := make(chan synctypes.WorkerResult, 3)
	results <- synctypes.WorkerResult{Path: "a.txt", ActionType: synctypes.ActionUpload, Success: false, ErrMsg: "fail1", HTTPStatus: 500, ActionID: 1}
	results <- synctypes.WorkerResult{Path: "b.txt", ActionType: synctypes.ActionUpload, Success: false, ErrMsg: "fail2", HTTPStatus: 500, ActionID: 2}
	results <- synctypes.WorkerResult{Path: "c.txt", ActionType: synctypes.ActionDownload, Success: true, ActionID: 3}
	close(results)

	err := runner.runResultsLoop(ctx, nil, nil, results)
	require.NoError(t, err)

	// Both upload failures should produce sync_failures.
	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, issues, 2, "one-shot engine loop should process all buffered results before exiting")
}

// Validates: R-2.10.5
func TestOneShotEngineLoop_UnauthorizedTerminatesAndDrainsQueuedReady(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	runner := newOneShotRunner(eng.Engine)
	runner.depGraph = syncdispatch.NewDepGraph(eng.logger)
	runner.readyCh = make(chan *synctypes.TrackedAction)

	runner.depGraph.Add(&synctypes.Action{
		Type: synctypes.ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	runner.depGraph.Add(&synctypes.Action{
		Type: synctypes.ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	runner.depGraph.Add(&synctypes.Action{
		Type: synctypes.ActionDownload,
		Path: "auth.txt",
	}, 3, nil)

	results := make(chan synctypes.WorkerResult, 2)
	results <- synctypes.WorkerResult{
		ActionID:   1,
		Path:       "root.txt",
		ActionType: synctypes.ActionUpload,
		Success:    true,
	}
	results <- synctypes.WorkerResult{
		ActionID:   3,
		Path:       "auth.txt",
		ActionType: synctypes.ActionDownload,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	}
	close(results)

	err := runner.runResultsLoop(ctx, nil, nil, results)
	require.ErrorIs(t, err, graph.ErrUnauthorized)
	assert.Equal(t, 0, runner.depGraph.InFlightCount(), "fatal termination should drain queued ready actions as shutdown")

	blocks, blockErr := eng.baseline.ListScopeBlocks(ctx)
	require.NoError(t, blockErr)
	require.Len(t, blocks, 1)
	assert.Equal(t, synctypes.SKAuthAccount(), blocks[0].Key)
	assert.Equal(t, synctypes.IssueUnauthorized, blocks[0].IssueType)
	assert.Equal(t, synctypes.ScopeTimingNone, blocks[0].TimingSource)
	assert.Zero(t, blocks[0].TrialInterval)
	assert.True(t, blocks[0].NextTrialAt.IsZero())
	assert.True(t, blocks[0].PreserveUntil.IsZero())

	failures, failureErr := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, failureErr)
	assert.Empty(t, failures, "fatal unauthorized should not create per-path sync_failures rows")
}

func assertUnauthorizedWatchHandlerStopsLoop(
	t *testing.T,
	handler func(*watchRuntime, context.Context, *watchPipeline, *synctypes.WorkerResult) ([]*synctypes.TrackedAction, bool, error),
) {
	t.Helper()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	rt := testWatchRuntime(t, eng)
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "auth.txt",
		DriveID: eng.driveID,
		ItemID:  "item-1",
	}, 21, nil)

	_, done, gotErr := handler(rt, ctx, &watchPipeline{bl: bl}, &synctypes.WorkerResult{
		ActionID:   21,
		Path:       "auth.txt",
		DriveID:    eng.driveID,
		ActionType: synctypes.ActionDownload,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	})

	assert.False(t, done)
	require.ErrorIs(t, gotErr, graph.ErrUnauthorized)
}

// Validates: R-2.10.5
func TestHandleBootstrapWorkerResult_UnauthorizedStopsBootstrap(t *testing.T) {
	t.Parallel()

	assertUnauthorizedWatchHandlerStopsLoop(t, func(
		rt *watchRuntime,
		ctx context.Context,
		p *watchPipeline,
		workerResult *synctypes.WorkerResult,
	) ([]*synctypes.TrackedAction, bool, error) {
		return rt.handleBootstrapWorkerResult(ctx, p, nil, workerResult, true)
	})
}

// Validates: R-2.10.5
func TestHandleWatchWorkerResult_UnauthorizedStopsWatchLoop(t *testing.T) {
	t.Parallel()

	assertUnauthorizedWatchHandlerStopsLoop(t, func(
		rt *watchRuntime,
		ctx context.Context,
		p *watchPipeline,
		workerResult *synctypes.WorkerResult,
	) ([]*synctypes.TrackedAction, bool, error) {
		return rt.handleWatchWorkerResult(ctx, p, nil, workerResult, true)
	})
}

// ---------------------------------------------------------------------------
// processWorkerResult — shared helper tests
// ---------------------------------------------------------------------------

// setupEngineDepGraph creates a syncdispatch.DepGraph on the engine and adds a dummy action
// for the given actionID so that processWorkerResult can call Complete without
// panicking on nil depGraph or unknown ID.
func setupEngineDepGraph(t *testing.T, eng *testEngine, actionID int64) *engineFlow {
	t.Helper()

	flow := &engineFlow{
		engine:   eng.Engine,
		depGraph: syncdispatch.NewDepGraph(eng.logger),
		readyCh:  make(chan *synctypes.TrackedAction, 16),
	}
	dummyAction := &synctypes.Action{Path: "dummy", Type: synctypes.ActionDownload}
	flow.depGraph.Add(dummyAction, actionID, nil)
	eng.flow = flow

	return flow
}

func TestProcessWorkerResult_UploadFailure_RecordsLocalIssue(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 0)

	processWorkerResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		Path:       "docs/report.xlsx",
		ActionType: synctypes.ActionUpload,
		Success:    false,
		ErrMsg:     "connection reset",
		HTTPStatus: 503,
	}, nil)

	// Should record upload failure in sync_failures.
	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "docs/report.xlsx", issues[0].Path)
	assert.Equal(t, synctypes.DirectionUpload, issues[0].Direction)
	assert.Equal(t, "connection reset", issues[0].LastError)
	assert.Equal(t, 503, issues[0].HTTPStatus)
}

func TestProcessWorkerResult_403ReadOnly_SkipsRemoteState(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(remoteDriveID).String() + ":root-id": {{ID: "p1", Roles: []string{"read"}}},
		},
	}

	shortcuts := []synctypes.Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: synctypes.ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []synctypes.Outcome{
		{
			Action: synctypes.ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: synctypes.ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()
	flow := setupEngineDepGraph(t, eng, 0)

	// Seed the latest shortcut snapshot so getShortcuts() returns it for handle403.
	flow.setShortcuts(shortcuts)

	flow.processWorkerResult(ctx, nil, &synctypes.WorkerResult{
		Path:       "Shared/TeamDocs/file.txt",
		ActionType: synctypes.ActionUpload,
		Success:    false,
		ErrMsg:     "403 Forbidden",
		HTTPStatus: 403,
	}, bl)

	// Confirmed remote read-only should collapse to one held blocked-write row.
	permIssues, err := eng.baseline.ListRemoteBlockedFailures(ctx)
	require.NoError(t, err)
	require.Len(t, permIssues, 1, "confirmed remote denial should record one blocked write row")
	assert.Equal(t, "Shared/TeamDocs/file.txt", permIssues[0].Path)
	assert.Equal(t, synctypes.SKPermRemote("Shared/TeamDocs"), permIssues[0].ScopeKey)

	// remote_state should be empty.
	failed, err := eng.baseline.ListActionableRemoteState(ctx)
	require.NoError(t, err)
	assert.Empty(t, failed, "confirmed read-only 403 should not be in remote_state")

	allFailures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, allFailures, 1, "confirmed remote denial should not leave a duplicate file-level failure behind")
}

func TestProcessWorkerResult_Success_NoRecords(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 0)

	processWorkerResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		Path:       "docs/report.xlsx",
		ActionType: synctypes.ActionDownload,
		Success:    true,
	}, nil)

	// No failures should be recorded.
	failed, err := eng.baseline.ListActionableRemoteState(ctx)
	require.NoError(t, err)
	assert.Empty(t, failed)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

// Validates: R-2.10.5
func TestProcessWorkerResult_UnauthorizedTerminatesRouting(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionDownload,
		Path:    "auth.txt",
		DriveID: driveid.New("d"),
		ItemID:  "item-1",
	}, 17, nil)

	outcome := processWorkerResultDetailedForTest(t, eng, ctx, &synctypes.WorkerResult{
		ActionID:   17,
		Path:       "auth.txt",
		ActionType: synctypes.ActionDownload,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	}, nil)

	require.ErrorIs(t, outcome.terminateErr, graph.ErrUnauthorized)
	assert.True(t, outcome.terminate)

	authBlock, ok := getTestScopeBlock(eng, synctypes.SKAuthAccount())
	require.True(t, ok, "fatal unauthorized should persist an auth scope block")
	assert.Equal(t, synctypes.IssueUnauthorized, authBlock.IssueType)
	assert.Equal(t, synctypes.ScopeTimingNone, authBlock.TimingSource)
	assert.Zero(t, authBlock.TrialInterval)
	assert.True(t, authBlock.NextTrialAt.IsZero())
	assert.True(t, authBlock.PreserveUntil.IsZero())

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "fatal unauthorized should not create per-path sync_failures rows")
}

// ---------------------------------------------------------------------------
// classifyResult — pure classification of synctypes.WorkerResult (R-6.8.15)
// ---------------------------------------------------------------------------

// Validates: R-6.8.15, R-6.7.12
type classifyResultCase struct {
	name              string
	result            synctypes.WorkerResult
	wantClass         resultClass
	wantScope         synctypes.ScopeKey
	wantRecordMode    failureRecordMode
	wantPermission    permissionFlow
	wantScopeDetect   bool
	wantRecordSuccess bool
}

func assertClassifyResultCases(t *testing.T, tests []classifyResultCase) {
	t.Helper()

	for i := range tests {
		tt := tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyResult(&tt.result)
			assert.Equal(t, tt.wantClass, got.Class, "resultClass mismatch")
			assert.Equal(t, tt.wantScope, got.ScopeKey, "scope key mismatch")
			assert.Equal(t, tt.wantRecordMode, got.RecordMode, "record mode mismatch")
			assert.Equal(t, tt.wantPermission, got.PermissionFlow, "permission flow mismatch")
			assert.Equal(t, tt.wantScopeDetect, got.RunScopeDetection, "scope detection mismatch")
			assert.Equal(t, tt.wantRecordSuccess, got.RecordSuccess, "record success mismatch")
		})
	}
}

func TestClassifyResult_LifecycleAndAuth(t *testing.T) {
	t.Parallel()

	assertClassifyResultCases(t, []classifyResultCase{
		{name: "success", result: synctypes.WorkerResult{Success: true}, wantClass: resultSuccess, wantRecordSuccess: true},
		{name: "context_canceled", result: synctypes.WorkerResult{Err: context.Canceled}, wantClass: resultShutdown},
		{name: "context_deadline_exceeded", result: synctypes.WorkerResult{Err: context.DeadlineExceeded}, wantClass: resultShutdown},
		{
			name:      "wrapped_context_canceled",
			result:    synctypes.WorkerResult{Err: fmt.Errorf("operation failed: %w", context.Canceled)},
			wantClass: resultShutdown,
		},
		{
			name:           "401_unauthorized",
			result:         synctypes.WorkerResult{HTTPStatus: http.StatusUnauthorized, Err: graph.ErrUnauthorized},
			wantClass:      resultFatal,
			wantRecordMode: recordFailureNone,
		},
		{
			name:           "403_forbidden",
			result:         synctypes.WorkerResult{HTTPStatus: http.StatusForbidden, Err: graph.ErrForbidden},
			wantClass:      resultSkip,
			wantRecordMode: recordFailureActionable,
			wantPermission: permissionFlowRemote403,
		},
	})
}

func TestClassifyResult_RemoteRetriesAndSkips(t *testing.T) {
	t.Parallel()

	genericInvalidRequestErr := &graph.GraphError{
		StatusCode: http.StatusBadRequest,
		Code:       "badRequest",
		InnerCodes: []string{"invalidRequest"},
		Message:    "Invalid request",
		Err:        graph.ErrBadRequest,
	}
	wrongCodeOutageErr := &graph.GraphError{
		StatusCode: http.StatusBadRequest,
		Code:       "badRequest",
		InnerCodes: []string{"nameAlreadyExists"},
		Message:    "ObjectHandle is Invalid for operation",
		Err:        graph.ErrBadRequest,
	}
	legacyOutageErr := &graph.GraphError{
		StatusCode: http.StatusBadRequest,
		Message:    "ObjectHandle is Invalid for operation",
		Err:        graph.ErrBadRequest,
	}

	assertClassifyResultCases(t, []classifyResultCase{
		{name: "404_not_found", result: synctypes.WorkerResult{HTTPStatus: http.StatusNotFound, Err: graph.ErrNotFound}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "408_request_timeout", result: synctypes.WorkerResult{HTTPStatus: http.StatusRequestTimeout, Err: errors.New("timeout")}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "412_precondition_failed", result: synctypes.WorkerResult{HTTPStatus: http.StatusPreconditionFailed, Err: errors.New("etag mismatch")}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "423_locked", result: synctypes.WorkerResult{HTTPStatus: http.StatusLocked, Err: graph.ErrLocked}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "429_too_many_requests", result: synctypes.WorkerResult{HTTPStatus: http.StatusTooManyRequests, Err: graph.ErrThrottled}, wantClass: resultScopeBlock, wantScope: synctypes.SKThrottleAccount(), wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "400_invalid_request_is_skip", result: synctypes.WorkerResult{HTTPStatus: http.StatusBadRequest, Err: genericInvalidRequestErr}, wantClass: resultSkip, wantRecordMode: recordFailureActionable},
		{name: "400_object_handle_message_only_is_skip", result: synctypes.WorkerResult{HTTPStatus: http.StatusBadRequest, Err: legacyOutageErr}, wantClass: resultSkip, wantRecordMode: recordFailureActionable},
		{name: "400_object_handle_wrong_code_is_skip", result: synctypes.WorkerResult{HTTPStatus: http.StatusBadRequest, Err: wrongCodeOutageErr}, wantClass: resultSkip, wantRecordMode: recordFailureActionable},
		{name: "500_internal_server_error", result: synctypes.WorkerResult{HTTPStatus: http.StatusInternalServerError, Err: graph.ErrServerError}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "502_bad_gateway", result: synctypes.WorkerResult{HTTPStatus: http.StatusBadGateway, Err: graph.ErrServerError}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "503_service_unavailable", result: synctypes.WorkerResult{HTTPStatus: http.StatusServiceUnavailable, Err: graph.ErrServerError}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "504_gateway_timeout", result: synctypes.WorkerResult{HTTPStatus: http.StatusGatewayTimeout, Err: graph.ErrServerError}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "509_bandwidth_limit", result: synctypes.WorkerResult{HTTPStatus: 509, Err: graph.ErrServerError}, wantClass: resultRequeue, wantRecordMode: recordFailureReconcile, wantScopeDetect: true},
		{name: "409_conflict", result: synctypes.WorkerResult{HTTPStatus: http.StatusConflict, Err: graph.ErrConflict}, wantClass: resultSkip, wantRecordMode: recordFailureActionable},
		{name: "other_4xx_falls_to_skip", result: synctypes.WorkerResult{HTTPStatus: http.StatusMethodNotAllowed, Err: graph.ErrMethodNotAllowed}, wantClass: resultSkip, wantRecordMode: recordFailureActionable},
	})
}

func TestClassifyResult_StorageScopes(t *testing.T) {
	t.Parallel()

	assertClassifyResultCases(t, []classifyResultCase{
		{
			name: "507_own_drive",
			result: synctypes.WorkerResult{
				HTTPStatus:  http.StatusInsufficientStorage,
				Err:         errors.New("insufficient storage"),
				ShortcutKey: "",
			},
			wantClass:       resultScopeBlock,
			wantScope:       synctypes.SKQuotaOwn(),
			wantRecordMode:  recordFailureReconcile,
			wantScopeDetect: true,
		},
		{
			name: "507_shortcut_drive",
			result: synctypes.WorkerResult{
				HTTPStatus:  http.StatusInsufficientStorage,
				Err:         errors.New("insufficient storage"),
				ShortcutKey: "drive1:item1",
			},
			wantClass:       resultScopeBlock,
			wantScope:       synctypes.SKQuotaShortcut("drive1:item1"),
			wantRecordMode:  recordFailureReconcile,
			wantScopeDetect: true,
		},
	})
}

func TestClassifyResult_LocalErrors(t *testing.T) {
	t.Parallel()

	assertClassifyResultCases(t, []classifyResultCase{
		{name: "os_err_permission", result: synctypes.WorkerResult{Err: os.ErrPermission}, wantClass: resultSkip, wantRecordMode: recordFailureActionable, wantPermission: permissionFlowLocalPermission},
		{
			name:           "wrapped_os_err_permission",
			result:         synctypes.WorkerResult{Err: fmt.Errorf("cannot write: %w", os.ErrPermission)},
			wantClass:      resultSkip,
			wantRecordMode: recordFailureActionable,
			wantPermission: permissionFlowLocalPermission,
		},
		{
			name:           "disk_full",
			result:         synctypes.WorkerResult{Err: fmt.Errorf("download failed: %w", driveops.ErrDiskFull)},
			wantClass:      resultScopeBlock,
			wantScope:      synctypes.SKDiskLocal(),
			wantRecordMode: recordFailureReconcile,
		},
		{
			name:           "file_too_large_for_space",
			result:         synctypes.WorkerResult{Err: fmt.Errorf("download failed: %w", driveops.ErrFileTooLargeForSpace)},
			wantClass:      resultSkip,
			wantRecordMode: recordFailureActionable,
		},
		{
			name:           "file_exceeds_onedrive_limit",
			result:         synctypes.WorkerResult{Err: fmt.Errorf("upload failed: %w", driveops.ErrFileExceedsOneDriveLimit)},
			wantClass:      resultSkip,
			wantRecordMode: recordFailureActionable,
		},
	})
}

// computeBackoff tests removed — backoff is now handled by retry.Reconcile
// policy via sync_failures + the integrated retrier. See internal/retry/named_test.go.

// ---------------------------------------------------------------------------
// processTrialResult (R-2.10.5, R-2.10.6, R-2.10.8, R-2.10.14)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestProcessTrialResultV2_Success_ClearsScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	// Set up an active persisted scope block.
	now := eng.nowFunc()
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount(),
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 10 * time.Second,
		NextTrialAt:   now.Add(10 * time.Second),
	})

	// Add scope-blocked failures to the DB (these would be unblocked on success).
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "first.txt", DriveID: driveid.New("d"), Direction: synctypes.DirectionUpload,
		Role:     synctypes.FailureRoleHeld,
		Category: synctypes.CategoryTransient, ErrMsg: "rate limited", ScopeKey: synctypes.SKThrottleAccount(),
	}, nil)) // nil delayFn → scope-blocked (next_retry_at = NULL)

	// Add the trial action to the syncdispatch.DepGraph.
	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{Type: synctypes.ActionUpload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)

	// Simulate successful trial result.
	processTrialResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		ActionID:      1,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKThrottleAccount(),
		Success:       true,
	})

	// Scope block should be cleared.
	assert.False(t, isTestScopeBlocked(eng, synctypes.SKThrottleAccount()),
		"scope block should be removed after successful trial")

	// Scope-blocked failures should now be retryable (next_retry_at set to ~now).
	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "scope-blocked failures should be unblocked after trial success")
}

// Validates: R-2.10.14
func TestProcessTrialResultV2_Failure_DoublesInterval(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	now := eng.nowFunc()
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	})

	// Add the trial action to the syncdispatch.DepGraph.
	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{Type: synctypes.ActionDownload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

	processTrialResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKService(),
		Success:       false,
		HTTPStatus:    503,
		ErrMsg:        "service unavailable",
	})

	// Verify block's TrialInterval was doubled.
	got, ok := getTestScopeBlock(eng, synctypes.SKService())
	require.True(t, ok)
	assert.Equal(t, 60*time.Second, got.TrialInterval, "interval should be doubled")
}

// Validates: R-2.10.6, R-2.10.8, R-2.10.14 — unified cap for all scope types.
func TestProcessTrialResultV2_Failure_CapsAt5m(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scopeKey   synctypes.ScopeKey
		issueType  string
		httpStatus int
		actionType synctypes.ActionType
	}{
		{"quota", synctypes.SKQuotaOwn(), synctypes.IssueQuotaExceeded, 507, synctypes.ActionUpload},
		{"service", synctypes.SKService(), synctypes.IssueServiceOutage, 500, synctypes.ActionDownload},
		{"throttle", synctypes.SKThrottleAccount(), synctypes.IssueRateLimited, 429, synctypes.ActionUpload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng := newSingleOwnerEngine(t)
			ctx := t.Context()

			now := eng.nowFunc()

			// Start with an interval that would exceed 5m when doubled.
			setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
				Key:           tt.scopeKey,
				IssueType:     tt.issueType,
				BlockedAt:     now,
				TrialInterval: 4 * time.Minute,
				NextTrialAt:   now.Add(4 * time.Minute),
			})

			testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{Type: tt.actionType, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

			processTrialResultForTest(t, eng, ctx, &synctypes.WorkerResult{
				ActionID:      99,
				IsTrial:       true,
				TrialScopeKey: tt.scopeKey,
				Success:       false,
				HTTPStatus:    tt.httpStatus,
				ErrMsg:        "test failure",
			})

			got, ok := getTestScopeBlock(eng, tt.scopeKey)
			require.True(t, ok)
			assert.Equal(t, syncdispatch.DefaultMaxTrialInterval, got.TrialInterval,
				"%s interval should cap at %v", tt.name, syncdispatch.DefaultMaxTrialInterval)
		})
	}
}

// Validates: Group A — trial failure must NOT trigger scope detection.
func TestProcessTrialResultV2_Failure_NoScopeDetection(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	ss := syncdispatch.NewScopeState(eng.nowFn, eng.logger)
	testWatchRuntime(t, eng).scopeState = ss

	now := eng.nowFunc()
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	})

	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{Type: synctypes.ActionDownload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

	processTrialResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKService(),
		Success:       false,
		HTTPStatus:    500,
		ErrMsg:        "internal server error",
	})

	got, ok := getTestScopeBlock(eng, synctypes.SKService())
	require.True(t, ok, "scope block should still exist")
	assert.Equal(t, 60*time.Second, got.TrialInterval, "interval should be doubled, not reset")
}

// Validates: R-2.10.5
func TestProcessTrialResultV2_Preserve_RetryableHTTPStatusesKeepScopeTimingAndHeldCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		err    error
		errMsg string
	}{
		{name: "404_not_found", status: http.StatusNotFound, err: graph.ErrNotFound, errMsg: "transient not found"},
		{name: "408_timeout", status: http.StatusRequestTimeout, err: errors.New("timeout"), errMsg: "timeout"},
		{name: "412_precondition", status: http.StatusPreconditionFailed, err: errors.New("etag mismatch"), errMsg: "etag mismatch"},
		{name: "423_locked", status: http.StatusLocked, err: graph.ErrLocked, errMsg: "locked"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng := newSingleOwnerEngine(t)
			ctx := t.Context()
			now := eng.nowFunc()

			setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
				Key:           synctypes.SKService(),
				IssueType:     synctypes.IssueServiceOutage,
				BlockedAt:     now,
				TrialInterval: 30 * time.Second,
				NextTrialAt:   now.Add(30 * time.Second),
				TrialCount:    2,
			})
			require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
				Path:      "trial.txt",
				DriveID:   eng.driveID,
				Direction: synctypes.DirectionDownload,
				Role:      synctypes.FailureRoleHeld,
				Category:  synctypes.CategoryTransient,
				ScopeKey:  synctypes.SKService(),
				ItemID:    "i1",
			}, nil))

			testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
				Type:    synctypes.ActionDownload,
				Path:    "trial.txt",
				DriveID: driveid.New("d"),
				ItemID:  "i1",
			}, 99, nil)

			processTrialResultForTest(t, eng, ctx, &synctypes.WorkerResult{
				ActionID:      99,
				IsTrial:       true,
				TrialScopeKey: synctypes.SKService(),
				ActionType:    synctypes.ActionDownload,
				Path:          "trial.txt",
				DriveID:       eng.driveID,
				Success:       false,
				HTTPStatus:    tt.status,
				Err:           tt.err,
				ErrMsg:        tt.errMsg,
			})

			got, ok := getTestScopeBlock(eng, synctypes.SKService())
			require.True(t, ok, "scope block should still exist")
			assert.Equal(t, 30*time.Second, got.TrialInterval, "inconclusive trial must not back off the original scope")
			assert.Equal(t, now.Add(30*time.Second), got.NextTrialAt, "preserve should re-arm the original interval")
			assert.Equal(t, 2, got.TrialCount, "preserve should not increment trial backoff history")

			failures, err := eng.baseline.ListSyncFailures(ctx)
			require.NoError(t, err)
			require.Len(t, failures, 1)
			assert.Equal(t, synctypes.FailureRoleHeld, failures[0].Role,
				"inconclusive trial should leave the candidate held for the original scope")
			assert.Equal(t, synctypes.SKService(), failures[0].ScopeKey)
		})
	}
}

// Validates: R-2.10.5
func TestProcessTrialResultV2_Fatal401DoesNotExtendScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount(),
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 45 * time.Second,
		NextTrialAt:   now.Add(45 * time.Second),
		TrialCount:    3,
	})
	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		ItemID:  "i1",
	}, 77, nil)

	outcome := processWorkerResultDetailedForTest(t, eng, ctx, &synctypes.WorkerResult{
		ActionID:      77,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKThrottleAccount(),
		ActionType:    synctypes.ActionUpload,
		Path:          "trial.txt",
		DriveID:       eng.driveID,
		Success:       false,
		HTTPStatus:    http.StatusUnauthorized,
		Err:           graph.ErrUnauthorized,
		ErrMsg:        "unauthorized",
	}, nil)

	require.ErrorIs(t, outcome.terminateErr, graph.ErrUnauthorized)
	assert.True(t, outcome.terminate, "trial unauthorized should terminate result routing")

	got, ok := getTestScopeBlock(eng, synctypes.SKThrottleAccount())
	require.True(t, ok, "fatal unauthorized should not clear the original scope")
	assert.Equal(t, 45*time.Second, got.TrialInterval, "fatal unauthorized must not back off the original scope")
	assert.Equal(t, now.Add(45*time.Second), got.NextTrialAt, "fatal unauthorized must not reschedule the original scope")
	assert.Equal(t, 3, got.TrialCount)

	authBlock, authOK := getTestScopeBlock(eng, synctypes.SKAuthAccount())
	require.True(t, authOK, "trial unauthorized should activate the auth scope")
	assert.Equal(t, synctypes.IssueUnauthorized, authBlock.IssueType)
	assert.Equal(t, synctypes.ScopeTimingNone, authBlock.TimingSource)
	assert.Zero(t, authBlock.TrialInterval)
	assert.True(t, authBlock.NextTrialAt.IsZero())
	assert.True(t, authBlock.PreserveUntil.IsZero())

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "trial unauthorized should not create per-path sync_failures rows")
}

// Validates: R-2.10.5, R-2.10.14
func TestProcessTrialResultV2_Preserve_LocalPermissionRecordsCandidateFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 45 * time.Second,
		NextTrialAt:   now.Add(45 * time.Second),
		TrialCount:    1,
	})
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "trial.txt",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  synctypes.SKService(),
		ItemID:    "i1",
	}, nil))

	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		ItemID:  "i1",
	}, 88, nil)

	processTrialResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		ActionID:      88,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKService(),
		ActionType:    synctypes.ActionUpload,
		Path:          "trial.txt",
		DriveID:       eng.driveID,
		Success:       false,
		Err:           os.ErrPermission,
		ErrMsg:        "permission denied",
	})

	got, ok := getTestScopeBlock(eng, synctypes.SKService())
	require.True(t, ok)
	assert.Equal(t, 45*time.Second, got.TrialInterval)
	assert.Equal(t, now.Add(45*time.Second), got.NextTrialAt)
	assert.Equal(t, 1, got.TrialCount)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, synctypes.FailureRoleItem, failures[0].Role)
	assert.Equal(t, synctypes.IssueLocalPermissionDenied, failures[0].IssueType)
	assert.True(t, failures[0].ScopeKey.IsZero(), "file-level local permission preserve should not rewrite the original scope")
}

// Validates: R-2.10.5, R-2.10.14, R-2.14.1
func TestProcessTrialResultV2_Preserve_Remote403RehomesCandidateToPermissionScope(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID
	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(remoteDriveID).String() + ":root-id": {{ID: "p1", Roles: []string{"read"}}},
		},
	}
	shortcuts := []synctypes.Shortcut{{
		ItemID:       "sc-1",
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

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	setupWatchEngine(t, eng)
	testEngineFlow(t, eng).setShortcuts(shortcuts)

	ctx := t.Context()
	now := eng.nowFunc()
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
		TrialCount:    4,
	})
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "Shared/TeamDocs/file.txt",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  synctypes.SKService(),
		ItemID:    "i1",
	}, nil))

	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "Shared/TeamDocs/file.txt",
		DriveID: eng.driveID,
		ItemID:  "i1",
	}, 55, nil)

	processWorkerResultDetailedForTest(t, eng, ctx, &synctypes.WorkerResult{
		ActionID:      55,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKService(),
		ActionType:    synctypes.ActionUpload,
		Path:          "Shared/TeamDocs/file.txt",
		DriveID:       eng.driveID,
		Success:       false,
		HTTPStatus:    http.StatusForbidden,
		Err:           graph.ErrForbidden,
		ErrMsg:        "read-only",
	}, bl)

	got, ok := getTestScopeBlock(eng, synctypes.SKService())
	require.True(t, ok)
	assert.Equal(t, 30*time.Second, got.TrialInterval)
	assert.Equal(t, now.Add(30*time.Second), got.NextTrialAt)
	assert.Equal(t, 4, got.TrialCount)
	assert.True(t, isTestScopeBlocked(eng, synctypes.SKPermRemote("Shared/TeamDocs")),
		"preserved candidate should activate the more specific permission scope")

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "Shared/TeamDocs/file.txt", failures[0].Path)
	assert.Equal(t, synctypes.FailureRoleHeld, failures[0].Role)
	assert.Equal(t, synctypes.SKPermRemote("Shared/TeamDocs"), failures[0].ScopeKey)
}

// Validates: R-2.10.5, R-2.10.6, R-2.10.7, R-2.10.8, R-2.10.43
func TestEvaluateTrialOutcome_OnlyMatchingScopeEvidenceExtends(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	tests := []struct {
		name     string
		scopeKey synctypes.ScopeKey
		result   synctypes.WorkerResult
		want     trialOutcome
	}{
		{
			name:     "throttle_429_extends",
			scopeKey: synctypes.SKThrottleAccount(),
			result:   synctypes.WorkerResult{HTTPStatus: http.StatusTooManyRequests, Err: graph.ErrThrottled},
			want:     trialOutcomeExtend,
		},
		{
			name:     "service_503_extends",
			scopeKey: synctypes.SKService(),
			result:   synctypes.WorkerResult{HTTPStatus: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			want:     trialOutcomeExtend,
		},
		{
			name:     "quota_own_507_extends",
			scopeKey: synctypes.SKQuotaOwn(),
			result:   synctypes.WorkerResult{HTTPStatus: http.StatusInsufficientStorage},
			want:     trialOutcomeExtend,
		},
		{
			name:     "quota_shortcut_matching_507_extends",
			scopeKey: synctypes.SKQuotaShortcut("drive1:item1"),
			result:   synctypes.WorkerResult{HTTPStatus: http.StatusInsufficientStorage, ShortcutKey: "drive1:item1"},
			want:     trialOutcomeExtend,
		},
		{
			name:     "disk_full_extends",
			scopeKey: synctypes.SKDiskLocal(),
			result:   synctypes.WorkerResult{Err: driveops.ErrDiskFull},
			want:     trialOutcomeExtend,
		},
		{
			name:     "throttle_does_not_extend_service_error",
			scopeKey: synctypes.SKThrottleAccount(),
			result:   synctypes.WorkerResult{HTTPStatus: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			want:     trialOutcomePreserve,
		},
		{
			name:     "service_does_not_extend_throttle_error",
			scopeKey: synctypes.SKService(),
			result:   synctypes.WorkerResult{HTTPStatus: http.StatusTooManyRequests, Err: graph.ErrThrottled},
			want:     trialOutcomePreserve,
		},
		{
			name:     "quota_shortcut_mismatch_preserves",
			scopeKey: synctypes.SKQuotaShortcut("drive1:item1"),
			result:   synctypes.WorkerResult{HTTPStatus: http.StatusInsufficientStorage, ShortcutKey: "drive2:item2"},
			want:     trialOutcomePreserve,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			decision := classifyResult(&tt.result)
			assert.Equal(t, tt.want, flow.evaluateTrialOutcome(tt.scopeKey, decision, &tt.result))
		})
	}
}

// Validates: R-2.10.14 — computeTrialInterval is the single source of truth
// for initial intervals and backoff extensions.
func TestComputeTrialInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		scopeKey        synctypes.ScopeKey
		retryAfter      time.Duration
		currentInterval time.Duration
		want            time.Duration
	}{
		// Retry-After: used directly, no cap (R-2.10.7).
		{"retry-after honored", synctypes.SKService(), 90 * time.Second, 0, 90 * time.Second},
		{"retry-after exceeds max", synctypes.SKService(), 30 * time.Minute, 0, 30 * time.Minute},
		{"retry-after with current", synctypes.SKService(), 2 * time.Minute, 30 * time.Second, 2 * time.Minute},

		// No Retry-After, no current: initial interval.
		{"initial interval", synctypes.SKService(), 0, 0, syncdispatch.DefaultInitialTrialInterval},
		{"disk initial interval", synctypes.SKDiskLocal(), 0, 0, diskScopeInitialTrialInterval},

		// No Retry-After, with current: double + cap.
		{"double interval", synctypes.SKService(), 0, 30 * time.Second, 60 * time.Second},
		{"double caps at max", synctypes.SKService(), 0, 4 * time.Minute, syncdispatch.DefaultMaxTrialInterval},
		{"already at max stays", synctypes.SKService(), 0, syncdispatch.DefaultMaxTrialInterval, syncdispatch.DefaultMaxTrialInterval},
		{"disk double interval", synctypes.SKDiskLocal(), 0, 30 * time.Minute, 60 * time.Minute},
		{"disk caps at max", synctypes.SKDiskLocal(), 0, 45 * time.Minute, diskScopeMaxTrialInterval},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTrialInterval(tt.scopeKey, tt.retryAfter, tt.currentInterval)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-2.10.7 — Retry-After is used directly with no cap.
func TestExtendTrialInterval_WithRetryAfter(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	now := eng.nowFunc()
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount(),
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	})

	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{Type: synctypes.ActionUpload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

	// Retry-After of 30 minutes exceeds syncdispatch.DefaultMaxTrialInterval (5m) — must be
	// honored directly with no cap, because the server is ground truth.
	processTrialResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKThrottleAccount(),
		Success:       false,
		HTTPStatus:    429,
		RetryAfter:    30 * time.Minute,
		ErrMsg:        "too many requests",
	})

	got, ok := getTestScopeBlock(eng, synctypes.SKThrottleAccount())
	require.True(t, ok)
	assert.Equal(t, 30*time.Minute, got.TrialInterval,
		"Retry-After must be used directly with no cap — server is ground truth")
}

// Validates: R-2.10.43 — full disk:local scope-block lifecycle:
// ErrDiskFull → classifyResult → active scope blocks downloads → trial → release.
func TestDiskLocalScopeBlock_FullCycle(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	// 1. classifyResult maps ErrDiskFull to disk:local scope block.
	decision := classifyResult(&synctypes.WorkerResult{
		Err: fmt.Errorf("download failed: %w", driveops.ErrDiskFull),
	})
	require.Equal(t, resultScopeBlock, decision.Class)
	require.Equal(t, synctypes.SKDiskLocal(), decision.ScopeKey)
	require.Equal(t, recordFailureReconcile, decision.RecordMode)
	assert.False(t, decision.RunScopeDetection, "disk:local uses direct scope activation, not HTTP scope detection")

	// 2. Establish the active scope block.
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKDiskLocal(),
		IssueType:     synctypes.IssueDiskFull,
		BlockedAt:     now,
		TrialInterval: 10 * time.Second,
		NextTrialAt:   now.Add(10 * time.Second),
	})

	// 3. Active-scope admission blocks downloads under disk:local, allows uploads.
	dlAction := &synctypes.TrackedAction{ID: 1, Action: synctypes.Action{Type: synctypes.ActionDownload, Path: "big.zip", DriveID: driveid.New("d"), ItemID: "dl1"}}
	ulAction := &synctypes.TrackedAction{ID: 2, Action: synctypes.Action{Type: synctypes.ActionUpload, Path: "small.txt", DriveID: driveid.New("d"), ItemID: "ul1"}}

	assert.False(t, activeBlockingScopeForTest(t, eng, dlAction).IsZero(), "download should be blocked by disk:local scope")
	assert.True(t, activeBlockingScopeForTest(t, eng, ulAction).IsZero(), "upload should NOT be blocked by disk:local scope")

	// 4. Release scope block (simulating trial success / disk space freed).
	require.NoError(t, releaseTestScope(t, eng, ctx, synctypes.SKDiskLocal()))

	// 5. Download should now be admitted.
	assert.True(t, activeBlockingScopeForTest(t, eng, dlAction).IsZero(), "download should be admitted after scope release")
}

// ---------------------------------------------------------------------------
// deriveScopeKey
// ---------------------------------------------------------------------------

// Validates: R-2.10.2
func TestDeriveScopeKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		httpStatus  int
		shortcutKey string
		want        synctypes.ScopeKey
	}{
		{"429_throttle", 429, "", synctypes.SKThrottleAccount()},
		{"503_service", 503, "", synctypes.SKService()},
		{"507_own", 507, "", synctypes.SKQuotaOwn()},
		{"507_shortcut", 507, "drive1:item1", synctypes.SKQuotaShortcut("drive1:item1")},
		{"500_service", 500, "", synctypes.SKService()},
		{"502_service", 502, "", synctypes.SKService()},
		{"200_empty", 200, "", synctypes.ScopeKey{}},
		{"404_empty", 404, "", synctypes.ScopeKey{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &synctypes.WorkerResult{HTTPStatus: tt.httpStatus, ShortcutKey: tt.shortcutKey}
			assert.Equal(t, tt.want, deriveScopeKey(r))
		})
	}
}

// ---------------------------------------------------------------------------
// applyScopeBlock arms trial timer
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestApplyScopeBlock_ArmsTrialTimer(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	// applyScopeBlock persists the scope and arms the trial timer.
	applyScopeBlockForTest(t, eng, ctx, synctypes.ScopeUpdateResult{
		Block:      true,
		ScopeKey:   synctypes.SKThrottleAccount(),
		IssueType:  synctypes.IssueRateLimited,
		RetryAfter: 30 * time.Second,
	})

	// Verify the block has the correct NextTrialAt from the injectable clock.
	earliest, ok := testWatchRuntime(t, eng).earliestTrialAt()
	require.True(t, ok, "EarliestTrialAt should find the scope block")
	assert.Equal(t, now.Add(30*time.Second), earliest, "NextTrialAt should be now + trial interval")

	// Trial timer should be armed.
	timerSet := testWatchRuntime(t, eng).hasTrialTimer()
	assert.True(t, timerSet, "trial timer should be armed after applyScopeBlock")
}

// ---------------------------------------------------------------------------
// recordFailure populates synctypes.ScopeKey
// ---------------------------------------------------------------------------

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	processWorkerResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		Path:       "quota-fail.txt",
		ActionType: synctypes.ActionUpload,
		Success:    false,
		ErrMsg:     "insufficient storage",
		HTTPStatus: 507,
		ActionID:   1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.SKQuotaOwn(), issues[0].ScopeKey, "507 own-drive should populate scope key")
}

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey_429(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	processWorkerResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		Path:       "throttled.txt",
		ActionType: synctypes.ActionDownload,
		Success:    false,
		ErrMsg:     "too many requests",
		HTTPStatus: 429,
		ActionID:   1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.SKThrottleAccount(), issues[0].ScopeKey)
}

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey_507Shortcut(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	processWorkerResultForTest(t, eng, ctx, &synctypes.WorkerResult{
		Path:        "shared/file.txt",
		ActionType:  synctypes.ActionUpload,
		Success:     false,
		ErrMsg:      "quota exceeded",
		HTTPStatus:  507,
		ShortcutKey: "driveA:item42",
		ActionID:    1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, synctypes.SKQuotaShortcut("driveA:item42"), issues[0].ScopeKey)
}

// ---------------------------------------------------------------------------
// One-shot engine-loop integration tests (single-owner result processing)
// ---------------------------------------------------------------------------

// startDrainLoop creates a real engine with DepGraph, watch-mode scope state,
// readyCh, buf, and retryTimerCh — the full one-shot engine-loop pipeline used
// by these tests. Tests access the ready channel and buffer via the returned
// engine.
func startDrainLoop(t *testing.T) (chan synctypes.WorkerResult, <-chan struct{}, context.CancelFunc, *testEngine) {
	t.Helper()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.scopeState = syncdispatch.NewScopeState(eng.nowFunc, eng.logger)
	rt.buf = syncobserve.NewBuffer(eng.logger)

	results := make(chan synctypes.WorkerResult, 16)

	ctx, cancel := context.WithCancel(t.Context())
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	safety := synctypes.DefaultSafetyConfig()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rt.stopTrialTimer()
		runResultDrainLoopForTest(ctx, rt, bl, safety, results)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return results, done, cancel, eng
}

func runResultDrainLoopForTest(
	ctx context.Context,
	rt *watchRuntime,
	bl *synctypes.Baseline,
	safety *synctypes.SafetyConfig,
	results <-chan synctypes.WorkerResult,
) {
	var outbox []*synctypes.TrackedAction

	for {
		if len(outbox) == 0 {
			nextOutbox, done := runResultDrainLoopIdleForTest(ctx, rt, bl, safety, results)
			outbox = nextOutbox
			if done {
				return
			}
			continue
		}

		nextOutbox, done := runResultDrainLoopWithOutboxForTest(ctx, rt, bl, safety, results, outbox)
		outbox = nextOutbox
		if done {
			return
		}
	}
}

func runResultDrainLoopIdleForTest(
	ctx context.Context,
	rt *watchRuntime,
	bl *synctypes.Baseline,
	safety *synctypes.SafetyConfig,
	results <-chan synctypes.WorkerResult,
) ([]*synctypes.TrackedAction, bool) {
	select {
	case workerResult, ok := <-results:
		if !ok {
			return nil, true
		}
		return appendDrainOutcome(rt, ctx, bl, nil, &workerResult)
	case <-rt.trialTimerChan():
		return rt.runTrialDispatch(ctx, bl, synctypes.SyncBidirectional, safety), false
	case <-rt.retryTimerChan():
		return rt.runRetrierSweep(ctx, bl, synctypes.SyncBidirectional, safety), false
	case <-ctx.Done():
		return nil, true
	}
}

func runResultDrainLoopWithOutboxForTest(
	ctx context.Context,
	rt *watchRuntime,
	bl *synctypes.Baseline,
	safety *synctypes.SafetyConfig,
	results <-chan synctypes.WorkerResult,
	outbox []*synctypes.TrackedAction,
) ([]*synctypes.TrackedAction, bool) {
	select {
	case rt.readyCh <- outbox[0]:
		return outbox[1:], false
	case workerResult, ok := <-results:
		if !ok {
			return outbox, true
		}
		return appendDrainOutcome(rt, ctx, bl, outbox, &workerResult)
	case <-rt.trialTimerChan():
		return append(outbox, rt.runTrialDispatch(ctx, bl, synctypes.SyncBidirectional, safety)...), false
	case <-rt.retryTimerChan():
		return append(outbox, rt.runRetrierSweep(ctx, bl, synctypes.SyncBidirectional, safety)...), false
	case <-ctx.Done():
		return outbox, true
	}
}

func appendDrainOutcome(
	rt *watchRuntime,
	ctx context.Context,
	bl *synctypes.Baseline,
	outbox []*synctypes.TrackedAction,
	workerResult *synctypes.WorkerResult,
) ([]*synctypes.TrackedAction, bool) {
	outcome := rt.processWorkerResult(ctx, rt, workerResult, bl)
	if outcome.terminate {
		return outbox, true
	}

	return append(outbox, outcome.dispatched...), false
}

// readReadyAction reads one synctypes.TrackedAction from the ready channel
// with a 1s timeout.
func readReadyAction(t *testing.T, ready <-chan *synctypes.TrackedAction) *synctypes.TrackedAction {
	t.Helper()

	select {
	case ta := <-ready:
		return ta
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for action on ready channel")
	}

	return nil
}

func readReady(t *testing.T, ready <-chan *synctypes.TrackedAction) {
	t.Helper()
	_ = readReadyAction(t, ready)
}

// Validates: R-2.10.5 — the one-shot engine loop processes results and routes dependents.
func TestE2E_OneShotEngineLoop_ProcessesAndRoutes(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng := startDrainLoop(t)
	defer cancel()

	ctx := t.Context()

	// Add parent action to syncdispatch.DepGraph, send to readyCh.
	ta := testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{Type: synctypes.ActionUpload, Path: "a.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 0, nil)
	require.NotNil(t, ta)
	testWatchRuntime(t, eng).readyCh <- ta
	readReady(t, testWatchRuntime(t, eng).readyCh)

	// Send 429 result — scope detection creates block + records failure.
	results <- synctypes.WorkerResult{
		ActionID:   0,
		Path:       "a.txt",
		ActionType: synctypes.ActionUpload,
		DriveID:    driveid.New(engineTestDriveID),
		Success:    false,
		HTTPStatus: 429,
		RetryAfter: 5 * time.Millisecond,
		ErrMsg:     "rate limited",
		Err:        fmt.Errorf("rate limited"),
	}

	// Verify scope block created and failure recorded.
	require.Eventually(t, func() bool {
		return isTestScopeBlocked(eng, synctypes.SKThrottleAccount())
	}, time.Second, time.Millisecond, "scope block should be created")

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, issues, "failure should be recorded")
}

// Validates: R-2.1.2, R-6.8.9
func TestWatchLoop_SteadyStateContinuesAfterGraphDrains(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	ready := setupWatchEngine(t, eng)
	batches := make(chan []synctypes.PathChanges, 2)
	results := make(chan synctypes.WorkerResult, 2)
	done := make(chan error, 1)

	go func() {
		done <- runWatchLoopForTest(eng, ctx, &watchPipeline{
			bl:      bl,
			safety:  synctypes.DefaultSafetyConfig(),
			ready:   batches,
			results: results,
			mode:    synctypes.SyncBidirectional,
		})
	}()

	batches <- []synctypes.PathChanges{{
		Path: "alpha.txt",
		RemoteEvents: []synctypes.ChangeEvent{{
			Source:  synctypes.SourceRemote,
			Type:    synctypes.ChangeCreate,
			Path:    "alpha.txt",
			DriveID: driveID,
			ItemID:  "alpha-item",
			Hash:    "alpha-hash",
			Size:    10,
		}},
	}}

	first := readReadyAction(t, ready)
	require.Equal(t, "alpha.txt", first.Action.Path)

	results <- synctypes.WorkerResult{
		ActionID:   first.ID,
		Path:       first.Action.Path,
		DriveID:    driveID,
		ActionType: first.Action.Type,
		Success:    true,
	}

	require.Eventually(t, func() bool {
		return testWatchRuntime(t, eng).depGraph.InFlightCount() == 0
	}, time.Second, 10*time.Millisecond, "first batch should drain completely")

	select {
	case err := <-done:
		require.Failf(t, "watch loop exited after graph drained", "err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	batches <- []synctypes.PathChanges{{
		Path: "beta.txt",
		RemoteEvents: []synctypes.ChangeEvent{{
			Source:  synctypes.SourceRemote,
			Type:    synctypes.ChangeCreate,
			Path:    "beta.txt",
			DriveID: driveID,
			ItemID:  "beta-item",
			Hash:    "beta-hash",
			Size:    12,
		}},
	}}

	second := readReadyAction(t, ready)
	require.Equal(t, "beta.txt", second.Action.Path, "steady-state watch loop should keep processing later batches")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "watch loop did not exit after cancellation")
	}
}

// Validates: R-2.10.5, R-2.10.11
// TestE2E_OneShotLoop_TrialResultSuccess verifies that trial success clears the
// scope block and re-injects held failures without waiting for a new external observation.
func TestE2E_OneShotLoop_TrialResultSuccess(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done

	ctx := t.Context()

	// Set up scope block and a scope-blocked failure.
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount(),
		IssueType:     synctypes.IssueRateLimited,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	})
	require.NoError(t, os.WriteFile(filepath.Join(eng.syncRoot, "blocked.txt"), []byte("blocked payload"), 0o600))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "blocked.txt", DriveID: driveid.New(engineTestDriveID), Direction: synctypes.DirectionUpload,
		Role:     synctypes.FailureRoleHeld,
		Category: synctypes.CategoryTransient, ErrMsg: "rate limited", ScopeKey: synctypes.SKThrottleAccount(),
	}, nil))

	// Add trial action to depGraph.
	ta := testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{Type: synctypes.ActionUpload, Path: "trial.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 1, nil)
	require.NotNil(t, ta)

	// Send trial success via results channel.
	results <- synctypes.WorkerResult{
		ActionID:      1,
		Path:          "trial.txt",
		ActionType:    synctypes.ActionUpload,
		DriveID:       driveid.New(engineTestDriveID),
		Success:       true,
		IsTrial:       true,
		TrialScopeKey: synctypes.SKThrottleAccount(),
	}

	// Scope block should be cleared.
	require.Eventually(t, func() bool {
		return !isTestScopeBlocked(eng, synctypes.SKThrottleAccount())
	}, time.Second, time.Millisecond, "scope block should be cleared after trial success")

	var released *synctypes.TrackedAction
	require.Eventually(t, func() bool {
		select {
		case released = <-testWatchRuntime(t, eng).readyCh:
			return released != nil && released.Action.Path == "blocked.txt"
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "trial success should re-dispatch the held failure without external observation")

	require.NotNil(t, released)
	assert.Equal(t, synctypes.ActionUpload, released.Action.Type)
}

// TestE2E_OneShotLoop_TrialResultFailure verifies trial failure doubles the interval.
func TestE2E_OneShotLoop_TrialResultFailure(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	})

	ta := testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{Type: synctypes.ActionDownload, Path: "trial.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 99, nil)
	require.NotNil(t, ta)

	results <- synctypes.WorkerResult{
		ActionID:      99,
		Path:          "trial.txt",
		ActionType:    synctypes.ActionDownload,
		DriveID:       driveid.New(engineTestDriveID),
		Success:       false,
		HTTPStatus:    500,
		ErrMsg:        "internal server error",
		Err:           fmt.Errorf("internal server error"),
		IsTrial:       true,
		TrialScopeKey: synctypes.SKService(),
	}

	// Interval should be doubled from 10ms to 20ms.
	require.Eventually(t, func() bool {
		block, ok := getTestScopeBlock(eng, synctypes.SKService())
		return ok && block.TrialInterval == 20*time.Millisecond
	}, time.Second, time.Millisecond, "trial failure should double interval")
}

func TestE2E_OneShotLoopExit_StopsTimer(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	ctx := t.Context()

	// Create scope block → arms trial timer.
	applyScopeBlockForTest(t, eng, ctx, synctypes.ScopeUpdateResult{
		Block:      true,
		ScopeKey:   synctypes.SKService(),
		IssueType:  synctypes.IssueServiceOutage,
		RetryAfter: time.Hour, // long interval so it doesn't fire during test
	})

	// Verify timer is armed.
	require.Eventually(t, func() bool {
		return testWatchRuntime(t, eng).hasTrialTimer()
	}, time.Second, time.Millisecond)

	// Close results channel → the one-shot loop returns → defer stopTrialTimer.
	close(results)
	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "one-shot engine loop did not exit after results channel close")
	}

	assert.False(t, testWatchRuntime(t, eng).hasTrialTimer(), "drain exit should stop and clear the trial timer")
}

// ---------------------------------------------------------------------------
// Unit tests — trial timing initial intervals and caps (R-2.10.6, R-2.10.7, R-2.10.8)
// ---------------------------------------------------------------------------

func assertScopeWindowBlock(
	t *testing.T,
	httpStatus int,
	threshold int,
	wantScope synctypes.ScopeKey,
	wantIssue string,
) {
	t.Helper()

	clock := controllableClock()
	ss := syncdispatch.NewScopeState(clock, discardLogger())

	for i := range threshold {
		sr := ss.UpdateScope(&synctypes.WorkerResult{
			Path:       fmt.Sprintf("/file-%d.txt", i),
			HTTPStatus: httpStatus,
		})
		if i < threshold-1 {
			assert.False(t, sr.Block, "should not trigger before threshold")
			continue
		}

		require.True(t, sr.Block, "should trigger at threshold")
		assert.Equal(t, wantScope, sr.ScopeKey)
		assert.Equal(t, wantIssue, sr.IssueType)
		assert.Zero(t, sr.RetryAfter, "sliding window trigger should have zero RetryAfter")
	}
}

func assertImmediateRetryAfterBlock(
	t *testing.T,
	httpStatus int,
	retryAfter time.Duration,
	wantScope synctypes.ScopeKey,
	wantIssue string,
) {
	t.Helper()

	clock := controllableClock()
	ss := syncdispatch.NewScopeState(clock, discardLogger())

	sr := ss.UpdateScope(&synctypes.WorkerResult{
		Path:       "/file.txt",
		HTTPStatus: httpStatus,
		RetryAfter: retryAfter,
	})

	require.True(t, sr.Block, "Retry-After should trigger an immediate scope block")
	assert.Equal(t, wantScope, sr.ScopeKey)
	assert.Equal(t, wantIssue, sr.IssueType)
	assert.Equal(t, retryAfter, sr.RetryAfter, "RetryAfter should pass through")
}

// Validates: R-2.10.6
func TestTrialTimer_QuotaStartsAt5s(t *testing.T) {
	t.Parallel()

	assertScopeWindowBlock(t, 507, 3, synctypes.SKQuotaOwn(), "quota_exceeded")
}

// TestTrialTimer_BackoffCapsAt5m is covered by
// TestProcessTrialResultV2_Failure_CapsAt5m which uses active persisted scopes.
// Removed: old test used held-queue mechanism.

// Validates: R-2.10.7
func TestTrialTimer_RateLimited_StartsAtRetryAfter(t *testing.T) {
	t.Parallel()

	assertImmediateRetryAfterBlock(t, 429, 90*time.Second, synctypes.SKThrottleAccount(), "rate_limited")
}

// TestTrialTimer_RateLimited_BlocksAllActionTypes is covered by
// scope_gate_test.go covers the pure active-scope helper functions directly.
// Removed: old test used held-queue mechanism.

// Validates: R-2.10.8
func TestTrialTimer_Service_StartsAt5s(t *testing.T) {
	t.Parallel()

	assertScopeWindowBlock(t, 500, 5, synctypes.SKService(), "service_outage")
}

// Validates: R-2.10.8
func TestTrialTimer_Service_503RetryAfterOverride(t *testing.T) {
	t.Parallel()

	assertImmediateRetryAfterBlock(t, 503, 120*time.Second, synctypes.SKService(), "service_outage")
}

// clearResolvedSkippedItems
// ---------------------------------------------------------------------------

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_AllThreeIssueTypes(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record failures for each scanner-detectable issue type.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "bad\x01name.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueInvalidFilename, Category: synctypes.CategoryActionable, ErrMsg: "invalid character",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "still-bad\x02.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueInvalidFilename, Category: synctypes.CategoryActionable, ErrMsg: "invalid character",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "very/long/path.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssuePathTooLong, Category: synctypes.CategoryActionable, ErrMsg: "path exceeds limit",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "huge-file.bin", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueFileTooLarge, Category: synctypes.CategoryActionable, ErrMsg: "file too large",
	}, nil))

	// Verify all 4 failures exist.
	all, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 4)

	// Simulate a new scan where only "still-bad\x02.txt" still exists as skipped.
	// "bad\x01name.txt" was renamed, "very/long/path.txt" was shortened,
	// "huge-file.bin" was deleted.
	currentSkipped := []synctypes.SkippedItem{
		{Path: "still-bad\x02.txt", Reason: synctypes.IssueInvalidFilename},
	}

	testEngineFlow(t, eng).clearResolvedSkippedItems(ctx, currentSkipped)

	// Only the still-existing invalid filename should remain.
	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "still-bad\x02.txt", remaining[0].Path)
	assert.Equal(t, synctypes.IssueInvalidFilename, remaining[0].IssueType)
}

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_EmptySkipped_ClearsAll(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record one failure per type.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "bad.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueInvalidFilename, Category: synctypes.CategoryActionable, ErrMsg: "invalid",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "long.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssuePathTooLong, Category: synctypes.CategoryActionable, ErrMsg: "too long",
	}, nil))

	// Empty scan — all problematic files were resolved.
	testEngineFlow(t, eng).clearResolvedSkippedItems(ctx, nil)

	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "all scanner-detectable failures should be cleared when no skipped items remain")
}

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_DoesNotAffectRuntimeIssues(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record a scanner-detectable failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "bad.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueInvalidFilename, Category: synctypes.CategoryActionable, ErrMsg: "invalid",
	}, nil))

	// Record a runtime failure (permission denied — not scanner-detectable).
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "Shared/folder", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssuePermissionDenied, Category: synctypes.CategoryActionable, ErrMsg: "read-only",
		HTTPStatus: 403,
	}, nil))

	// Clear all scanner-detectable items (empty = all resolved).
	testEngineFlow(t, eng).clearResolvedSkippedItems(ctx, nil)

	// Runtime failure should survive — clearResolvedSkippedItems only
	// clears invalid_filename, path_too_long, file_too_large.
	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, synctypes.IssuePermissionDenied, remaining[0].IssueType)
}

// Validates: R-2.12.2
func TestClearResolvedSkippedItems_CaseCollision(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record case collision failures.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "File.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueCaseCollision, Category: synctypes.CategoryActionable,
		ErrMsg: "conflicts with file.txt",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "file.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		IssueType: synctypes.IssueCaseCollision, Category: synctypes.CategoryActionable,
		ErrMsg: "conflicts with File.txt",
	}, nil))

	// Verify both exist.
	all, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)

	// Simulate user renaming one collider — next scan finds zero case collisions.
	testEngineFlow(t, eng).clearResolvedSkippedItems(ctx, nil)

	// Both case collision entries should be cleared.
	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "case collision failures should be auto-cleared when resolved")
}

// ---------------------------------------------------------------------------
// feedScopeDetection guard: local errors must not feed scope windows
// ---------------------------------------------------------------------------

// Validates: R-6.7.27
func TestFeedScopeDetection_LocalErrorIgnored(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	testWatchRuntime(t, eng).scopeState = syncdispatch.NewScopeState(time.Now, eng.logger)

	// Feed several local errors (HTTPStatus=0) — should not trigger a scope block.
	for i := range 10 {
		feedScopeDetectionForTest(t, eng, t.Context(), &synctypes.WorkerResult{
			Path:       fmt.Sprintf("file-%d.txt", i),
			ActionType: synctypes.ActionDownload,
			HTTPStatus: 0, // local error — no HTTP status
			Err:        os.ErrPermission,
			ErrMsg:     "permission denied",
		})
	}

	// No scope block should have been created.
	assert.False(t, isTestScopeBlocked(eng, synctypes.SKService()),
		"local errors with HTTPStatus=0 must not trigger service scope")
	assert.False(t, isTestScopeBlocked(eng, synctypes.SKThrottleAccount()),
		"local errors with HTTPStatus=0 must not trigger throttle scope")
}

// Validates: R-2.10.30
func TestIsObservationSuppressed_Throttled(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)

	// Initially not suppressed.
	assert.False(t, isObservationSuppressedForTest(t, eng, testWatchRuntime(t, eng)))

	// After throttle block, should be suppressed.
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKThrottleAccount(),
		IssueType:     synctypes.IssueRateLimited,
		TrialInterval: 30 * time.Second,
	})
	assert.True(t, isObservationSuppressedForTest(t, eng, testWatchRuntime(t, eng)))
}

// Validates: R-2.10.30
func TestIsObservationSuppressed_ServiceOutage(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)

	// Service outage should also suppress.
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		TrialInterval: 60 * time.Second,
	})
	assert.True(t, isObservationSuppressedForTest(t, eng, testWatchRuntime(t, eng)))
}

// Validates: R-2.10.30
func TestIsObservationSuppressed_OneShotMode_NoWatchState(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})

	// With nil watch (one-shot mode), should not panic and should return false.
	_, ok := lookupTestWatchRuntime(eng)
	assert.False(t, ok, "watch runtime should be nil after NewEngine")
	assert.False(t, isObservationSuppressedForTest(t, eng, nil))
}

// Validates: R-2.10.30, R-2.10.31
func TestIsObservationSuppressed_QuotaDoesNotSuppress(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)

	// Quota scope block should NOT suppress observation (R-2.10.31).
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKQuotaOwn(),
		IssueType:     synctypes.IssueQuotaExceeded,
		TrialInterval: 5 * time.Minute,
	})
	assert.False(t, isObservationSuppressedForTest(t, eng, testWatchRuntime(t, eng)))
}

// ---------------------------------------------------------------------------
// watch runtime absence invariant
// ---------------------------------------------------------------------------

func TestWatchState_NilInOneShotMode(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// After construction, the engine should not retain a watch runtime.
	_, ok := lookupTestWatchRuntime(eng)
	assert.False(t, ok, "watch runtime should be absent after NewEngine")

	// After RunOnce, watch state must still be nil.
	report, err := eng.RunOnce(ctx, synctypes.SyncDownloadOnly, synctypes.RunOpts{})
	require.NoError(t, err)
	assert.NotNil(t, report)
	_, ok = lookupTestWatchRuntime(eng)
	assert.False(t, ok, "watch runtime should still be absent after RunOnce")
}

// ---------------------------------------------------------------------------
// issueTypeForHTTPStatus — maps HTTP status to issue type (R-6.6.10)
// ---------------------------------------------------------------------------

// Validates: R-6.6.10
func TestIssueTypeForHTTPStatus(t *testing.T) {
	t.Parallel()

	genericInvalidRequestErr := &graph.GraphError{
		StatusCode: http.StatusBadRequest,
		Code:       "badRequest",
		InnerCodes: []string{"invalidRequest"},
		Message:    "Invalid request",
		Err:        graph.ErrBadRequest,
	}
	objectHandleErr := &graph.GraphError{
		StatusCode: http.StatusBadRequest,
		Message:    "ObjectHandle is Invalid for operation",
		Err:        graph.ErrBadRequest,
	}

	tests := []struct {
		name       string
		httpStatus int
		err        error
		want       string
	}{
		{"429_rate_limited", http.StatusTooManyRequests, nil, synctypes.IssueRateLimited},
		{"401_unauthorized", http.StatusUnauthorized, graph.ErrUnauthorized, synctypes.IssueUnauthorized},
		{"507_quota_exceeded", http.StatusInsufficientStorage, nil, synctypes.IssueQuotaExceeded},
		{"403_permission_denied", http.StatusForbidden, nil, synctypes.IssuePermissionDenied},
		{"400_invalid_request", http.StatusBadRequest, genericInvalidRequestErr, ""},
		{"400_object_handle_message_only", http.StatusBadRequest, objectHandleErr, ""},
		{"400_normal", http.StatusBadRequest, errors.New("bad request"), ""},
		{"500_service_outage", http.StatusInternalServerError, nil, synctypes.IssueServiceOutage},
		{"503_service_outage", http.StatusServiceUnavailable, nil, synctypes.IssueServiceOutage},
		{"408_request_timeout", http.StatusRequestTimeout, nil, "request_timeout"},
		{"412_transient_conflict", http.StatusPreconditionFailed, nil, "transient_conflict"},
		{"404_transient_not_found", http.StatusNotFound, nil, "transient_not_found"},
		{"423_resource_locked", http.StatusLocked, nil, "resource_locked"},
		{"permission_error", 0, os.ErrPermission, synctypes.IssueLocalPermissionDenied},
		// Validates: R-2.10.43
		{"disk_full", 0, driveops.ErrDiskFull, synctypes.IssueDiskFull},
		{"wrapped_disk_full", 0, fmt.Errorf("download: %w", driveops.ErrDiskFull), synctypes.IssueDiskFull},
		// Validates: R-2.10.44
		{"file_too_large_for_space", 0, driveops.ErrFileTooLargeForSpace, synctypes.IssueFileTooLargeForSpace},
		{"file_exceeds_onedrive_limit", 0, driveops.ErrFileExceedsOneDriveLimit, synctypes.IssueFileTooLarge},
		{"unknown_status", 418, nil, ""},
		{"zero_status_no_error", 0, nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := issueTypeForHTTPStatus(tt.httpStatus, tt.err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// logFailureSummary — aggregated failure logging (R-6.6.12)
// ---------------------------------------------------------------------------

// Validates: R-6.6.12
func TestLogFailureSummary_AggregatesAboveThreshold(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := newEngineFlow(eng.Engine)

	// Add 15 errors with the same message prefix — should aggregate.
	for i := range 15 {
		flow.syncErrors = append(flow.syncErrors, fmt.Errorf("quota_exceeded: upload failed for file %d", i))
	}

	// Should not panic; clears syncErrors after logging.
	flow.logFailureSummary()

	assert.Empty(t, flow.syncErrors, "syncErrors should be cleared after summary")
}

// Validates: R-6.6.12
func TestLogFailureSummary_NoErrors(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := newEngineFlow(eng.Engine)

	// Should be a no-op with no errors.
	flow.logFailureSummary()

	assert.Empty(t, flow.syncErrors)
}

// ---------------------------------------------------------------------------
// Retrier pipeline integration test (single-owner architecture)
//
// Exercises the integrated retrier: action → failure → sync_failures
// → retry timer fires → runRetrierSweep → createEventFromDB → syncobserve.Buffer.
// ---------------------------------------------------------------------------

// Validates: R-6.8.10, R-6.8.11, R-6.8.7
func TestRetryPipeline_TransientFailure_IntegratedRetrier(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	testPath := "docs/report.pdf"

	// Seed remote_state so createEventFromDB can build a full event when
	// the retrier sweep processes this failure.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-abc",
			Path:     testPath,
			ItemType: synctypes.ItemTypeFile,
			Hash:     "report-hash",
			Size:     4096,
		},
	}, "", driveID))

	// Add action to depGraph, send to readyCh, drain it.
	ta := testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
		Type: synctypes.ActionDownload, Path: testPath, DriveID: driveID, ItemID: "item-abc",
	}, 0, nil)
	require.NotNil(t, ta)
	testWatchRuntime(t, eng).readyCh <- ta
	readReady(t, testWatchRuntime(t, eng).readyCh)

	// Use a nowFn that's 1 hour in the future so retrier sees rows as due.
	futureTime := time.Now().Add(time.Hour)
	eng.nowFn = func() time.Time { return futureTime }

	// Send a 503 result — classifies as resultRequeue (transient).
	results <- synctypes.WorkerResult{
		ActionID:   0,
		Path:       testPath,
		ActionType: synctypes.ActionDownload,
		DriveID:    driveID,
		Success:    false,
		HTTPStatus: http.StatusServiceUnavailable,
		ErrMsg:     "service unavailable",
		Err:        fmt.Errorf("service unavailable"),
	}

	// Verify: sync_failures row created.
	require.Eventually(t, func() bool {
		rows, err := eng.baseline.ListSyncFailures(ctx)
		return err == nil && len(rows) == 1
	}, time.Second, time.Millisecond, "sync_failures row should be created")

	// Trigger retrier sweep manually (in production this fires from retry timer).
	outbox := runTestRetrierSweep(t, eng, ctx)

	require.Len(t, outbox, 1, "retrier should dispatch one action through the planner path")
	assert.Equal(t, testPath, outbox[0].Action.Path)
	assert.Equal(t, synctypes.ActionDownload, outbox[0].Action.Type)
}

// Validates: R-6.8.10
func TestOneShotEngineLoop_Success_ClearsSyncFailure(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	testPath := "docs/stale-failure.txt"

	// Seed a sync_failures row — simulates a previous transient failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: testPath, DriveID: driveID, Direction: synctypes.DirectionDownload,
		Category: synctypes.CategoryTransient, ErrMsg: "previous failure",
		HTTPStatus: http.StatusServiceUnavailable,
	}, func(int) time.Duration { return time.Hour }))

	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "seeded failure should exist")

	// Add action, send to readyCh, drain it.
	ta := testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
		Type: synctypes.ActionDownload, Path: testPath, DriveID: driveID, ItemID: "item-ok",
	}, 0, nil)
	require.NotNil(t, ta)
	testWatchRuntime(t, eng).readyCh <- ta
	readReady(t, testWatchRuntime(t, eng).readyCh)

	// Send a success result — defensive clear removes the row.
	results <- synctypes.WorkerResult{
		ActionID: 0, Path: testPath, ActionType: synctypes.ActionDownload,
		DriveID: driveID, Success: true,
	}

	// Verify: sync_failures row cleared.
	require.Eventually(t, func() bool {
		rows, err := eng.baseline.ListSyncFailures(ctx)
		return err == nil && len(rows) == 0
	}, time.Second, time.Millisecond, "sync_failures row should be cleared on success")

	assert.Equal(t, 1, testEngineFlow(t, eng).succeeded, "succeeded counter")
}

// ---------------------------------------------------------------------------
// clearFailureOnSuccess unit tests (D-6)
// ---------------------------------------------------------------------------

// Validates: D-6
func TestClearFailureOnSuccess_RemovesFailureRow(t *testing.T) {
	// Verify that clearFailureOnSuccess removes a previously recorded
	// sync_failures row, confirming the engine-owns-failure-lifecycle
	// contract from D-6.
	ctx := context.Background()
	eng, _ := newTestEngine(t, &engineMockClient{})
	driveID := driveid.New(engineTestDriveID)

	// Record a failure for the test path.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "clear-test/file.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Category:  synctypes.CategoryTransient,
		ErrMsg:    "test error",
	}, nil))

	// Confirm the failure exists.
	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "failure should be recorded")

	// clearFailureOnSuccess should remove it.
	flow := newEngineFlow(eng.Engine)
	flow.clearFailureOnSuccess(ctx, &synctypes.WorkerResult{
		Path:       "clear-test/file.txt",
		DriveID:    driveID,
		ActionType: synctypes.ActionDownload,
		Success:    true,
	})

	// Verify the failure is gone.
	rows, err = eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "failure should be cleared after success")
}

// Validates: D-6
func TestClearFailureOnSuccess_FallbackDriveID(t *testing.T) {
	// When synctypes.WorkerResult.DriveID is zero, clearFailureOnSuccess falls back
	// to the engine's own driveID. This covers own-drive actions where the
	// worker doesn't set an explicit drive ID.
	ctx := context.Background()
	eng, _ := newTestEngine(t, &engineMockClient{})
	driveID := driveid.New(engineTestDriveID)

	// Record a failure using the engine's own drive ID.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "fallback-test/file.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
		Category:  synctypes.CategoryTransient,
		ErrMsg:    "quota exceeded",
	}, nil))

	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "failure should be recorded")

	// Call clearFailureOnSuccess with a zero DriveID — should fall back
	// to eng.driveID and still clear the failure.
	flow := newEngineFlow(eng.Engine)
	flow.clearFailureOnSuccess(ctx, &synctypes.WorkerResult{
		Path:       "fallback-test/file.txt",
		DriveID:    driveid.ID{}, // zero value
		ActionType: synctypes.ActionUpload,
		Success:    true,
	})

	rows, err = eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "failure should be cleared via fallback drive ID")
}

// ---------------------------------------------------------------------------
// waitForQuiescence
// ---------------------------------------------------------------------------

func TestWaitForQuiescence_EmptyGraph(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	setupWatchEngine(t, eng)

	ctx := t.Context()

	// Empty graph — should return immediately.
	err := waitForQuiescenceForTest(t, eng, ctx)
	require.NoError(t, err)
}

func TestWaitForQuiescence_ContextCancel(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	setupWatchEngine(t, eng)

	// Add an action that will never complete — quiescence depends on cancel.
	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
		Type: synctypes.ActionDownload, Path: "stuck.txt",
		DriveID: driveid.New(engineTestDriveID), ItemID: "stuck-item",
	}, 1, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	err := waitForQuiescenceForTest(t, eng, ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// bootstrapSync
// ---------------------------------------------------------------------------

func TestBootstrapSync_NoChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Set up watch infrastructure manually (simulating initWatchInfra).
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.scopeState = syncdispatch.NewScopeState(eng.nowFunc, eng.logger)

	neverDone := make(chan struct{})
	pool := syncexec.NewWorkerPool(eng.execCfg, rt.readyCh, neverDone, eng.baseline, eng.logger, 1024)
	pool.Start(ctx, 1)
	defer pool.Stop()

	pipe := &watchPipeline{
		safety:  synctypes.DefaultSafetyConfig(),
		pool:    pool,
		results: pool.Results(),
		mode:    synctypes.SyncBidirectional,
	}

	// No changes expected — bootstrapSync should return nil.
	err := bootstrapSyncForTest(t, eng, ctx, synctypes.SyncBidirectional, pipe)
	require.NoError(t, err)
}

func TestBootstrapSync_WithChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "item-1",
					Name:         "newfile.txt",
					DriveID:      driveID,
					ParentID:     "root",
					Size:         10,
					QuickXorHash: "hash1",
				},
			}, "token-2"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Set up watch infrastructure manually.
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.scopeState = syncdispatch.NewScopeState(eng.nowFunc, eng.logger)

	neverDone := make(chan struct{})
	pool := syncexec.NewWorkerPool(eng.execCfg, rt.readyCh, neverDone, eng.baseline, eng.logger, 1024)
	pool.Start(ctx, 2)
	defer pool.Stop()

	pipe := &watchPipeline{
		safety:  synctypes.DefaultSafetyConfig(),
		pool:    pool,
		results: pool.Results(),
		mode:    synctypes.SyncDownloadOnly,
	}

	err := bootstrapSyncForTest(t, eng, ctx, synctypes.SyncDownloadOnly, pipe)
	require.NoError(t, err)

	// Verify the file was downloaded.
	_, statErr := os.Stat(filepath.Join(syncRoot, "newfile.txt"))
	require.NoError(t, statErr, "newfile.txt should have been downloaded")

	// syncdispatch.DepGraph should be empty — all actions completed.
	assert.Equal(t, 0, rt.depGraph.InFlightCount())
}

// Validates: R-2.10.41
func TestBootstrapSync_CrashRecovery_MixedDeletingCandidates(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	writeLocalFile(t, syncRoot, "exists.txt", "still here")

	now := time.Now().Unix()
	_, err := eng.baseline.DB().ExecContext(ctx, `
		INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, 'gone', '/gone.txt', 'file', 'deleting', ?),
		       (?, 'exists', '/exists.txt', 'file', 'deleting', ?),
		       (?, 'bad', '/../bad.txt', 'file', 'deleting', ?)`,
		engineTestDriveID, now,
		engineTestDriveID, now,
		engineTestDriveID, now,
	)
	require.NoError(t, err, "seed crash-recovery rows")

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.scopeState = syncdispatch.NewScopeState(eng.nowFunc, eng.logger)

	neverDone := make(chan struct{})
	pool := syncexec.NewWorkerPool(eng.execCfg, rt.readyCh, neverDone, eng.baseline, eng.logger, 1024)
	pool.Start(ctx, 1)
	defer pool.Stop()

	pipe := &watchPipeline{
		safety:  synctypes.DefaultSafetyConfig(),
		pool:    pool,
		results: pool.Results(),
		mode:    synctypes.SyncBidirectional,
	}

	err = bootstrapSyncForTest(t, eng, ctx, synctypes.SyncBidirectional, pipe)
	require.NoError(t, err)

	var goneStatus synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'gone'`).Scan(&goneStatus)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusDeleted, goneStatus)

	var existsStatus synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'exists'`).Scan(&existsStatus)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusPendingDelete, existsStatus)

	var badStatus synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'bad'`).Scan(&badStatus)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusPendingDelete, badStatus)
}
