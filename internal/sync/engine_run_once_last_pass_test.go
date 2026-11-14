package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-6.6.28
//
// A pass that ends at its first dispatch, here because admission is refused
// once the run is canceled, reports the work it planned and neither completed
// nor failed as left for the next sync, by the same count as every other end
// of a pass.
func TestExecutePreparedPlan_APassThatEndsAtItsFirstDispatchLeavesItsWork(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	report := &Report{}
	runtime := &runtimePlan{
		Plan: &actionPlan{
			Actions: []Action{
				{Type: ActionUpload, Path: "a.txt", View: &pathView{Path: "a.txt"}},
				{Type: ActionUpload, Path: "b.txt", View: &pathView{Path: "b.txt"}},
			},
			Deps: [][]int{nil, nil},
		},
		Report: report,
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := newOneShotRunner(eng.Engine).executePreparedPlan(ctx, runtime, &Baseline{})
	require.Error(t, err, "admission refuses a canceled run")
	assert.Zero(t, report.Succeeded+report.Failed, "%+v", report)
	assert.Equal(t, len(runtime.Plan.Actions), report.LeftForNextSync, "neither completed nor failed: left")
	assert.Equal(t, LeftWork{WillRetry: len(runtime.Plan.Actions)}, report.LeftWork())
}

// lastPassTestThrottle answers OneDrive's 429 with a Retry-After.
func lastPassTestThrottle(retryAfter time.Duration) error {
	return &graph.GraphError{
		StatusCode: http.StatusTooManyRequests,
		Code:       "activityLimitReached",
		Err:        graph.ErrThrottled,
		RetryAfter: retryAfter,
	}
}

// throttledUploadTestEngine is an engine with two new local files and a
// remote whose first two uploads, made together, are refused by a 30 s
// throttle; answer decides every later upload, by its number from 3 on.
func throttledUploadTestEngine(
	t *testing.T,
	answer func(call int32) error,
) (*testEngine, *moveScenarioRemote) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	// Creates go through Upload, so the refusals below see every one.
	client.uploadCreatingFn = nil
	upload := client.uploadFn
	var calls atomic.Int32
	bothIn := make(chan struct{})
	client.uploadFn = func(
		ctx context.Context, d driveid.ID, parentID, name string, content io.ReaderAt, size int64,
		mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		call := calls.Add(1)
		if call <= 2 {
			if call == 2 {
				close(bothIn)
			}
			select {
			case <-bothIn:
			case <-time.After(5 * time.Second):
			}
			return nil, lastPassTestThrottle(30 * time.Second)
		}
		if err := answer(call); err != nil {
			return nil, err
		}

		return upload(ctx, d, parentID, name, content, size, mtime, progress)
	}

	eng, syncRoot := newTestEngine(t, client)
	eng.transferWorkers = 2
	clock := newManualClock(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	eng.waitFn = func(_ context.Context, d time.Duration) error {
		clock.Advance(d)
		return nil
	}
	writeLocalFile(t, syncRoot, "a.txt", "alpha\n")
	writeLocalFile(t, syncRoot, "b.txt", "beta\n")

	return eng, remote
}

// Validates: R-2.1.7, R-6.6.28
//
// A pause the run waits out, whose trial OneDrive refuses again: the run
// ends with the refused trial kept for retry and the other file never
// dispatched, and it leaves those two files once each, though the first pass
// refused both too. Every retry row the store holds is one of them.
func TestRunOnce_APauseThatRefusesItsTrialAgainLeavesEachFileOnce(t *testing.T) {
	t.Parallel()

	eng, remote := throttledUploadTestEngine(t, func(call int32) error {
		if call == 3 {
			return lastPassTestThrottle(time.Hour)
		}
		return nil
	})

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, report.Failed, "both refusals of the first pass and the refused trial: %+v", report)
	assert.Equal(t, LeftWork{WillRetry: 2}, report.LeftWork(), "%+v", report)
	assert.Zero(t, remote.uploads)

	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Len(t, rows, report.LastPassFailed+report.LeftForNextSync,
		"each retry row is an action the last pass counted: %+v", rows)
}

