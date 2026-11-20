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
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// goneItemID is the remote item a stopping run's download found missing.
const goneItemID = "gone-item"

// failedUploads registers an upload for each path in actions, numbered from
// firstID, and returns what each reported as its run stopped: a 502 from
// OneDrive, a result that already happened.
func failedUploads(actions *DepGraph, firstID int64, paths ...string) []ActionCompletion {
	completions := make([]ActionCompletion, 0, len(paths))
	for i, path := range paths {
		id := firstID + int64(i)
		actions.Add(&Action{Type: ActionUpload, Path: path, View: &PathView{Path: path}}, id, nil)
		completions = append(completions, ActionCompletion{
			ActionID: id, Path: path, ActionType: ActionUpload, RunEnded: true,
			HTTPStatus: http.StatusBadGateway, Err: graph.ErrServerError, ErrMsg: "bad gateway",
		})
	}

	return completions
}

// stoppingRunCompletions registers three actions in actions, seeds the remote
// row of the item one of them finds gone, and returns what each reported as
// its run stopped: an upload OneDrive answered with a 502, a download whose
// item is gone, and a folder create whose outcome commit outlasted its bound.
// Each is a result that already happened.
func stoppingRunCompletions(t *testing.T, eng *testEngine, actions *DepGraph) []ActionCompletion {
	t.Helper()

	require.NoError(t, eng.baseline.CommitObservation(t.Context(), []ObservedItem{{
		DriveID:  eng.driveID,
		ItemID:   goneItemID,
		Path:     "gone.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash-gone",
		Size:     4,
		ETag:     "etag-gone",
	}}, "delta-1", eng.driveID))

	completions := failedUploads(actions, 1, "report.txt")
	actions.Add(&Action{
		Type: ActionDownload, Path: "gone.txt", ItemID: goneItemID, DriveID: eng.driveID, View: &PathView{Path: "gone.txt"},
	}, 2, nil)
	actions.Add(&Action{Type: ActionFolderCreate, Path: "Projects", View: &PathView{Path: "Projects"}}, 3, nil)

	return append(completions,
		ActionCompletion{
			ActionID: 2, Path: "gone.txt", ActionType: ActionDownload, RunEnded: true,
			Err: &RemoteItemGoneError{Op: "download", ItemID: goneItemID, Role: "item"},
		},
		ActionCompletion{
			ActionID: 3, Path: "Projects", ActionType: ActionFolderCreate, RunEnded: true,
			Err: fmt.Errorf("sync: commit outcome: %w: %w", context.DeadlineExceeded, ErrStoreAccessTimedOut),
		},
	)
}

// retryWorkPaths returns the paths of the retry work store holds.
func retryWorkPaths(t *testing.T, store *SyncStore) []string {
	t.Helper()

	rows := listRetryWorkForTest(t, store, t.Context())
	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}

	return paths
}

// requireStoppingRunRecorded holds that each result stoppingRunCompletions
// returns is in the store: both failures as retry work, the item proven gone
// forgotten.
func requireStoppingRunRecorded(t *testing.T, eng *testEngine) {
	t.Helper()

	assert.ElementsMatch(t, []string{"report.txt", "Projects"}, retryWorkPaths(t, eng.baseline),
		"each failure is recorded as retry work")

	_, found, err := eng.baseline.GetRemoteStateByID(t.Context(), eng.driveID, goneItemID)
	require.NoError(t, err)
	assert.False(t, found, "the item proven gone is forgotten")
}

// closedCompletions returns a closed channel that holds completions.
func closedCompletions(completions []ActionCompletion) chan ActionCompletion {
	ch := make(chan ActionCompletion, len(completions))
	for i := range completions {
		ch <- completions[i]
	}
	close(ch)

	return ch
}

// stoppedContext returns a context that has already been canceled, as a
// run's context is once it stops.
func stoppedContext(t *testing.T) context.Context {
	t.Helper()

	ctx, stop := context.WithCancel(t.Context())
	stop()

	return ctx
}

// runWatchDrain runs the drain steps of rt on ctx until the drain is done and
// returns the step's error. It takes at most one step per completion in
// p.completions and one for its close, so a drain that never finishes is an
// error instead of a spin.
func runWatchDrain(ctx context.Context, rt *watchRuntime, p *watchPipeline) error {
	for range len(p.completions) + 1 {
		done, err := rt.runDrainStep(ctx, p)
		if err != nil || done {
			return err
		}
	}

	return errors.New("the drain did not finish within one step per completion")
}

