package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	// The upload should NOT have been attempted — caught at observation time.
	assert.Equal(t, 0, report.Uploads, "path-too-long file should not reach planner")

	// The sync_failures table should have an entry from recordSkippedItems.
	issues, issErr := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, issErr)
	require.NotEmpty(t, issues, "sync_failures should have an entry for path too long")

	found := false
	for _, iss := range issues {
		if iss.IssueType == IssuePathTooLong {
			found = true

			break
		}
	}

	assert.True(t, found, "expected IssuePathTooLong issue in sync_failures")
}

// Validates: R-6.6.7
func TestRecordSkippedItems_AggregatesWarningsAndKeepsPerItemDebugLogs(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := newEngineFlow(eng.Engine)
	ctx := t.Context()

	skipped := make([]SkippedItem, 11)
	for i := range skipped {
		skipped[i] = SkippedItem{
			Path:   fmt.Sprintf("bad-%02d.txt", i),
			Reason: IssueInvalidFilename,
			Detail: "invalid filename",
		}
	}

	flow.recordSkippedItems(ctx, skipped)

	output := logBuf.String()
	assert.Equal(t, 1, strings.Count(output, "level=WARN msg=\"observation filter: skipped files\""))
	assert.Equal(t, 11, strings.Count(output, "level=DEBUG msg=\"observation filter: skipped file\""))
	assert.Contains(t, output, "count=11")
	assert.Contains(t, output, "bad-00.txt")
	assert.Contains(t, output, "bad-10.txt")

	rows := syncFailuresByIssueTypeForTest(t, eng.baseline, ctx, IssueInvalidFilename)
	assert.Len(t, rows, 11)
}

// Validates: R-6.6.7
func TestRecordSkippedItems_BelowThresholdLogsPerItemWarningsOnly(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := newEngineFlow(eng.Engine)
	ctx := t.Context()

	skipped := []SkippedItem{
		{
			Path:   "bad-a.txt",
			Reason: IssueInvalidFilename,
			Detail: "invalid filename",
		},
		{
			Path:   "bad-b.txt",
			Reason: IssueInvalidFilename,
			Detail: "invalid filename",
		},
	}

	flow.recordSkippedItems(ctx, skipped)

	output := logBuf.String()
	assert.Equal(t, 2, strings.Count(output, "level=WARN msg=\"observation filter: skipped file\""))
	assert.Equal(t, 0, strings.Count(output, "level=DEBUG msg=\"observation filter: skipped file\""))
	assert.NotContains(t, output, "level=WARN msg=\"observation filter: skipped files\"")

	rows := syncFailuresByIssueTypeForTest(t, eng.baseline, ctx, IssueInvalidFilename)
	assert.Len(t, rows, 2)
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
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction, 16)
	for _, id := range []int64{1, 2, 3} {
		runner.depGraph.Add(&Action{Path: fmt.Sprintf("action-%d", id), Type: ActionUpload}, id, nil)
	}

	results := make(chan ActionCompletion, 3)
	results <- ActionCompletion{Path: "a.txt", ActionType: ActionUpload, Success: false, ErrMsg: "fail1", HTTPStatus: 500, ActionID: 1}
	results <- ActionCompletion{Path: "b.txt", ActionType: ActionUpload, Success: false, ErrMsg: "fail2", HTTPStatus: 500, ActionID: 2}
	results <- ActionCompletion{Path: "c.txt", ActionType: ActionDownload, Success: true, ActionID: 3}
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
	runner.depGraph = NewDepGraph(eng.logger)
	runner.dispatchCh = make(chan *TrackedAction)

	runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "root.txt",
	}, 1, nil)
	runner.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "child.txt",
	}, 2, []int64{1})
	runner.depGraph.Add(&Action{
		Type: ActionDownload,
		Path: "auth.txt",
	}, 3, nil)

	results := make(chan ActionCompletion, 2)
	results <- ActionCompletion{
		ActionID:   1,
		Path:       "root.txt",
		ActionType: ActionUpload,
		Success:    true,
	}
	results <- ActionCompletion{
		ActionID:   3,
		Path:       "auth.txt",
		ActionType: ActionDownload,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	}
	close(results)

	err := runner.runResultsLoop(ctx, nil, nil, results)
	require.ErrorIs(t, err, graph.ErrUnauthorized)
	assert.Equal(t, 0, runner.depGraph.InFlightCount(), "fatal termination should drain queued ready actions as shutdown")

	assert.Equal(t, authstate.ReasonSyncAuthRejected, loadCatalogAuthRequirementForTest(t, eng))

	failures, failureErr := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, failureErr)
	assert.Empty(t, failures, "fatal unauthorized should not create per-path sync_failures rows")
}

func assertUnauthorizedWatchHandlerStopsLoop(
	t *testing.T,
	handler func(*watchRuntime, context.Context, *watchPipeline, *ActionCompletion) ([]*TrackedAction, bool, error),
) {
	t.Helper()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	rt := testWatchRuntime(t, eng)
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "auth.txt",
		DriveID: eng.driveID,
		ItemID:  "item-1",
	}, 21, nil)

	_, done, gotErr := handler(rt, ctx, &watchPipeline{bl: bl}, &ActionCompletion{
		ActionID:   21,
		Path:       "auth.txt",
		DriveID:    eng.driveID,
		ActionType: ActionDownload,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	})

	assert.False(t, done)
	require.ErrorIs(t, gotErr, graph.ErrUnauthorized)
}

// Validates: R-2.10.5
func TestHandleBootstrapCompletion_UnauthorizedStopsBootstrap(t *testing.T) {
	t.Parallel()

	assertUnauthorizedWatchHandlerStopsLoop(t, func(
		rt *watchRuntime,
		ctx context.Context,
		p *watchPipeline,
		workerResult *ActionCompletion,
	) ([]*TrackedAction, bool, error) {
		return rt.handleBootstrapCompletion(ctx, p, nil, workerResult, true)
	})
}

// Validates: R-6.8.16, R-6.6.11
func TestRecordFailure_LogsSummaryKey(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)

	result := &ActionCompletion{
		ActionID:   123,
		Path:       "service.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusServiceUnavailable,
		Err:        graph.ErrServerError,
		ErrMsg:     "service unavailable",
	}
	decision := classifyResult(result)
	require.Equal(t, SummaryServiceOutage, decision.SummaryKey)

	eng.flow.recordFailure(t.Context(), &decision, result, func(_ int) time.Duration {
		return time.Second
	})

	output := logBuf.String()
	assert.Contains(t, output, "run_id=run-")
	assert.Contains(t, output, "action_id=123")
	assert.Contains(t, output, "summary_key=service_outage")
	assert.Contains(t, output, "failure_class=\"retryable transient\"")
	assert.Contains(t, output, "log_owner=sync")
	assert.Contains(t, output, "issue_type=service_outage")
}

func readDriveStatusSnapshotForTest(t *testing.T, eng *testEngine, ctx context.Context) DriveStatusSnapshot {
	t.Helper()

	require.NoError(t, eng.baseline.Checkpoint(ctx, 0))

	inspector, err := openStoreInspector(syncStorePathForTest(t, ctx, eng), testLogger(t))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, inspector.Close())
	}()

	snapshot, err := inspector.ReadDriveStatusSnapshot(ctx, false)
	require.NoError(t, err)

	return snapshot
}

func requireIssueGroupSummaryKey(
	t *testing.T,
	snapshot *DriveStatusSnapshot,
	key SummaryKey,
) IssueGroupSnapshot {
	t.Helper()

	require.NotNil(t, snapshot)

	for i := range snapshot.IssueGroups {
		if snapshot.IssueGroups[i].SummaryKey == key {
			return snapshot.IssueGroups[i]
		}
	}

	require.FailNowf(t, "missing issue group", "summary key %q not found", key)
	return IssueGroupSnapshot{}
}

func findIssueGroupSummaryKey(snapshot *DriveStatusSnapshot, key SummaryKey) (IssueGroupSnapshot, bool) {
	if snapshot == nil {
		return IssueGroupSnapshot{}, false
	}

	for i := range snapshot.IssueGroups {
		if snapshot.IssueGroups[i].SummaryKey == key {
			return snapshot.IssueGroups[i], true
		}
	}

	return IssueGroupSnapshot{}, false
}

// Validates: R-6.8.16, R-6.6.11
func TestProcessActionCompletion_EndToEndSummaryKey_ServiceOutage(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	processActionCompletionForTest(t, eng, ctx, &ActionCompletion{
		ActionID:   1,
		Path:       "service.txt",
		ActionType: ActionUpload,
		Success:    false,
		HTTPStatus: http.StatusServiceUnavailable,
		Err:        graph.ErrServerError,
		ErrMsg:     "service unavailable",
	}, nil)

	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, IssueServiceOutage, rows[0].IssueType)
	assert.Equal(t, CategoryTransient, rows[0].Category)
	assert.Equal(t, FailureRoleItem, rows[0].Role)
	assert.Equal(t, SKService(), rows[0].ScopeKey)
	assert.Equal(t, SummaryServiceOutage,
		SummaryKeyForPersistedFailure(rows[0].IssueType, rows[0].Category, rows[0].Role))

	output := logBuf.String()
	assert.Contains(t, output, "run_id=run-")
	assert.Contains(t, output, "action_id=1")
	assert.Contains(t, output, "summary_key=service_outage")
	assert.Contains(t, output, "failure_class=\"retryable transient\"")
	assert.Contains(t, output, "log_owner=sync")
	assert.Contains(t, output, "issue_type="+IssueServiceOutage)
}