// Validates: R-2.1.7, R-6.6.28
//
// A pause the run waits out, after which every upload goes through, leaves
// nothing: the first pass's refusals are not work the run left.
func TestRunOnce_APauseWaitedOutLeavesNothing(t *testing.T) {
	t.Parallel()

	eng, remote := throttledUploadTestEngine(t, func(int32) error { return nil })

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, report.Failed, "the first pass's refusals: %+v", report)
	assert.False(t, report.LeftWork().Any(), "%+v", report)
	assert.Equal(t, 2, remote.uploads)
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// supersededMoveTestEngine is an engine whose drive synced dir/a.txt, after
// which this computer renamed dir and added new.txt. OneDrive refuses the
// first move with a 412, so the run passes again; failNew decides the first
// upload of new.txt.
func supersededMoveTestEngine(
	t *testing.T,
	failNew func(syncRoot string) error,
) (*testEngine, *moveScenarioRemote, *engineMockClient, *manualClock) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-dir", "root", "dir")
	remote.addFile(t, "item-a", "item-dir", "a.txt", "alpha\n")
	client := remote.client(t)
	move := client.moveItemOptsFn
	var refused atomic.Int32
	client.moveItemOptsFn = func(
		ctx context.Context, d driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error) {
		if refused.CompareAndSwap(0, 1) {
			return nil, &graph.GraphError{StatusCode: http.StatusPreconditionFailed, Code: "preconditionFailed", Err: graph.ErrPreconditionFailed}
		}

		return move(ctx, d, itemID, newParentID, newName, opts)
	}
	var syncRoot string
	client.uploadCreatingFn = nil
	upload := client.uploadFn
	var uploads atomic.Int32
	client.uploadFn = func(
		ctx context.Context, d driveid.ID, parentID, name string, content io.ReaderAt, size int64,
		mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		if name == "new.txt" && uploads.Add(1) == 1 {
			if err := failNew(syncRoot); err != nil {
				return nil, err
			}
		}

		return upload(ctx, d, parentID, name, content, size, mtime, progress)
	}

	eng, root := newTestEngine(t, client)
	syncRoot = root
	clock := newManualClock(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	runScenarioPass(t, eng)
	require.NoError(t, os.Rename(filepath.Join(syncRoot, "dir"), filepath.Join(syncRoot, "dir-moved")))
	writeLocalFile(t, syncRoot, "new.txt", "new\n")

	return eng, remote, client, clock
}

// lastPassTestServerError is a 500 from OneDrive, a failure kept for retry.
func lastPassTestServerError() error {
	return &graph.GraphError{StatusCode: http.StatusInternalServerError, Err: graph.ErrServerError}
}

// Validates: R-2.1.7, R-6.6.28
//
// A file that failed in a pass and that the next pass of the same run has no
// action for (the user removed it in between) is not left: the run's last
// pass decides, and the failure's retry row went with the action.
func TestRunOnce_AFailureTheNextPassNoLongerNeedsIsNotLeft(t *testing.T) {
	t.Parallel()

	eng, remote, _, _ := supersededMoveTestEngine(t, func(syncRoot string) error {
		if err := os.Remove(filepath.Join(syncRoot, "new.txt")); err != nil {
			return fmt.Errorf("removing new.txt: %w", err)
		}
		return lastPassTestServerError()
	})

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Superseded, "%+v", report)
	assert.Equal(t, 1, report.Failed, "the first pass failed new.txt: %+v", report)
	assert.False(t, report.LeftWork().Any(), "%+v", report)
	assert.Contains(t, remote.tree(), "dir-moved/a.txt")
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// Validates: R-2.1.7, R-6.6.28
//
// A file that failed in a pass and that a later pass of the same run uploads
// is not left.
func TestRunOnce_AFailureALaterPassFinishesIsNotLeft(t *testing.T) {
	t.Parallel()

	var failed atomic.Bool
	eng, remote, client, clock := supersededMoveTestEngine(t, func(string) error {
		failed.Store(true)
		return lastPassTestServerError()
	})
	advanceBeforeTheNextPass(client, clock, &failed)

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Failed, "the first pass failed new.txt: %+v", report)
	assert.False(t, report.LeftWork().Any(), "%+v", report)
	assert.Equal(t, "new\n", remote.tree()["new.txt"], "a later pass uploaded it")
}

// advanceBeforeTheNextPass moves the clock an hour on at the first remote
// observation after ready is set, so the retry of a failure made before it is
// due in the pass that observation starts.
func advanceBeforeTheNextPass(client *engineMockClient, clock *manualClock, ready *atomic.Bool) {
	delta := client.deltaFn
	var advanced atomic.Bool
	client.deltaFn = func(ctx context.Context, d driveid.ID, token string) (*graph.DeltaPage, error) {
		if ready.Load() && advanced.CompareAndSwap(false, true) {
			clock.Advance(time.Hour)
		}
		return delta(ctx, d, token)
	}
}

// refuseGoneCleanupSQL makes the store refuse to remove gone.txt's baseline
// row, so its cleanup, a failed action no activity names, fails.
const refuseGoneCleanupSQL = `CREATE TRIGGER refuse_gone_cleanup BEFORE DELETE ON baseline
	WHEN OLD.path = 'gone.txt' BEGIN SELECT RAISE(ABORT, 'cleanup refused for the test'); END`