// holdStoreConnection takes the store's one connection, so every access of
// the store waits until its context ends, and returns the release, which a
// cleanup also runs.
func holdStoreConnection(t *testing.T, store *SyncStore) func() {
	t.Helper()

	conn, err := store.RawDBForTest().Conn(t.Context())
	require.NoError(t, err)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		assert.NoError(t, conn.Close())
	}
	t.Cleanup(release)

	return release
}

// stoppingWatchRuntime returns a watch runtime whose stop bound is short and
// whose log the test reads.
func stoppingWatchRuntime(t *testing.T) (*testEngine, *watchRuntime, *syncLockedBuffer) {
	t.Helper()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.stopBound = workerTestCommitBound

	return eng, rt, logs
}

// A one-shot pass canceled with completions in flight records each result
// that already happened, on a context the stop does not cancel: a failure as
// retry work, an item proven gone as gone.
//
// Validates: R-2.8.3
func TestOneShotEngineLoop_ACanceledPassRecordsTheCompletionsItDrains(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *TrackedAction, 1)
	completions := closedCompletions(stoppingRunCompletions(t, eng, runner.sched.graph))

	err := runWithin(t, stoppedContext(t), "the canceled pass to drain its completions", func(ctx context.Context) error {
		return runner.runResultsLoopWithInitialOutbox(ctx, nil, nil, completions, nil)
	})
	require.NoError(t, err)

	requireStoppingRunRecorded(t, eng)
	require.Eventually(t, func() bool { return runner.canceledAt.Load() != nil }, eventWait, time.Millisecond,
		"the pass records its cancel, which its stop's bound starts at")
}

// A watch that stops records each result that already happened among the
// completions its drain receives, as a one-shot pass does.
//
// Validates: R-2.8.3
func TestWatchRuntime_ADrainRecordsTheCompletionsItReceives(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	p := &watchPipeline{completions: closedCompletions(stoppingRunCompletions(t, eng, rt.sched.graph))}
	stopped := stoppedContext(t)
	rt.beginWatchDrain(stopped, p)

	require.NoError(t, runWatchDrain(stopped, rt, p))

	requireStoppingRunRecorded(t, eng)
}

// A store that does not answer cannot hold a stop past its one bound: the
// drain's records wait out the bound, the results it cut are said once with
// their count, and the drain ends with the store failure the sync database's
// silence is. That every record shares the one bound is
// TestWatchRuntime_EveryRecordOfAStopSharesItsOneBound.
//
// Validates: R-6.3.9, R-2.8.3
func TestWatchRuntime_AStoreThatDoesNotAnswerEndsTheDrainAtItsBound(t *testing.T) {
	t.Parallel()

	eng, rt, logs := stoppingWatchRuntime(t)
	p := &watchPipeline{completions: closedCompletions(failedUploads(rt.sched.graph, 1, "a.txt", "b.txt"))}
	stopped := stoppedContext(t)
	rt.beginWatchDrain(stopped, p)

	release := holdStoreConnection(t, eng.baseline)
	err := runWithin(t, t.Context(), "the drain to end at its bound", func(context.Context) error {
		return runWatchDrain(stopped, rt, p)
	})
	release()

	require.ErrorIs(t, err, ErrStoreAccessTimedOut)
	require.ErrorContains(t, err, "the stop's time ran out before recording 2 of its results")
	assert.Empty(t, retryWorkPaths(t, eng.baseline), "the bound cut both records")
	assert.Equal(t, 1, countLevel(logs.String(), "WARN"), "the loss is said once: %s", logs.String())
	assert.Contains(t, logs.String(), "The stop's time ran out before 2 results were recorded")
}

// Every record of a stop shares its one bound, the nested clear of a
// success's retry work too: once the bound is spent, a record is not tried on
// a bound of its own even when the store answers again, so the records of a
// drain of any number of completions end within one bound, which the watch
// control plane derives its limit on a drive's stop from.
//
// Validates: R-6.3.9
func TestWatchRuntime_EveryRecordOfAStopSharesItsOneBound(t *testing.T) {
	t.Parallel()

	eng, rt, logs := stoppingWatchRuntime(t)
	succeeded := []string{"first.txt", "second.txt"}
	completions := make([]ActionCompletion, 0, len(succeeded))
	for i, path := range succeeded {
		_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), testRetryWorkFailure(path, "", ActionUpload),
			retryDelayForDecision(nil))
		require.NoError(t, err)
		id := int64(i + 1)
		rt.sched.graph.Add(&Action{Type: ActionUpload, Path: path, View: &PathView{Path: path}}, id, nil)
		completions = append(completions, ActionCompletion{
			ActionID: id, Path: path, ActionType: ActionUpload, Success: true, RunEnded: true,
		})
	}
	p := &watchPipeline{}
	stopped := stoppedContext(t)
	rt.beginWatchDrain(stopped, p)

	release := holdStoreConnection(t, eng.baseline)
	_, err := rt.handleDrainingCompletion(stopped, p, &completions[0], true)
	require.NoError(t, err)
	release()
	_, err = rt.handleDrainingCompletion(stopped, p, &completions[1], true)
	require.NoError(t, err)

	assert.ElementsMatch(t, succeeded, retryWorkPaths(t, eng.baseline), "no clear ran past the stop's one bound")
	require.ErrorIs(t, rt.endStopDrain(), ErrStoreAccessTimedOut)
	assert.Contains(t, logs.String(), "The stop's time ran out before 2 results were recorded")
}