// Validates: R-6.8.16, R-6.6.11
// Validates: R-6.8.16, R-6.6.11
func TestProcessActionCompletion_EndToEndSummaryKey_AuthenticationRequired(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	outcome := processActionCompletionDetailedForTest(t, eng, ctx, &ActionCompletion{
		ActionID:   1,
		Path:       "auth.txt",
		ActionType: ActionDownload,
		Success:    false,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	}, nil)
	require.True(t, outcome.terminate)
	require.ErrorIs(t, outcome.terminateErr, graph.ErrUnauthorized)

	assert.Equal(t, authstate.ReasonSyncAuthRejected, loadCatalogAuthRequirementForTest(t, eng))

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)

	snapshot := readDriveStatusSnapshotForTest(t, eng, ctx)
	_, found := findIssueGroupSummaryKey(&snapshot, SummaryAuthenticationRequired)
	assert.False(t, found)

	output := logBuf.String()
	assert.Contains(t, output, "run_id=run-")
	assert.Contains(t, output, "summary_key=authentication_required")
	assert.Contains(t, output, "failure_class=fatal")
	assert.Contains(t, output, "log_owner=sync")
	assert.NotContains(t, output, "scope_key=")
}

// Validates: R-6.8.16, R-6.6.11
func TestProcessActionCompletion_EndToEndSummaryKey_LocalPermissionDenied(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	processActionCompletionForTest(t, eng, ctx, &ActionCompletion{
		ActionID:   1,
		Path:       "file.txt",
		ActionType: ActionUpload,
		Success:    false,
		Err:        os.ErrPermission,
		ErrMsg:     "permission denied",
	}, nil)

	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, IssueLocalPermissionDenied, rows[0].IssueType)
	assert.Equal(t, CategoryActionable, rows[0].Category)
	assert.Equal(t, FailureRoleItem, rows[0].Role)

	snapshot := readDriveStatusSnapshotForTest(t, eng, ctx)
	group := requireIssueGroupSummaryKey(t, &snapshot, SummaryLocalPermissionDenied)
	assert.Equal(t, 1, group.Count)
	assert.Equal(t, []string{"file.txt"}, group.Paths)

	output := logBuf.String()
	assert.Contains(t, output, "run_id=run-")
	assert.Contains(t, output, "summary_key=local_permission_denied")
	assert.Contains(t, output, "failure_class=actionable")
	assert.Contains(t, output, "log_owner=sync")
	assert.Contains(t, output, "issue_type="+IssueLocalPermissionDenied)
}

// Validates: R-2.10.5
func TestHandleWatchCompletion_UnauthorizedStopsWatchLoop(t *testing.T) {
	t.Parallel()

	assertUnauthorizedWatchHandlerStopsLoop(t, func(
		rt *watchRuntime,
		ctx context.Context,
		p *watchPipeline,
		workerResult *ActionCompletion,
	) ([]*TrackedAction, bool, error) {
		return rt.handleWatchCompletion(ctx, p, nil, workerResult, true)
	})
}

// ---------------------------------------------------------------------------
// processActionCompletion — shared helper tests
// ---------------------------------------------------------------------------

// setupEngineDepGraph creates a DepGraph on the engine and adds a dummy action
// for the given actionID so that processActionCompletion can call Complete without
// panicking on nil depGraph or unknown ID.
func setupEngineDepGraph(t *testing.T, eng *testEngine, actionID int64) {
	t.Helper()

	flow := newEngineFlow(eng.Engine)
	flow.depGraph = NewDepGraph(eng.logger)
	flow.dispatchCh = make(chan *TrackedAction, 16)
	dummyAction := &Action{Path: "dummy", Type: ActionDownload}
	flow.depGraph.Add(dummyAction, actionID, nil)
	eng.flow = &flow
}

func TestProcessActionCompletion_UploadFailure_RecordsLocalIssue(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 0)

	processActionCompletionForTest(t, eng, ctx, &ActionCompletion{
		Path:       "docs/report.xlsx",
		ActionType: ActionUpload,
		Success:    false,
		ErrMsg:     "connection reset",
		HTTPStatus: 503,
	}, nil)

	// Should record upload failure in sync_failures.
	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "docs/report.xlsx", issues[0].Path)
	assert.Equal(t, DirectionUpload, issues[0].Direction)
	assert.Equal(t, "connection reset", issues[0].LastError)
	assert.Equal(t, 503, issues[0].HTTPStatus)
}

func TestProcessActionCompletion_Success_NoRecords(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 0)

	processActionCompletionForTest(t, eng, ctx, &ActionCompletion{
		Path:       "docs/report.xlsx",
		ActionType: ActionDownload,
		Success:    true,
	}, nil)

	// No failures should be recorded.
	failed, err := eng.baseline.ListRemoteState(ctx)
	require.NoError(t, err)
	assert.Empty(t, failed)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

// Validates: R-2.10.5
func TestProcessActionCompletion_UnauthorizedTerminatesRouting(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "auth.txt",
		DriveID: driveid.New("d"),
		ItemID:  "item-1",
	}, 17, nil)

	outcome := processActionCompletionDetailedForTest(t, eng, ctx, &ActionCompletion{
		ActionID:   17,
		Path:       "auth.txt",
		ActionType: ActionDownload,
		HTTPStatus: http.StatusUnauthorized,
		Err:        graph.ErrUnauthorized,
		ErrMsg:     "unauthorized",
	}, nil)

	require.ErrorIs(t, outcome.terminateErr, graph.ErrUnauthorized)
	assert.True(t, outcome.terminate)

	assert.Equal(t, authstate.ReasonSyncAuthRejected, loadCatalogAuthRequirementForTest(t, eng))

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "fatal unauthorized should not create per-path sync_failures rows")
}

// ---------------------------------------------------------------------------
// classifyResult — pure classification of ActionCompletion (R-6.8.15)
// ---------------------------------------------------------------------------

// Validates: R-6.8.15, R-6.7.12
type classifyResultCase struct {
	name              string
	result            ActionCompletion
	wantClass         errclass.Class
	wantScope         ScopeKey
	wantSummaryKey    SummaryKey
	wantPersistence   resultPersistenceMode
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
			assert.Equal(t, tt.wantSummaryKey, got.SummaryKey, "summary key mismatch")
			assert.Equal(t, tt.wantPersistence, got.Persistence, "persistence mismatch")
			assert.Equal(t, tt.wantPermission, got.PermissionFlow, "permission flow mismatch")
			assert.Equal(t, tt.wantScopeDetect, got.RunScopeDetection, "scope detection mismatch")
			assert.Equal(t, tt.wantRecordSuccess, got.RecordSuccess, "record success mismatch")
		})
	}
}