// Validates: R-2.1.7, R-6.6.28
//
// A failed cleanup (a baseline row both sides no longer have, which no file
// event names) counts as failed when the run's last pass failed it, and not
// when a later pass of the same run redid it.
func TestRunOnce_AFailedCleanupCountsOnlyWhenTheLastPassFailedIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		redo     bool
		wantLeft LeftWork
	}{
		{name: "failed by the last pass", wantLeft: LeftWork{Failed: 1}},
		{name: "redone by a later pass", redo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFolder("item-dir", "root", "dir")
			remote.addFile(t, "item-a", "item-dir", "a.txt", "alpha\n")
			remote.addFile(t, "item-gone", "root", "gone.txt", "gone\n")
			client := remote.client(t)
			var refused atomic.Bool
			if tc.redo {
				// A move OneDrive refuses once makes the run pass again.
				move := client.moveItemOptsFn
				client.moveItemOptsFn = func(
					ctx context.Context, d driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
				) (*graph.Item, error) {
					if refused.CompareAndSwap(false, true) {
						return nil, &graph.GraphError{
							StatusCode: http.StatusPreconditionFailed, Code: "preconditionFailed", Err: graph.ErrPreconditionFailed,
						}
					}
					return move(ctx, d, itemID, newParentID, newName, opts)
				}
			}
			eng, syncRoot := newTestEngine(t, client)
			clock := newManualClock(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
			installManualClockForTestEngine(eng, clock)
			runScenarioPass(t, eng)

			db := requireStoreTestRawDB(t, eng.baseline)
			_, err := db.ExecContext(t.Context(), refuseGoneCleanupSQL)
			require.NoError(t, err)
			if tc.redo {
				delta := client.deltaFn
				var dropped atomic.Bool
				client.deltaFn = func(ctx context.Context, d driveid.ID, token string) (*graph.DeltaPage, error) {
					if refused.Load() && dropped.CompareAndSwap(false, true) {
						if _, dropErr := db.ExecContext(ctx, `DROP TRIGGER refuse_gone_cleanup`); dropErr != nil {
							return nil, fmt.Errorf("dropping the cleanup trigger: %w", dropErr)
						}
						clock.Advance(time.Hour)
					}
					return delta(ctx, d, token)
				}
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "dir"), filepath.Join(syncRoot, "dir-moved")))
			}
			require.NoError(t, os.Remove(filepath.Join(syncRoot, "gone.txt")))
			remote.deleteAt(t, "gone.txt")

			report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, report.Failed, "the cleanup failed once: %+v", report)
			assert.Equal(t, tc.wantLeft, report.LeftWork(), "%+v", report)
		})
	}
}

// Validates: R-2.1.7, R-2.4.8, R-6.6.28
//
// A run the pass bound stops right after a pass that kept both versions of
// a file leaves the conflict copy, which only the next pass uploads, as work
// for the next sync, so a removed shared folder's final drain cannot call
// that copy synced.
func TestRunOnce_AConflictCopyAtThePassBoundIsLeftWork(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-doc", "root", "doc.txt", "first\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("edited here\n"), 0o600))
	remote.putFileAt(t, "doc.txt", "edited online\n")
	eng.oneShotPassLimit = 1

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.ConflictCopies, "%+v", report)
	assert.Positive(t, report.LeftWork().WillRetry, "the copy is the next sync's: %+v", report)
	assert.True(t, report.LocalTreeHolds().Any())
}

// Validates: R-2.4.8, R-6.4.9
//
// A file an interrupted removal set aside is the user's content: the report
// counts it, as status lists it, so a removed shared folder's local tree that
// holds one is not removed. It is not work left: the sync exits 0 for it.
func TestRunOnce_ReportsTheFilesAnInterruptedRemovalSetAside(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, ".onedrive-go.0123456789abcdef.detach"), []byte("half-removed\n"), 0o600))

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.InterruptedRemovals)
	assert.False(t, report.LeftWork().Any(), "%+v", report)
	assert.Equal(t, LocalTreeHolds{SetAsideFiles: 1}, report.LocalTreeHolds())
}

// Validates: R-6.6.18, R-6.6.28
//
// One predicate decides "kept for retry": for every failure class, the
// activity feed says "will retry" exactly when the pass counts the failure as
// kept for retry, so what a file line said and what the summary counts agree.
func TestFailureKeptForRetry_TheFeedAndThePassCountAgree(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	var activities []Activity
	eng.SetActivityHook(func(a Activity) { activities = append(activities, a) })

	for _, class := range []errclass.Class{
		errclass.ClassRetryableTransient,
		errclass.ClassScopeBlockingTransient,
		errclass.ClassActionable,
		errclass.ClassFatal,
		errclass.ClassInvalid,
	} {
		flow.resetResultStats()
		activities = nil
		completion := &actionCompletion{ActionType: ActionUpload, Path: "a.txt", Err: errors.New("refused")}

		eng.emitActivity(completion, class)
		flow.recordError(&resultDecision{Class: class}, completion)

		require.Len(t, activities, 1, "class %v", class)
		assert.Equal(t, activities[0].WillRetry, flow.results.keptForRetry == 1, "class %v", class)
		assert.Equal(t, 1, flow.results.failed, "class %v", class)
	}
}