// The stop's one bound starts at the cancel, not when the run first sees its
// stop: a cancel that lands while the run's goroutine waits on a store that
// does not answer leaves the drain only what remains of the bound, so the
// stop still ends within one bound of its cancel.
//
// Validates: R-6.3.9, R-2.8.3
func TestWatchRuntime_AStopsBoundStartsAtItsCancel(t *testing.T) {
	t.Parallel()

	eng, rt, _ := stoppingWatchRuntime(t)
	rt.stopBound = time.Hour
	run, cancelRun := context.WithCancel(t.Context())
	stopNoting := rt.noteRunCancel(run)
	defer stopNoting()

	release := holdStoreConnection(t, eng.baseline)
	err := RunDetachedStoreAccess(run, workerTestCommitBound, func(ctx context.Context) error {
		cancelRun()
		_, listErr := eng.baseline.ListBlockScopes(ctx)

		return listErr
	})
	release()
	require.ErrorIs(t, err, ErrStoreAccessTimedOut, "the run's goroutine waited out the store")
	require.Eventually(t, func() bool { return rt.canceledAt.Load() != nil }, eventWait, time.Millisecond,
		"the cancel is recorded")

	seen := time.Now()
	rt.beginWatchDrain(run, &watchPipeline{})

	require.NotNil(t, rt.stop)
	assert.True(t, rt.stop.deadline.Equal(rt.canceledAt.Load().Add(rt.stopBound)),
		"the bound ends one bound after the cancel: %v", rt.stop.deadline)
	assert.True(t, rt.stop.deadline.Before(seen.Add(rt.stopBound)),
		"the bound does not start when the run first saw its stop")
}

// A store access detached inside a context the helper already detached keeps
// that context's bound instead of starting one of its own, so the accesses of
// one stop add up to one bound; the values of the run's context are kept and
// its cancel is not.
//
// Validates: R-6.3.9
func TestRunDetachedStoreAccess_ANestedAccessKeepsTheOuterBound(t *testing.T) {
	t.Parallel()

	type runValueKey struct{}
	run, stop := context.WithCancel(context.WithValue(t.Context(), runValueKey{}, "kept"))
	stop()
	outer, cancel := DetachedStoreContext(run, time.Now().Add(time.Hour))
	defer cancel()
	outerDeadline, bounded := outer.Deadline()
	require.True(t, bounded, "a detached context has a deadline")

	var innerDeadline time.Time
	require.NoError(t, RunDetachedStoreAccess(outer, OutcomeCommitTimeout, func(ctx context.Context) error {
		innerDeadline, _ = ctx.Deadline()
		assert.Equal(t, "kept", ctx.Value(runValueKey{}), "the run's values are kept")

		return ctx.Err()
	}))
	assert.Equal(t, outerDeadline, innerDeadline, "the nested access ends with the outer bound, not a new one")
}

// A one-shot pass that is canceled seals its outbox, as a watch's drain does:
// work admitted but not yet handed to a worker completes as a shutdown at
// once instead of being offered to the workers after the stop, and the pass
// waits only for what is in flight.
//
// Validates: R-2.8.3
func TestOneShotEngineLoop_ACanceledPassSealsItsOutbox(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = NewDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *TrackedAction)
	admitted := runner.sched.graph.Add(&Action{Type: ActionUpload, Path: "queued.txt", View: &PathView{Path: "queued.txt"}}, 1, nil)
	require.NotNil(t, admitted)

	err := runWithin(t, stoppedContext(t), "the canceled pass to seal its outbox", func(ctx context.Context) error {
		return runner.runResultsLoopWithInitialOutbox(ctx, nil, nil, make(chan ActionCompletion), []*TrackedAction{admitted})
	})
	require.NoError(t, err)

	assert.Empty(t, runner.outbox)
	assert.Zero(t, runner.sched.graph.InFlightCount(), "the admitted work completed as a shutdown")
}