func TestClassifyResult_LifecycleAndAuth(t *testing.T) {
	t.Parallel()

	assertClassifyResultCases(t, []classifyResultCase{
		{name: "success", result: ActionCompletion{Success: true}, wantClass: resultSuccess, wantRecordSuccess: true},
		{name: "context_canceled", result: ActionCompletion{Err: context.Canceled}, wantClass: resultShutdown},
		{name: "context_deadline_exceeded", result: ActionCompletion{Err: context.DeadlineExceeded}, wantClass: resultShutdown},
		{
			name:      "wrapped_context_canceled",
			result:    ActionCompletion{Err: fmt.Errorf("operation failed: %w", context.Canceled)},
			wantClass: resultShutdown,
		},
		{
			name:            "401_unauthorized",
			result:          ActionCompletion{HTTPStatus: http.StatusUnauthorized, Err: graph.ErrUnauthorized},
			wantClass:       resultFatal,
			wantSummaryKey:  SummaryAuthenticationRequired,
			wantPersistence: persistActionableFailure,
		},
		{
			name:            "403_forbidden",
			result:          ActionCompletion{HTTPStatus: http.StatusForbidden, Err: graph.ErrForbidden},
			wantClass:       resultSkip,
			wantSummaryKey:  SummaryRemotePermissionDenied,
			wantPersistence: persistActionableFailure,
			wantPermission:  permissionFlowRemote403,
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
		{name: "404_not_found", result: ActionCompletion{HTTPStatus: http.StatusNotFound, Err: graph.ErrNotFound}, wantClass: resultRequeue, wantSummaryKey: SummarySyncFailure, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "408_request_timeout", result: ActionCompletion{HTTPStatus: http.StatusRequestTimeout, Err: errors.New("timeout")}, wantClass: resultRequeue, wantSummaryKey: SummarySyncFailure, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "412_precondition_failed", result: ActionCompletion{HTTPStatus: http.StatusPreconditionFailed, Err: errors.New("etag mismatch")}, wantClass: resultRequeue, wantSummaryKey: SummarySyncFailure, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "423_locked", result: ActionCompletion{HTTPStatus: http.StatusLocked, Err: graph.ErrLocked}, wantClass: resultRequeue, wantSummaryKey: SummarySyncFailure, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "429_too_many_requests", result: ActionCompletion{HTTPStatus: http.StatusTooManyRequests, DriveID: testThrottleDriveID(), Err: graph.ErrThrottled}, wantClass: resultBlockScope, wantScope: testThrottleScope(), wantSummaryKey: SummaryRateLimited, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "400_invalid_request_is_skip", result: ActionCompletion{HTTPStatus: http.StatusBadRequest, Err: genericInvalidRequestErr}, wantClass: resultSkip, wantSummaryKey: SummarySyncFailure, wantPersistence: persistActionableFailure},
		{name: "400_object_handle_message_only_is_skip", result: ActionCompletion{HTTPStatus: http.StatusBadRequest, Err: legacyOutageErr}, wantClass: resultSkip, wantSummaryKey: SummarySyncFailure, wantPersistence: persistActionableFailure},
		{name: "400_object_handle_wrong_code_is_skip", result: ActionCompletion{HTTPStatus: http.StatusBadRequest, Err: wrongCodeOutageErr}, wantClass: resultSkip, wantSummaryKey: SummarySyncFailure, wantPersistence: persistActionableFailure},
		{name: "500_internal_server_error", result: ActionCompletion{HTTPStatus: http.StatusInternalServerError, Err: graph.ErrServerError}, wantClass: resultRequeue, wantSummaryKey: SummaryServiceOutage, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "502_bad_gateway", result: ActionCompletion{HTTPStatus: http.StatusBadGateway, Err: graph.ErrServerError}, wantClass: resultRequeue, wantSummaryKey: SummaryServiceOutage, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "503_service_unavailable", result: ActionCompletion{HTTPStatus: http.StatusServiceUnavailable, Err: graph.ErrServerError}, wantClass: resultRequeue, wantSummaryKey: SummaryServiceOutage, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "504_gateway_timeout", result: ActionCompletion{HTTPStatus: http.StatusGatewayTimeout, Err: graph.ErrServerError}, wantClass: resultRequeue, wantSummaryKey: SummaryServiceOutage, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "509_storage_limit", result: ActionCompletion{HTTPStatus: 509, Err: graph.ErrServerError}, wantClass: resultRequeue, wantSummaryKey: SummaryServiceOutage, wantPersistence: persistTransientFailure, wantScopeDetect: true},
		{name: "409_conflict", result: ActionCompletion{HTTPStatus: http.StatusConflict, Err: graph.ErrConflict}, wantClass: resultSkip, wantSummaryKey: SummarySyncFailure, wantPersistence: persistActionableFailure},
		{name: "other_4xx_falls_to_skip", result: ActionCompletion{HTTPStatus: http.StatusMethodNotAllowed, Err: graph.ErrMethodNotAllowed}, wantClass: resultSkip, wantSummaryKey: SummarySyncFailure, wantPersistence: persistActionableFailure},
	})
}

func TestClassifyResult_StorageScopes(t *testing.T) {
	t.Parallel()

	assertClassifyResultCases(t, []classifyResultCase{
		{
			name: "507_own_drive",
			result: ActionCompletion{
				HTTPStatus: http.StatusInsufficientStorage,
				Err:        errors.New("insufficient storage"),
			},
			wantClass:       resultBlockScope,
			wantScope:       SKQuotaOwn(),
			wantSummaryKey:  SummaryQuotaExceeded,
			wantPersistence: persistTransientFailure,
			wantScopeDetect: true,
		},
		{
			name: "507_shared_root_drive",
			result: ActionCompletion{
				HTTPStatus:    http.StatusInsufficientStorage,
				Err:           errors.New("insufficient storage"),
				TargetDriveID: driveid.New("drive1"),
			},
			wantClass:       resultBlockScope,
			wantScope:       SKQuotaOwn(),
			wantSummaryKey:  SummaryQuotaExceeded,
			wantPersistence: persistTransientFailure,
			wantScopeDetect: true,
		},
	})
}

func TestClassifyResult_LocalErrors(t *testing.T) {
	t.Parallel()

	assertClassifyResultCases(t, []classifyResultCase{
		{name: "os_err_permission", result: ActionCompletion{Err: os.ErrPermission}, wantClass: resultSkip, wantSummaryKey: SummaryLocalPermissionDenied, wantPersistence: persistActionableFailure, wantPermission: permissionFlowLocalPermission},
		{
			name:            "wrapped_os_err_permission",
			result:          ActionCompletion{Err: fmt.Errorf("cannot write: %w", os.ErrPermission)},
			wantClass:       resultSkip,
			wantSummaryKey:  SummaryLocalPermissionDenied,
			wantPersistence: persistActionableFailure,
			wantPermission:  permissionFlowLocalPermission,
		},
		{
			name:            "disk_full",
			result:          ActionCompletion{Err: fmt.Errorf("download failed: %w", driveops.ErrDiskFull)},
			wantClass:       resultBlockScope,
			wantScope:       SKDiskLocal(),
			wantSummaryKey:  SummaryDiskFull,
			wantPersistence: persistTransientFailure,
		},
		{
			name:            "file_too_large_for_space",
			result:          ActionCompletion{Err: fmt.Errorf("download failed: %w", driveops.ErrFileTooLargeForSpace)},
			wantClass:       resultSkip,
			wantSummaryKey:  SummaryFileTooLargeForSpace,
			wantPersistence: persistActionableFailure,
		},
		{
			name:            "file_exceeds_onedrive_limit",
			result:          ActionCompletion{Err: fmt.Errorf("upload failed: %w", driveops.ErrFileExceedsOneDriveLimit)},
			wantClass:       resultSkip,
			wantSummaryKey:  SummaryFileTooLarge,
			wantPersistence: persistActionableFailure,
		},
		{
			name:            "stale_delete_precondition",
			result:          ActionCompletion{Err: fmt.Errorf("delete lost race: %w", ErrActionPreconditionChanged)},
			wantClass:       resultRequeue,
			wantSummaryKey:  SummarySyncFailure,
			wantPersistence: persistTransientFailure,
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

	// Set up an active persisted block scope.
	now := eng.nowFunc()
	setTestBlockScope(t, eng, &BlockScope{
		Key:           testThrottleScope(),
		IssueType:     IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 10 * time.Second,
		NextTrialAt:   now.Add(10 * time.Second),
	})

	// Add scope-blocked failures to the DB (these would be unblocked on success).
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "first.txt", DriveID: driveid.New("d"), Direction: DirectionUpload,
		Role:     FailureRoleHeld,
		Category: CategoryTransient, ErrMsg: "rate limited", ScopeKey: testThrottleScope(),
	}, nil)) // nil delayFn → scope-blocked (next_retry_at = NULL)

	// Add the trial action to the DepGraph.
	testWatchRuntime(t, eng).depGraph.Add(&Action{Type: ActionUpload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)

	// Simulate successful trial result.
	processTrialResultForTest(t, eng, ctx, &ActionCompletion{
		ActionID:      1,
		IsTrial:       true,
		TrialScopeKey: testThrottleScope(),
		Success:       true,
	})

	// Scope block should be cleared.
	assert.False(t, isTestBlockScopeed(eng, testThrottleScope()),
		"block scope should be removed after successful trial")

	// Scope-blocked failures should now be retryable (next_retry_at set to ~now).
	rows := readyRetryWorkForTest(t, eng.baseline, ctx, now)
	assert.Len(t, rows, 1, "scope-blocked failures should be unblocked after trial success")
}

// Validates: R-2.10.14
func TestProcessTrialResultV2_Failure_DoublesInterval(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	now := eng.nowFunc()
	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	})

	// Add the trial action to the DepGraph.
	testWatchRuntime(t, eng).depGraph.Add(&Action{Type: ActionDownload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

	processTrialResultForTest(t, eng, ctx, &ActionCompletion{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: SKService(),
		Success:       false,
		HTTPStatus:    503,
		ErrMsg:        "service unavailable",
	})

	// Verify block's TrialInterval was doubled.
	got, ok := getTestBlockScope(eng, SKService())
	require.True(t, ok)
	assert.Equal(t, 60*time.Second, got.TrialInterval, "interval should be doubled")
}

// Validates: R-2.10.6, R-2.10.8, R-2.10.14
// Unified cap for all scope types.
func TestProcessTrialResultV2_Failure_CapsAt5m(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scopeKey   ScopeKey
		issueType  string
		httpStatus int
		actionType ActionType
	}{
		{"quota", SKQuotaOwn(), IssueQuotaExceeded, 507, ActionUpload},
		{"service", SKService(), IssueServiceOutage, 500, ActionDownload},
		{"throttle", testThrottleScope(), IssueRateLimited, 429, ActionUpload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng := newSingleOwnerEngine(t)
			ctx := t.Context()

			now := eng.nowFunc()

			// Start with an interval that would exceed 5m when doubled.
			setTestBlockScope(t, eng, &BlockScope{
				Key:           tt.scopeKey,
				IssueType:     tt.issueType,
				BlockedAt:     now,
				TrialInterval: 4 * time.Minute,
				NextTrialAt:   now.Add(4 * time.Minute),
			})

			testWatchRuntime(t, eng).depGraph.Add(&Action{Type: tt.actionType, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

			processTrialResultForTest(t, eng, ctx, &ActionCompletion{
				ActionID:      99,
				IsTrial:       true,
				TrialScopeKey: tt.scopeKey,
				Success:       false,
				HTTPStatus:    tt.httpStatus,
				DriveID:       testThrottleDriveID(),
				ErrMsg:        "test failure",
			})

			got, ok := getTestBlockScope(eng, tt.scopeKey)
			require.True(t, ok)
			assert.Equal(t, DefaultMaxTrialInterval, got.TrialInterval,
				"%s interval should cap at %v", tt.name, DefaultMaxTrialInterval)
		})
	}
}

// Validates: R-2.10.5
// Trial failure must not trigger scope detection.
func TestProcessTrialResultV2_Failure_NoScopeDetection(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	ss := NewScopeState(eng.nowFn, eng.logger)
	testWatchRuntime(t, eng).scopeState = ss

	now := eng.nowFunc()
	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	})

	testWatchRuntime(t, eng).depGraph.Add(&Action{Type: ActionDownload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

	processTrialResultForTest(t, eng, ctx, &ActionCompletion{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: SKService(),
		Success:       false,
		HTTPStatus:    500,
		ErrMsg:        "internal server error",
	})

	got, ok := getTestBlockScope(eng, SKService())
	require.True(t, ok, "block scope should still exist")
	assert.Equal(t, 60*time.Second, got.TrialInterval, "interval should be doubled, not reset")
}

// Validates: R-2.10.5
func TestProcessTrialResultV2_Rearm_RetryableHTTPStatusesKeepScopeTimingAndHeldCandidate(t *testing.T) {
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

			setTestBlockScope(t, eng, &BlockScope{
				Key:           SKService(),
				IssueType:     IssueServiceOutage,
				BlockedAt:     now,
				TrialInterval: 30 * time.Second,
				NextTrialAt:   now.Add(30 * time.Second),
				TrialCount:    2,
			})
			require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
				Path:      "trial.txt",
				DriveID:   eng.driveID,
				Direction: DirectionDownload,
				Role:      FailureRoleHeld,
				Category:  CategoryTransient,
				ScopeKey:  SKService(),
				ItemID:    "i1",
			}, nil))

			testWatchRuntime(t, eng).depGraph.Add(&Action{
				Type:    ActionDownload,
				Path:    "trial.txt",
				DriveID: driveid.New("d"),
				ItemID:  "i1",
			}, 99, nil)

			processTrialResultForTest(t, eng, ctx, &ActionCompletion{
				ActionID:      99,
				IsTrial:       true,
				TrialScopeKey: SKService(),
				ActionType:    ActionDownload,
				Path:          "trial.txt",
				DriveID:       eng.driveID,
				Success:       false,
				HTTPStatus:    tt.status,
				Err:           tt.err,
				ErrMsg:        tt.errMsg,
			})

			got, ok := getTestBlockScope(eng, SKService())
			require.True(t, ok, "block scope should still exist")
			assert.Equal(t, 30*time.Second, got.TrialInterval, "inconclusive trial must not back off the original scope")
			assert.Equal(t, now.Add(30*time.Second), got.NextTrialAt, "rearm should keep the original interval")
			assert.Equal(t, 2, got.TrialCount, "rearm should not increment trial backoff history")

			failures, err := eng.baseline.ListSyncFailures(ctx)
			require.NoError(t, err)
			require.Len(t, failures, 1)
			assert.Equal(t, FailureRoleHeld, failures[0].Role,
				"inconclusive trial should leave the candidate held for the original scope")
			assert.Equal(t, SKService(), failures[0].ScopeKey)
		})
	}
}

// Validates: R-2.10.5
func TestProcessTrialResultV2_Fatal401DoesNotExtendScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           testThrottleScope(),
		IssueType:     IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 45 * time.Second,
		NextTrialAt:   now.Add(45 * time.Second),
		TrialCount:    3,
	})
	testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		ItemID:  "i1",
	}, 77, nil)

	outcome := processActionCompletionDetailedForTest(t, eng, ctx, &ActionCompletion{
		ActionID:      77,
		IsTrial:       true,
		TrialScopeKey: testThrottleScope(),
		ActionType:    ActionUpload,
		Path:          "trial.txt",
		DriveID:       eng.driveID,
		Success:       false,
		HTTPStatus:    http.StatusUnauthorized,
		Err:           graph.ErrUnauthorized,
		ErrMsg:        "unauthorized",
	}, nil)

	require.ErrorIs(t, outcome.terminateErr, graph.ErrUnauthorized)
	assert.True(t, outcome.terminate, "trial unauthorized should terminate result routing")

	got, ok := getTestBlockScope(eng, testThrottleScope())
	require.True(t, ok, "fatal unauthorized should not clear the original scope")
	assert.Equal(t, 45*time.Second, got.TrialInterval, "fatal unauthorized must not back off the original scope")
	assert.Equal(t, now.Add(45*time.Second), got.NextTrialAt, "fatal unauthorized must not reschedule the original scope")
	assert.Equal(t, 3, got.TrialCount)

	assert.Equal(t, authstate.ReasonSyncAuthRejected, loadCatalogAuthRequirementForTest(t, eng))

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "trial unauthorized should not create per-path sync_failures rows")
}

// Validates: R-2.10.5, R-2.10.14
func TestProcessTrialResultV2_Rearm_LocalPermissionRecordsCandidateFailureAndDiscardsEmptyScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 45 * time.Second,
		NextTrialAt:   now.Add(45 * time.Second),
		TrialCount:    1,
	})
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "trial.txt",
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ScopeKey:  SKService(),
		ItemID:    "i1",
	}, nil))

	testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		ItemID:  "i1",
	}, 88, nil)

	processTrialResultForTest(t, eng, ctx, &ActionCompletion{
		ActionID:      88,
		IsTrial:       true,
		TrialScopeKey: SKService(),
		ActionType:    ActionUpload,
		Path:          "trial.txt",
		DriveID:       eng.driveID,
		Success:       false,
		Err:           os.ErrPermission,
		ErrMsg:        "permission denied",
	})

	_, ok := getTestBlockScope(eng, SKService())
	assert.False(t, ok, "scope should be discarded once the held retry work is cleared")

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, FailureRoleItem, failures[0].Role)
	assert.Equal(t, IssueLocalPermissionDenied, failures[0].IssueType)
	assert.True(t, failures[0].ScopeKey.IsZero(), "file-level local permission retry reclassification should not rewrite the original scope")
}