// A stopping run starts nothing new, a request to OneDrive least of all: a
// 403 it drains is recorded as the retry work it is, and the permission check
// a live run would make waits for the next sync.
//
// Validates: R-2.8.3
func TestEngineFlow_AStoppingRunAsksOneDriveNothing(t *testing.T) {
	t.Parallel()

	const mountRoot = "stop-mount-root"
	probes := 0
	eng, _ := newTestEngine(t, &engineMockClient{
		listItemPermissionsFn: func(context.Context, driveid.ID, string) ([]graph.Permission, error) {
			probes++
			return []graph.Permission{{Roles: []string{"read"}}}, nil
		},
	})
	eng.remoteRootItemID = mountRoot
	eng.permHandler.remoteRootItemID = mountRoot
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	flow.sched.graph.Add(&Action{Type: ActionUpload, Path: "Shared/file.txt", View: &PathView{Path: "Shared/file.txt"}}, 1, nil)
	refused := ActionCompletion{
		ActionID: 1, Path: "Shared/file.txt", ActionType: ActionUpload, RunEnded: true,
		HTTPStatus: http.StatusForbidden, ErrMsg: "folder is read-only",
	}

	require.NoError(t, flow.processShutdownCompletion(stoppedContext(t), &refused, &Baseline{}))
	require.NoError(t, flow.endStopDrain())

	assert.Zero(t, probes, "a stop asks OneDrive nothing")
	assert.Equal(t, []string{"Shared/file.txt"}, retryWorkPaths(t, eng.baseline), "the refusal is recorded")
}

// A stopping run's scope trial asks OneDrive nothing either: a trial refused
// with a 403 is recorded as the retry work it is, released from its scope,
// and the permission check a live run would make waits for the next sync.
//
// Validates: R-2.8.3
func TestEngineFlow_AStoppingRunAsksOneDriveNothingForATrial(t *testing.T) {
	t.Parallel()

	const (
		mountRoot = "stop-mount-root"
		trialPath = "Shared/file.txt"
	)
	probes := 0
	eng, _ := newTestEngine(t, &engineMockClient{
		listItemPermissionsFn: func(context.Context, driveid.ID, string) ([]graph.Permission, error) {
			probes++
			return []graph.Permission{{Roles: []string{"read"}}}, nil
		},
	})
	eng.remoteRootItemID = mountRoot
	eng.permHandler.remoteRootItemID = mountRoot
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := SKService()
	setTestBlockScope(t, eng, &BlockScope{Key: scopeKey, NextTrialAt: eng.nowFunc()})
	blocked := &RetryWorkRow{Path: trialPath, ActionType: ActionUpload, ScopeKey: scopeKey, Blocked: true, AttemptCount: 1}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), blocked))
	flow.retryWork.rows[blocked.WorkKey()] = *blocked
	flow.sched.graph.Add(&Action{Type: ActionUpload, Path: trialPath, View: &PathView{Path: trialPath}}, 1, nil)
	refused := ActionCompletion{
		ActionID: 1, Path: trialPath, ActionType: ActionUpload, RunEnded: true,
		HTTPStatus: http.StatusForbidden, ErrMsg: "folder is read-only",
		IsTrial: true, TrialScopeKey: scopeKey,
	}

	require.NoError(t, flow.processShutdownCompletion(stoppedContext(t), &refused, &Baseline{}))
	require.NoError(t, flow.endStopDrain())

	assert.Zero(t, probes, "a stop asks OneDrive nothing for a trial")
	rows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, rows, 1)
	assert.Equal(t, trialPath, rows[0].Path)
	assert.False(t, rows[0].Blocked, "the refusal is recorded as retry work, not as a probed boundary")
}

// unwritableStopFolder is the folder a stopping run's download was refused
// in: listable, not writable, so a probe of it would find a write boundary.
const unwritableStopFolder = "ro"

// makeUnwritableStopFolder creates unwritableStopFolder under syncRoot and
// takes its write access away for the rest of the test.
func makeUnwritableStopFolder(t *testing.T, syncRoot string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere; the folder cannot refuse a write")
	}

	folder := filepath.Join(syncRoot, unwritableStopFolder)
	require.NoError(t, os.MkdirAll(folder, 0o750))
	makeReadOnlyForTest(t, folder)
}

// A stopping run probes the disk no more than OneDrive: a download refused
// with a local permission error is recorded as the retry work it is, and the
// folder check a live run would make waits for the next sync.
//
// Validates: R-2.8.3
func TestEngineFlow_AStoppingRunProbesNoLocalFolder(t *testing.T) {
	t.Parallel()

	const refusedPath = unwritableStopFolder + "/file.txt"
	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	makeUnwritableStopFolder(t, syncRoot)
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	flow.sched.graph.Add(&Action{Type: ActionDownload, Path: refusedPath, View: &PathView{Path: refusedPath}}, 1, nil)
	refused := ActionCompletion{
		ActionID: 1, Path: refusedPath, ActionType: ActionDownload, RunEnded: true,
		Err: os.ErrPermission, ErrMsg: "permission denied", FailureCapability: PermissionCapabilityLocalWrite,
	}

	require.NoError(t, flow.processShutdownCompletion(stoppedContext(t), &refused, &Baseline{}))
	require.NoError(t, flow.endStopDrain())

	rows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, rows, 1)
	assert.Equal(t, refusedPath, rows[0].Path)
	assert.False(t, rows[0].Blocked, "the refusal is recorded as retry work, not as a probed boundary")
	assert.True(t, rows[0].ScopeKey.IsZero(), "no folder scope holds the refusal")
	assert.False(t, isTestBlockScopeed(eng, SKPermLocalWrite(unwritableStopFolder)), "the folder was not probed")
}

// A stopping run's scope trial probes no local folder either: a trial refused
// with a local permission error is recorded as the retry work it is, released
// from its scope, and the folder check waits for the next sync.
//
// Validates: R-2.8.3
func TestEngineFlow_AStoppingRunProbesNoLocalFolderForATrial(t *testing.T) {
	t.Parallel()

	const trialPath = unwritableStopFolder + "/file.txt"
	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	makeUnwritableStopFolder(t, syncRoot)
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := SKService()
	setTestBlockScope(t, eng, &BlockScope{Key: scopeKey, NextTrialAt: eng.nowFunc()})
	blocked := &RetryWorkRow{Path: trialPath, ActionType: ActionDownload, ScopeKey: scopeKey, Blocked: true, AttemptCount: 1}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), blocked))
	flow.retryWork.rows[blocked.WorkKey()] = *blocked
	flow.sched.graph.Add(&Action{Type: ActionDownload, Path: trialPath, View: &PathView{Path: trialPath}}, 1, nil)
	refused := ActionCompletion{
		ActionID: 1, Path: trialPath, ActionType: ActionDownload, RunEnded: true,
		Err: os.ErrPermission, ErrMsg: "permission denied", FailureCapability: PermissionCapabilityLocalWrite,
		IsTrial: true, TrialScopeKey: scopeKey,
	}

	require.NoError(t, flow.processShutdownCompletion(stoppedContext(t), &refused, &Baseline{}))
	require.NoError(t, flow.endStopDrain())

	rows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, rows, 1)
	assert.Equal(t, trialPath, rows[0].Path)
	assert.False(t, rows[0].Blocked, "the refusal is recorded as retry work, not as a probed boundary")
	assert.False(t, isTestBlockScopeed(eng, SKPermLocalWrite(unwritableStopFolder)), "the folder was not probed")
}

// The watch records the cancel of the context it runs on, so the bound of its
// stop starts at that cancel (TestWatchRuntime_AStopsBoundStartsAtItsCancel).
//
// Validates: R-6.3.9
func TestRunWatch_RecordsTheCancelOfItsRun(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	rt := newWatchRuntime(eng.Engine)

	require.NoError(t, eng.runWatch(stoppedContext(t), rt, SyncBidirectional, WatchOptions{}))
	require.Eventually(t, func() bool { return rt.canceledAt.Load() != nil }, eventWait, time.Millisecond,
		"the watch records its cancel")
}

// A stopping run releases no due held work: an action held for a retry that is
// already due stays held when the stop drains a completion, instead of being
// released and completed as a shutdown, and the next sync retries it.
//
// Validates: R-2.8.3
func TestEngineFlow_AStoppingRunReleasesNoHeldWork(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	completions := failedUploads(flow.sched.graph, 1, "report.txt")
	held := flow.sched.graph.Add(&Action{Type: ActionUpload, Path: "held.txt", View: &PathView{Path: "held.txt"}}, 2, nil)
	require.NotNil(t, held)
	flow.holdAction(held, heldReasonRetry, ScopeKey{}, flow.deps.now().Add(-time.Minute))

	require.NoError(t, flow.processShutdownCompletion(stoppedContext(t), &completions[0], &Baseline{}))
	require.NoError(t, flow.endStopDrain())

	assert.Contains(t, flow.retries.heldByKey, RetryWorkKeyForAction(&held.Action), "the due action stays held")
}