// Validates: R-2.10.5, R-2.10.14, R-2.14.1
// Validates: R-2.10.5, R-2.10.6, R-2.10.7, R-2.10.8, R-2.10.43
func TestEvaluateTrialOutcome_OnlyMatchingScopeEvidenceExtends(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	tests := []struct {
		name     string
		scopeKey ScopeKey
		result   ActionCompletion
		want     trialOutcome
	}{
		{
			name:     "throttle_429_extends",
			scopeKey: testThrottleScope(),
			result:   ActionCompletion{HTTPStatus: http.StatusTooManyRequests, DriveID: testThrottleDriveID(), Err: graph.ErrThrottled},
			want:     trialOutcomeExtend,
		},
		{
			name:     "service_503_extends",
			scopeKey: SKService(),
			result:   ActionCompletion{HTTPStatus: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			want:     trialOutcomeExtend,
		},
		{
			name:     "quota_own_507_extends",
			scopeKey: SKQuotaOwn(),
			result:   ActionCompletion{HTTPStatus: http.StatusInsufficientStorage},
			want:     trialOutcomeExtend,
		},
		{
			name:     "disk_full_extends",
			scopeKey: SKDiskLocal(),
			result:   ActionCompletion{Err: driveops.ErrDiskFull},
			want:     trialOutcomeExtend,
		},
		{
			name:     "throttle_does_not_extend_service_error",
			scopeKey: testThrottleScope(),
			result:   ActionCompletion{HTTPStatus: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			want:     trialOutcomeRearm,
		},
		{
			name:     "service_does_not_extend_throttle_error",
			scopeKey: SKService(),
			result:   ActionCompletion{HTTPStatus: http.StatusTooManyRequests, DriveID: testThrottleDriveID(), Err: graph.ErrThrottled},
			want:     trialOutcomeRearm,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			decision := classifyResult(&tt.result)
			assert.Equal(t, tt.want, flow.evaluateTrialOutcome(tt.scopeKey, &decision))
		})
	}
}

// Validates: R-2.10.14
// computeTrialInterval is the single source of truth for initial intervals and
// backoff extensions.
func TestComputeTrialInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		scopeKey        ScopeKey
		retryAfter      time.Duration
		currentInterval time.Duration
		want            time.Duration
	}{
		// Retry-After: used directly, no cap (R-2.10.7).
		{"retry-after honored", SKService(), 90 * time.Second, 0, 90 * time.Second},
		{"retry-after exceeds max", SKService(), 30 * time.Minute, 0, 30 * time.Minute},
		{"retry-after with current", SKService(), 2 * time.Minute, 30 * time.Second, 2 * time.Minute},

		// No Retry-After, no current: initial interval.
		{"initial interval", SKService(), 0, 0, DefaultInitialTrialInterval},
		{"disk initial interval", SKDiskLocal(), 0, 0, diskScopeInitialTrialInterval},

		// No Retry-After, with current: double + cap.
		{"double interval", SKService(), 0, 30 * time.Second, 60 * time.Second},
		{"double caps at max", SKService(), 0, 4 * time.Minute, DefaultMaxTrialInterval},
		{"already at max stays", SKService(), 0, DefaultMaxTrialInterval, DefaultMaxTrialInterval},
		{"disk double interval", SKDiskLocal(), 0, 30 * time.Minute, 60 * time.Minute},
		{"disk caps at max", SKDiskLocal(), 0, 45 * time.Minute, diskScopeMaxTrialInterval},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTrialInterval(tt.scopeKey, tt.retryAfter, tt.currentInterval)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-2.10.7
// Retry-After is used directly with no cap.
func TestExtendTrialInterval_WithRetryAfter(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	now := eng.nowFunc()
	setTestBlockScope(t, eng, &BlockScope{
		Key:           testThrottleScope(),
		IssueType:     IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	})

	testWatchRuntime(t, eng).depGraph.Add(&Action{Type: ActionUpload, Path: "trial.txt", DriveID: testThrottleDriveID(), ItemID: "i1"}, 99, nil)

	// Retry-After of 30 minutes exceeds DefaultMaxTrialInterval (5m) — must be
	// honored directly with no cap, because the server is ground truth.
	processTrialResultForTest(t, eng, ctx, &ActionCompletion{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: testThrottleScope(),
		Success:       false,
		HTTPStatus:    429,
		DriveID:       testThrottleDriveID(),
		RetryAfter:    30 * time.Minute,
		ErrMsg:        "too many requests",
	})

	got, ok := getTestBlockScope(eng, testThrottleScope())
	require.True(t, ok)
	assert.Equal(t, 30*time.Minute, got.TrialInterval,
		"Retry-After must be used directly with no cap — server is ground truth")
}

// Validates: R-2.10.43
// Full disk:local scope-block lifecycle:
// ErrDiskFull -> classifyResult -> active block scopes downloads -> trial -> release.
func TestDiskLocalBlockScope_FullCycle(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	// 1. classifyResult maps ErrDiskFull to disk:local block scope.
	decision := classifyResult(&ActionCompletion{
		Err: fmt.Errorf("download failed: %w", driveops.ErrDiskFull),
	})
	require.Equal(t, resultBlockScope, decision.Class)
	require.Equal(t, SKDiskLocal(), decision.ScopeKey)
	require.Equal(t, persistTransientFailure, decision.Persistence)
	assert.False(t, decision.RunScopeDetection, "disk:local uses direct scope activation, not HTTP scope detection")

	// 2. Establish the active block scope.
	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKDiskLocal(),
		IssueType:     IssueDiskFull,
		BlockedAt:     now,
		TrialInterval: 10 * time.Second,
		NextTrialAt:   now.Add(10 * time.Second),
	})

	// 3. Active-scope admission blocks downloads under disk:local, allows uploads.
	dlAction := &TrackedAction{ID: 1, Action: Action{Type: ActionDownload, Path: "big.zip", DriveID: driveid.New("d"), ItemID: "dl1"}}
	ulAction := &TrackedAction{ID: 2, Action: Action{Type: ActionUpload, Path: "small.txt", DriveID: driveid.New("d"), ItemID: "ul1"}}

	assert.False(t, activeBlockingScopeForTest(t, eng, dlAction).IsZero(), "download should be blocked by disk:local scope")
	assert.True(t, activeBlockingScopeForTest(t, eng, ulAction).IsZero(), "upload should NOT be blocked by disk:local scope")

	// 4. Release block scope (simulating trial success / disk space freed).
	require.NoError(t, releaseTestScope(t, eng, ctx, SKDiskLocal()))

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
		name       string
		httpStatus int
		want       ScopeKey
	}{
		{"429_throttle", 429, testThrottleScope()},
		{"503_service", 503, SKService()},
		{"507_own", 507, SKQuotaOwn()},
		{"500_service", 500, SKService()},
		{"502_service", 502, SKService()},
		{"200_empty", 200, ScopeKey{}},
		{"404_empty", 404, ScopeKey{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &ActionCompletion{
				HTTPStatus: tt.httpStatus,
				DriveID:    testThrottleDriveID(),
			}
			assert.Equal(t, tt.want, deriveScopeKey(r))
		})
	}
}

// ---------------------------------------------------------------------------
// applyBlockScope arms trial timer
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestApplyBlockScope_ArmsTrialTimer(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	// applyBlockScope persists the scope and arms the trial timer.
	applyBlockScopeForTest(t, eng, ctx, ScopeUpdateResult{
		Block:      true,
		ScopeKey:   testThrottleScope(),
		IssueType:  IssueRateLimited,
		RetryAfter: 30 * time.Second,
	})

	// Verify the block has the correct NextTrialAt from the injectable clock.
	earliest, ok := testWatchRuntime(t, eng).earliestTrialAt()
	require.True(t, ok, "EarliestTrialAt should find the block scope")
	assert.Equal(t, now.Add(30*time.Second), earliest, "NextTrialAt should be now + trial interval")

	// Trial timer should be armed.
	timerSet := testWatchRuntime(t, eng).hasTrialTimer()
	assert.True(t, timerSet, "trial timer should be armed after applyBlockScope")
}

// ---------------------------------------------------------------------------
// recordFailure populates ScopeKey
// ---------------------------------------------------------------------------

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	processActionCompletionForTest(t, eng, ctx, &ActionCompletion{
		Path:       "quota-fail.txt",
		ActionType: ActionUpload,
		Success:    false,
		ErrMsg:     "insufficient storage",
		HTTPStatus: 507,
		ActionID:   1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, SKQuotaOwn(), issues[0].ScopeKey, "507 own-drive should populate scope key")
}

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey_429(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	processActionCompletionForTest(t, eng, ctx, &ActionCompletion{
		Path:       "throttled.txt",
		ActionType: ActionDownload,
		Success:    false,
		ErrMsg:     "too many requests",
		HTTPStatus: 429,
		DriveID:    testThrottleDriveID(),
		ActionID:   1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, testThrottleScope(), issues[0].ScopeKey)
}

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey_507Quota(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	processActionCompletionForTest(t, eng, ctx, &ActionCompletion{
		Path:       "shared/file.txt",
		ActionType: ActionUpload,
		Success:    false,
		ErrMsg:     "quota exceeded",
		HTTPStatus: 507,
		ActionID:   1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, SKQuotaOwn(), issues[0].ScopeKey)
}

// ---------------------------------------------------------------------------
// One-shot engine-loop integration tests (single-owner result processing)
// ---------------------------------------------------------------------------

// startDrainLoop creates a real engine with DepGraph, watch-mode scope state,
// dispatchCh, dirty scheduler, and retryTimerCh — the full one-shot engine-loop
// pipeline used by these tests.
func startDrainLoop(t *testing.T) (chan ActionCompletion, <-chan struct{}, context.CancelFunc, *testEngine) {
	t.Helper()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	results := make(chan ActionCompletion, 16)

	ctx, cancel := context.WithCancel(t.Context())
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	safety := DefaultSafetyConfig()
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
	bl *Baseline,
	safety *SafetyConfig,
	results <-chan ActionCompletion,
) {
	var outbox []*TrackedAction

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
	bl *Baseline,
	safety *SafetyConfig,
	results <-chan ActionCompletion,
) ([]*TrackedAction, bool) {
	select {
	case workerResult, ok := <-results:
		if !ok {
			return nil, true
		}
		return appendDrainOutcome(rt, ctx, bl, nil, &workerResult)
	case <-rt.trialTimerChan():
		return rt.runTrialDispatch(ctx, bl, SyncBidirectional, safety), false
	case <-rt.retryTimerChan():
		return rt.runRetrierSweep(ctx, bl, SyncBidirectional, safety), false
	case <-ctx.Done():
		return nil, true
	}
}

func runResultDrainLoopWithOutboxForTest(
	ctx context.Context,
	rt *watchRuntime,
	bl *Baseline,
	safety *SafetyConfig,
	results <-chan ActionCompletion,
	outbox []*TrackedAction,
) ([]*TrackedAction, bool) {
	select {
	case rt.dispatchCh <- outbox[0]:
		return outbox[1:], false
	case workerResult, ok := <-results:
		if !ok {
			return outbox, true
		}
		return appendDrainOutcome(rt, ctx, bl, outbox, &workerResult)
	case <-rt.trialTimerChan():
		return append(outbox, rt.runTrialDispatch(ctx, bl, SyncBidirectional, safety)...), false
	case <-rt.retryTimerChan():
		return append(outbox, rt.runRetrierSweep(ctx, bl, SyncBidirectional, safety)...), false
	case <-ctx.Done():
		return outbox, true
	}
}

func appendDrainOutcome(
	rt *watchRuntime,
	ctx context.Context,
	bl *Baseline,
	outbox []*TrackedAction,
	workerResult *ActionCompletion,
) ([]*TrackedAction, bool) {
	outcome := rt.processActionCompletion(ctx, rt, workerResult, bl)
	if outcome.terminate {
		return outbox, true
	}

	return append(outbox, outcome.dispatched...), false
}

// readReadyAction reads one TrackedAction from the ready channel with a race-
// detector-friendly timeout. Full verifier runs can make SQLite replans take
// longer than the narrower unit-test-only budget.
func readReadyAction(t *testing.T, ready <-chan *TrackedAction) *TrackedAction {
	t.Helper()

	select {
	case ta := <-ready:
		return ta
	case <-time.After(3 * time.Second):
		require.Fail(t, "timed out waiting for action on ready channel")
	}

	return nil
}

func readReady(t *testing.T, ready <-chan *TrackedAction) {
	t.Helper()
	_ = readReadyAction(t, ready)
}

// Validates: R-2.10.5
// The one-shot engine loop processes results and routes dependents.
func TestE2E_OneShotEngineLoop_ProcessesAndRoutes(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng := startDrainLoop(t)
	defer cancel()

	ctx := t.Context()

	// Add parent action to DepGraph, send to dispatchCh.
	ta := testWatchRuntime(t, eng).depGraph.Add(&Action{Type: ActionUpload, Path: "a.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 0, nil)
	require.NotNil(t, ta)
	testWatchRuntime(t, eng).dispatchCh <- ta
	readReady(t, testWatchRuntime(t, eng).dispatchCh)

	// Send 429 result — scope detection creates block + records failure.
	results <- ActionCompletion{
		ActionID:   0,
		Path:       "a.txt",
		ActionType: ActionUpload,
		DriveID:    testThrottleDriveID(),
		Success:    false,
		HTTPStatus: 429,
		RetryAfter: 5 * time.Millisecond,
		ErrMsg:     "rate limited",
		Err:        fmt.Errorf("rate limited"),
	}

	// Verify block scope created and failure recorded.
	require.Eventually(t, func() bool {
		return isTestBlockScopeed(eng, testThrottleScope())
	}, time.Second, time.Millisecond, "block scope should be created")

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
	rt := testWatchRuntime(t, eng)
	batches := make(chan DirtyBatch, 2)
	results := make(chan ActionCompletion, 2)
	done := make(chan error, 1)

	go func() {
		done <- rt.runWatchLoop(ctx, &watchPipeline{
			runtime:     rt,
			bl:          bl,
			safety:      DefaultSafetyConfig(),
			batchReady:  batches,
			completions: results,
			mode:        SyncBidirectional,
		})
	}()

	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "alpha-item",
		Path:     "alpha.txt",
		ItemType: ItemTypeFile,
		Hash:     "alpha-hash",
		Size:     10,
	}}, "", driveID))
	batches <- DirtyBatch{Paths: []string{"alpha.txt"}}

	first := readReadyAction(t, ready)
	require.Equal(t, "alpha.txt", first.Action.Path)

	results <- ActionCompletion{
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

	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "beta-item",
		Path:     "beta.txt",
		ItemType: ItemTypeFile,
		Hash:     "beta-hash",
		Size:     12,
	}}, "", driveID))
	batches <- DirtyBatch{Paths: []string{"beta.txt"}}

	var second *TrackedAction
	for i := 0; i < 2; i++ {
		candidate := readReadyAction(t, ready)
		if candidate.Action.Path == "beta.txt" {
			second = candidate
			break
		}

		results <- ActionCompletion{
			ActionID:   candidate.ID,
			Path:       candidate.Action.Path,
			DriveID:    driveID,
			ActionType: candidate.Action.Type,
			Success:    true,
		}
	}
	require.NotNil(t, second, "steady-state watch loop should keep processing later batches")

	cancel()
	close(results)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "watch loop did not exit after cancellation")
	}
}

// Validates: R-2.10.5, R-2.10.11
// TestE2E_OneShotLoop_TrialResultSuccess verifies that trial success clears the
// block scope and re-injects held failures without waiting for a new external observation.
func TestE2E_OneShotLoop_TrialResultSuccess(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done

	ctx := t.Context()

	// Set up block scope and a scope-blocked failure.
	setTestBlockScope(t, eng, &BlockScope{
		Key:           testThrottleScope(),
		IssueType:     IssueRateLimited,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	})
	require.NoError(t, os.WriteFile(filepath.Join(eng.syncRoot, "blocked.txt"), []byte("blocked payload"), 0o600))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "blocked.txt", DriveID: driveid.New(engineTestDriveID), Direction: DirectionUpload,
		Role:     FailureRoleHeld,
		Category: CategoryTransient, ErrMsg: "rate limited", ScopeKey: testThrottleScope(),
	}, nil))

	// Add trial action to depGraph.
	ta := testWatchRuntime(t, eng).depGraph.Add(&Action{Type: ActionUpload, Path: "trial.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 1, nil)
	require.NotNil(t, ta)

	// Send trial success via completions channel.
	results <- ActionCompletion{
		ActionID:      1,
		Path:          "trial.txt",
		ActionType:    ActionUpload,
		DriveID:       driveid.New(engineTestDriveID),
		Success:       true,
		IsTrial:       true,
		TrialScopeKey: testThrottleScope(),
	}

	// Scope block should be cleared.
	require.Eventually(t, func() bool {
		return !isTestBlockScopeed(eng, testThrottleScope())
	}, 5*time.Second, 10*time.Millisecond, "block scope should be cleared after trial success")

	var released *TrackedAction
	require.Eventually(t, func() bool {
		select {
		case released = <-testWatchRuntime(t, eng).dispatchCh:
			return released != nil && released.Action.Path == "blocked.txt"
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "trial success should re-dispatch the held failure without external observation")

	require.NotNil(t, released)
	assert.Equal(t, ActionUpload, released.Action.Type)
}

// TestE2E_OneShotLoop_TrialResultFailure verifies trial failure doubles the interval.
func TestE2E_OneShotLoop_TrialResultFailure(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done

	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	})

	ta := testWatchRuntime(t, eng).depGraph.Add(&Action{Type: ActionDownload, Path: "trial.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 99, nil)
	require.NotNil(t, ta)

	results <- ActionCompletion{
		ActionID:      99,
		Path:          "trial.txt",
		ActionType:    ActionDownload,
		DriveID:       driveid.New(engineTestDriveID),
		Success:       false,
		HTTPStatus:    500,
		ErrMsg:        "internal server error",
		Err:           fmt.Errorf("internal server error"),
		IsTrial:       true,
		TrialScopeKey: SKService(),
	}

	// Interval should be doubled from 10ms to 20ms.
	require.Eventually(t, func() bool {
		block, ok := getTestBlockScope(eng, SKService())
		return ok && block.TrialInterval == 20*time.Millisecond
	}, time.Second, time.Millisecond, "trial failure should double interval")
}

func TestE2E_OneShotLoopExit_StopsTimer(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	ctx := t.Context()

	// Create block scope → arms trial timer.
	applyBlockScopeForTest(t, eng, ctx, ScopeUpdateResult{
		Block:      true,
		ScopeKey:   SKService(),
		IssueType:  IssueServiceOutage,
		RetryAfter: time.Hour, // long interval so it doesn't fire during test
	})

	// Verify timer is armed.
	require.Eventually(t, func() bool {
		return testWatchRuntime(t, eng).hasTrialTimer()
	}, time.Second, time.Millisecond)

	// Close completions channel → the one-shot loop returns → defer stopTrialTimer.
	close(results)
	select {
	case <-done:
	case <-time.After(time.Second):
		require.Fail(t, "one-shot engine loop did not exit after completions channel close")
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
	wantScope ScopeKey,
	wantIssue string,
) {
	t.Helper()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	for i := range threshold {
		sr := ss.UpdateScope(&ActionCompletion{
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
	wantScope ScopeKey,
	wantIssue string,
) {
	t.Helper()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	sr := ss.UpdateScope(&ActionCompletion{
		Path:          "/file.txt",
		HTTPStatus:    httpStatus,
		RetryAfter:    retryAfter,
		DriveID:       testThrottleDriveID(),
		TargetDriveID: testThrottleDriveID(),
	})

	require.True(t, sr.Block, "Retry-After should trigger an immediate block scope")
	assert.Equal(t, wantScope, sr.ScopeKey)
	assert.Equal(t, wantIssue, sr.IssueType)
	assert.Equal(t, retryAfter, sr.RetryAfter, "RetryAfter should pass through")
}

// Validates: R-2.10.6
func TestTrialTimer_QuotaStartsAt5s(t *testing.T) {
	t.Parallel()

	assertScopeWindowBlock(t, 507, 3, SKQuotaOwn(), "quota_exceeded")
}

// TestTrialTimer_BackoffCapsAt5m is covered by
// TestProcessTrialResultV2_Failure_CapsAt5m which uses active persisted scopes.
// Removed: old test used held-queue mechanism.

// Validates: R-2.10.7
func TestTrialTimer_RateLimited_StartsAtRetryAfter(t *testing.T) {
	t.Parallel()

	assertImmediateRetryAfterBlock(t, 429, 90*time.Second, testThrottleScope(), "rate_limited")
}

// TestTrialTimer_RateLimited_BlocksAllActionTypes is covered by
// scope_gate_test.go covers the pure active-scope helper functions directly.
// Removed: old test used held-queue mechanism.

// Validates: R-2.10.8
func TestTrialTimer_Service_StartsAt5s(t *testing.T) {
	t.Parallel()

	assertScopeWindowBlock(t, 500, 5, SKService(), "service_outage")
}

// Validates: R-2.10.8
func TestTrialTimer_Service_503RetryAfterOverride(t *testing.T) {
	t.Parallel()

	assertImmediateRetryAfterBlock(t, 503, 120*time.Second, SKService(), "service_outage")
}

// clearResolvedSkippedItems
// ---------------------------------------------------------------------------

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_AllScannerIssueTypes(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record failures for each scanner-detectable issue type.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "bad\x01name.txt", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueInvalidFilename, Category: CategoryActionable, ErrMsg: "invalid character",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "still-bad\x02.txt", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueInvalidFilename, Category: CategoryActionable, ErrMsg: "invalid character",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "very/long/path.txt", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssuePathTooLong, Category: CategoryActionable, ErrMsg: "path exceeds limit",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "huge-file.bin", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueFileTooLarge, Category: CategoryActionable, ErrMsg: "file too large",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "fragile.bin", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueHashPanic, Category: CategoryActionable, ErrMsg: "panic: boom",
	}, nil))

	// Verify all 5 failures exist.
	all, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 5)

	// Simulate a new scan where only "still-bad\x02.txt" still exists as skipped.
	// "bad\x01name.txt" was renamed, "very/long/path.txt" was shortened,
	// "huge-file.bin" was deleted, and "fragile.bin" hashed successfully.
	currentSkipped := []SkippedItem{
		{Path: "still-bad\x02.txt", Reason: IssueInvalidFilename},
	}

	testEngineFlow(t, eng).clearResolvedSkippedItems(ctx, currentSkipped)

	// Only the still-existing invalid filename should remain.
	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "still-bad\x02.txt", remaining[0].Path)
	assert.Equal(t, IssueInvalidFilename, remaining[0].IssueType)
}

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_EmptySkipped_ClearsAll(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record one failure per type.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "bad.txt", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueInvalidFilename, Category: CategoryActionable, ErrMsg: "invalid",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "long.txt", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssuePathTooLong, Category: CategoryActionable, ErrMsg: "too long",
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
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "bad.txt", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueInvalidFilename, Category: CategoryActionable, ErrMsg: "invalid",
	}, nil))

	// Record a runtime failure (permission denied — not scanner-detectable).
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "Shared/folder", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssuePermissionDenied, Category: CategoryActionable, ErrMsg: "read-only",
		HTTPStatus: 403,
	}, nil))

	// Clear all scanner-detectable items (empty = all resolved).
	testEngineFlow(t, eng).clearResolvedSkippedItems(ctx, nil)

	// Runtime failure should survive — clearResolvedSkippedItems only
	// clears scanner-detectable issue types.
	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, IssuePermissionDenied, remaining[0].IssueType)
}

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_HashPanicAutoClearsWhenScanRecovers(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "fragile.bin", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueHashPanic, Category: CategoryActionable, ErrMsg: "panic: boom",
	}, nil))

	testEngineFlow(t, eng).clearResolvedSkippedItems(ctx, nil)

	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "scanner-time hash panics should auto-clear after a healthy scan")
}

// Validates: R-2.12.2
func TestClearResolvedSkippedItems_CaseCollision(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record case collision failures.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "File.txt", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueCaseCollision, Category: CategoryActionable,
		ErrMsg: "conflicts with file.txt",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "file.txt", DriveID: driveID, Direction: DirectionUpload,
		IssueType: IssueCaseCollision, Category: CategoryActionable,
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
	testWatchRuntime(t, eng).scopeState = NewScopeState(time.Now, eng.logger)

	// Feed several local errors (HTTPStatus=0) — should not trigger a block scope.
	for i := range 10 {
		feedScopeDetectionForTest(t, eng, t.Context(), &ActionCompletion{
			Path:       fmt.Sprintf("file-%d.txt", i),
			ActionType: ActionDownload,
			HTTPStatus: 0, // local error — no HTTP status
			Err:        os.ErrPermission,
			ErrMsg:     "permission denied",
		})
	}

	// No block scope should have been created.
	assert.False(t, isTestBlockScopeed(eng, SKService()),
		"local errors with HTTPStatus=0 must not trigger service scope")
	assert.False(t, isTestBlockScopeed(eng, testThrottleScope()),
		"local errors with HTTPStatus=0 must not trigger throttle scope")
}

// Validates: R-2.10.30
func TestIsObservationSuppressed_TargetScopedThrottleDoesNotSuppressAllShortcuts(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)

	// Initially not suppressed.
	assert.False(t, isObservationSuppressedForTest(t, eng, testWatchRuntime(t, eng)))

	// Target-scoped throttle should not suppress all shared observation.
	setTestBlockScope(t, eng, &BlockScope{
		Key:           testThrottleScope(),
		IssueType:     IssueRateLimited,
		TrialInterval: 30 * time.Second,
	})
	assert.False(t, isObservationSuppressedForTest(t, eng, testWatchRuntime(t, eng)))
}

// Validates: R-2.10.30
func TestIsObservationSuppressed_ServiceOutage(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)

	// Service outage should also suppress.
	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
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

	// Quota block scope should NOT suppress observation (R-2.10.31).
	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKQuotaOwn(),
		IssueType:     IssueQuotaExceeded,
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
	report, err := eng.RunOnce(ctx, SyncDownloadOnly, RunOptions{})
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
		{"429_rate_limited", http.StatusTooManyRequests, nil, IssueRateLimited},
		{"401_unauthorized", http.StatusUnauthorized, graph.ErrUnauthorized, IssueUnauthorized},
		{"507_quota_exceeded", http.StatusInsufficientStorage, nil, IssueQuotaExceeded},
		{"403_permission_denied", http.StatusForbidden, nil, IssuePermissionDenied},
		{"400_invalid_request", http.StatusBadRequest, genericInvalidRequestErr, ""},
		{"400_object_handle_message_only", http.StatusBadRequest, objectHandleErr, ""},
		{"400_normal", http.StatusBadRequest, errors.New("bad request"), ""},
		{"500_service_outage", http.StatusInternalServerError, nil, IssueServiceOutage},
		{"503_service_outage", http.StatusServiceUnavailable, nil, IssueServiceOutage},
		{"408_request_timeout", http.StatusRequestTimeout, nil, "request_timeout"},
		{"412_transient_conflict", http.StatusPreconditionFailed, nil, "transient_conflict"},
		{"404_transient_not_found", http.StatusNotFound, nil, "transient_not_found"},
		{"423_resource_locked", http.StatusLocked, nil, "resource_locked"},
		{"permission_error", 0, os.ErrPermission, IssueLocalPermissionDenied},
		// Validates: R-2.10.43
		{"disk_full", 0, driveops.ErrDiskFull, IssueDiskFull},
		{"wrapped_disk_full", 0, fmt.Errorf("download: %w", driveops.ErrDiskFull), IssueDiskFull},
		// Validates: R-2.10.44
		{"file_too_large_for_space", 0, driveops.ErrFileTooLargeForSpace, IssueFileTooLargeForSpace},
		{"file_exceeds_onedrive_limit", 0, driveops.ErrFileExceedsOneDriveLimit, IssueFileTooLarge},
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
func TestLogFailureSummary_AggregatesByIssueTypeAboveThreshold(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := newEngineFlow(eng.Engine)

	for i := range 11 {
		result := &ActionCompletion{
			ActionID:   int64(i + 1),
			Path:       fmt.Sprintf("bulk-%02d.txt", i),
			ActionType: ActionUpload,
			HTTPStatus: http.StatusServiceUnavailable,
			Err:        fmt.Errorf("service unavailable %02d", i),
			ErrMsg:     fmt.Sprintf("service unavailable %02d", i),
		}
		decision := classifyResult(result)
		require.Equal(t, IssueServiceOutage, decision.IssueType)

		flow.recordError(&decision, result)
	}

	flow.logFailureSummary()

	output := logBuf.String()
	assert.Equal(t, 1, strings.Count(output, "level=WARN msg=\"sync failures (aggregated)\""))
	assert.Equal(t, 11, strings.Count(output, "level=DEBUG msg=\"sync failure\""))
	assert.Contains(t, output, "issue_type=service_outage")
	assert.Contains(t, output, "count=11")
	assert.Contains(t, output, "bulk-00.txt")
	assert.Contains(t, output, "bulk-10.txt")

	assert.Len(t, flow.syncErrors, 11, "raw report errors should remain available after summary logging")
	assert.Empty(t, flow.summaries, "summary entries should be cleared after logging")
}

// Validates: R-6.6.12
func TestLogFailureSummary_BelowThresholdWarnsPerItem(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	flow := newEngineFlow(eng.Engine)

	for i := range 2 {
		result := &ActionCompletion{
			ActionID:   int64(i + 1),
			Path:       fmt.Sprintf("small-%02d.txt", i),
			ActionType: ActionDownload,
			HTTPStatus: http.StatusGatewayTimeout,
			Err:        fmt.Errorf("gateway timeout %02d", i),
			ErrMsg:     fmt.Sprintf("gateway timeout %02d", i),
		}
		decision := classifyResult(result)
		require.Equal(t, IssueServiceOutage, decision.IssueType)

		flow.recordError(&decision, result)
	}

	flow.logFailureSummary()

	output := logBuf.String()
	assert.Equal(t, 0, strings.Count(output, "level=WARN msg=\"sync failures (aggregated)\""))
	assert.Equal(t, 2, strings.Count(output, "level=WARN msg=\"sync failure\""))
	assert.Contains(t, output, "path=small-00.txt")
	assert.Contains(t, output, "path=small-01.txt")
	assert.Contains(t, output, "issue_type=service_outage")
}

// Validates: R-6.6.12
func TestLogFailureSummary_NoTransientSummariesNoops(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := newEngineFlow(eng.Engine)

	flow.logFailureSummary()

	assert.Empty(t, flow.syncErrors)
	assert.Empty(t, flow.summaries)
}

// ---------------------------------------------------------------------------
// Retrier pipeline integration test (single-owner architecture)
//
// Exercises the integrated retrier: action → failure → sync_failures
// → retry timer fires → runRetrierSweep → dirty replan / action rebuild.
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
	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-abc",
			Path:     testPath,
			ItemType: ItemTypeFile,
			Hash:     "report-hash",
			Size:     4096,
		},
	}, "", driveID))

	// Add action to depGraph, send to dispatchCh, drain it.
	ta := testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type: ActionDownload, Path: testPath, DriveID: driveID, ItemID: "item-abc",
	}, 0, nil)
	require.NotNil(t, ta)
	testWatchRuntime(t, eng).dispatchCh <- ta
	readReady(t, testWatchRuntime(t, eng).dispatchCh)

	// Use a nowFn that's 1 hour in the future so retrier sees rows as due.
	futureTime := time.Now().Add(time.Hour)
	eng.nowFn = func() time.Time { return futureTime }

	// Send a 503 result — classifies as resultRequeue (transient).
	results <- ActionCompletion{
		ActionID:   0,
		Path:       testPath,
		ActionType: ActionDownload,
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
	assert.Equal(t, ActionDownload, outbox[0].Action.Type)
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
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: testPath, DriveID: driveID, Direction: DirectionDownload,
		Category: CategoryTransient, ErrMsg: "previous failure",
		HTTPStatus: http.StatusServiceUnavailable,
	}, func(int) time.Duration { return time.Hour }))

	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "seeded failure should exist")

	// Add action, send to dispatchCh, drain it.
	ta := testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type: ActionDownload, Path: testPath, DriveID: driveID, ItemID: "item-ok",
	}, 0, nil)
	require.NotNil(t, ta)
	testWatchRuntime(t, eng).dispatchCh <- ta
	readReady(t, testWatchRuntime(t, eng).dispatchCh)

	// Send a success result — defensive clear removes the row.
	results <- ActionCompletion{
		ActionID: 0, Path: testPath, ActionType: ActionDownload,
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

// Validates: R-2.10.41
func TestClearFailureOnSuccess_RemovesFailureRow(t *testing.T) {
	// Verify that clearFailureOnSuccess removes a previously recorded
	// sync_failures row, confirming the engine-owns-failure-lifecycle
	// contract from D-6.
	ctx := context.Background()
	eng, _ := newTestEngine(t, &engineMockClient{})
	driveID := driveid.New(engineTestDriveID)

	// Record a failure for the test path.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "clear-test/file.txt",
		DriveID:   driveID,
		Direction: DirectionDownload,
		Category:  CategoryTransient,
		ErrMsg:    "test error",
	}, nil))

	// Confirm the failure exists.
	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "failure should be recorded")

	// clearFailureOnSuccess should remove it.
	flow := newEngineFlow(eng.Engine)
	flow.clearFailureOnSuccess(ctx, &ActionCompletion{
		Path:       "clear-test/file.txt",
		DriveID:    driveID,
		ActionType: ActionDownload,
		Success:    true,
	})

	// Verify the failure is gone.
	rows, err = eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "failure should be cleared after success")
}

// Validates: R-2.10.41
func TestClearFailureOnSuccess_FallbackDriveID(t *testing.T) {
	// When ActionCompletion.DriveID is zero, clearFailureOnSuccess falls back
	// to the engine's own driveID. This covers own-drive actions where the
	// worker doesn't set an explicit drive ID.
	ctx := context.Background()
	eng, _ := newTestEngine(t, &engineMockClient{})
	driveID := driveid.New(engineTestDriveID)

	// Record a failure using the engine's own drive ID.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "fallback-test/file.txt",
		DriveID:   driveID,
		Direction: DirectionUpload,
		Category:  CategoryTransient,
		ErrMsg:    "quota exceeded",
	}, nil))

	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "failure should be recorded")

	// Call clearFailureOnSuccess with a zero DriveID — should fall back
	// to eng.driveID and still clear the failure.
	flow := newEngineFlow(eng.Engine)
	flow.clearFailureOnSuccess(ctx, &ActionCompletion{
		Path:       "fallback-test/file.txt",
		DriveID:    driveid.ID{}, // zero value
		ActionType: ActionUpload,
		Success:    true,
	})

	rows, err = eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "failure should be cleared via fallback drive ID")
}

// Validates: R-6.6.9
func TestClearFailureOnSuccess_LogsResolvedTransientFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "resolved-worker/file.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "server error",
		HTTPStatus: http.StatusServiceUnavailable,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "resolved-worker/file.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "server error again",
		HTTPStatus: http.StatusServiceUnavailable,
	}, nil))

	flow := newEngineFlow(eng.Engine)
	flow.clearFailureOnSuccess(ctx, &ActionCompletion{
		Path:       "resolved-worker/file.txt",
		DriveID:    driveID,
		ActionType: ActionUpload,
		Success:    true,
	})

	output := logBuf.String()
	assert.Contains(t, output, "level=INFO msg=\"transient failure resolved\"")
	assert.Contains(t, output, "path=resolved-worker/file.txt")
	assert.Contains(t, output, "drive_id="+driveID.String())
	assert.Contains(t, output, "issue_type=service_outage")
	assert.Contains(t, output, "action_type=upload")
	assert.Contains(t, output, "attempt_count=2")
	assert.Contains(t, output, "resolution_source=worker_success")
}

// Validates: R-6.6.9
func TestIsFailureResolved_LogsRetryResolutionForTransientItemFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "resolved-retry/file.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "server error",
		HTTPStatus: http.StatusServiceUnavailable,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "resolved-retry/file.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "server error again",
		HTTPStatus: http.StatusServiceUnavailable,
	}, nil))

	flow := newEngineFlow(eng.Engine)
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	candidate := flow.buildRetryCandidateFromRetryWork(ctx, bl, &RetryWorkRow{
		Path:       "resolved-retry/file.txt",
		ActionType: ActionUpload,
	}, driveID)
	assert.True(t, candidate.resolved)
	flow.clearRetryWorkCandidate(
		ctx,
		retryWorkKey("resolved-retry/file.txt", "", ActionUpload),
		driveID,
		"TestProcessActionCompletion_ClearsResolvedFailure",
	)

	output := logBuf.String()
	assert.Contains(t, output, "level=INFO msg=\"transient failure resolved\"")
	assert.Contains(t, output, "path=resolved-retry/file.txt")
	assert.Contains(t, output, "drive_id="+driveID.String())
	assert.Contains(t, output, "issue_type=service_outage")
	assert.Contains(t, output, "action_type=upload")
	assert.Contains(t, output, "attempt_count=2")
	assert.Contains(t, output, "resolution_source=retry_resolution")
}

// Validates: R-6.6.9
func TestClearFailureOnSuccess_DoesNotLogActionableResolution(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "actionable/file.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Category:   CategoryActionable,
		IssueType:  IssueInvalidFilename,
		ErrMsg:     "reserved name",
	}, nil))

	flow := newEngineFlow(eng.Engine)
	flow.clearFailureOnSuccess(ctx, &ActionCompletion{
		Path:       "actionable/file.txt",
		DriveID:    driveID,
		ActionType: ActionUpload,
		Success:    true,
	})

	assert.NotContains(t, logBuf.String(), "msg=\"transient failure resolved\"")
}

// Validates: R-6.6.9
func TestClearFailureCandidate_DoesNotLogHeldScopeResolution(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "held/file.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Category:   CategoryTransient,
		IssueType:  IssueServiceOutage,
		ErrMsg:     "service unavailable",
		HTTPStatus: http.StatusServiceUnavailable,
		ScopeKey:   SKService(),
		Role:       FailureRoleHeld,
	}, nil))

	flow := newEngineFlow(eng.Engine)
	flow.clearRetryWorkCandidate(
		ctx,
		retryWorkKey("held/file.txt", "", ActionUpload),
		driveID,
		"TestClearFailureCandidate_DoesNotLogHeldScopeResolution",
	)

	assert.NotContains(t, logBuf.String(), "msg=\"transient failure resolved\"")
}
