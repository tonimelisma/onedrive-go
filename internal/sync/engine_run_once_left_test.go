package sync

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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

// Validates: R-6.6.19, R-6.8.24
//
// Live, 2026-11-02: a one-shot confirm of 6,000 deletes met a 429 after
// 5,557; the scope took the remaining 442 back for a trial after the pass
// had ended, and the summary said "1 will be retried", counting only the
// failed one. The report now counts the planned work the pass neither
// completed nor failed, and a converged pass counts none.
func TestRunOnce_ReportCountsTheWorkAThrottleLeftForTheNextSync(t *testing.T) {
	t.Parallel()

	const files = 40
	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	clock := newManualClock(time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC))
	const window = 2 * time.Second
	limiter := newThrottleWindowClient(clock.Now, 10, window)
	eng, syncRoot := newTestEngine(t, limiter.wrap(remote.client(t)))
	eng.transferWorkers = 4
	installManualClockForTestEngine(eng, clock)
	for i := range files {
		writeLocalFile(t, syncRoot, fmt.Sprintf("bulk/file-%02d.txt", i), fmt.Sprintf("content %d\n", i))
	}

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	planned := report.FolderCreates + report.Uploads
	require.Less(t, report.Succeeded, planned, "the window of 10 requests cannot carry the whole plan")
	assert.Positive(t, report.LeftForNextSync, "the 429 took work back for a trial after the pass")
	assert.Equal(t, planned-report.Succeeded-report.Failed, report.LeftForNextSync,
		"everything planned is completed, failed or left for the next sync")

	for i := 0; i < 40 && report.LeftForNextSync+report.Failed > 0; i++ {
		clock.Advance(window)
		report, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}
	assert.Zero(t, report.LeftForNextSync, "a converged pass leaves nothing")
	assert.Equal(t, files, remote.uploads, "every upload completes exactly once")
	quiet, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, quiet.LeftForNextSync)
	assert.Zero(t, quiet.Succeeded)
	assert.Zero(t, quiet.UnsyncedIssues, "a converged drive holds no issue")
}

// Validates: R-6.6.28
//
// A live one-shot run counts every file it leaves unsynced once. A refusal
// only the user can clear is a failed action, and work waiting on the retry
// curve is a planned action the run holds and leaves for the next sync;
// their retry rows are the store's only retry work, because a live pass
// prunes the rows to its plan's actions. A name OneDrive refuses has no plan
// action at all: the run reads it from the snapshot `status` reads.
func TestRunOnce_ReportCountsUnresolvedWork(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	client.createFolderFn = func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
		return nil, &graph.GraphError{
			StatusCode: http.StatusBadRequest,
			Message:    "The specified file or folder name is too long.",
			Err:        graph.ErrBadRequest,
		}
	}
	eng, syncRoot := newTestEngine(t, client)
	clock := newManualClock(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)

	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "deep"), 0o750))
	writeLocalFile(t, syncRoot, "CON", "a name OneDrive reserves")
	writeLocalFile(t, syncRoot, "waiting.txt", "failed three times before")
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "waiting.txt",
		ActionType:   ActionUpload,
		AttemptCount: unresolvedTestRetryingAttempts,
		NextRetryAt:  clock.Now().Add(time.Hour).UnixNano(),
	}))

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Failed, "the folder OneDrive refused: %v", report.Errors)
	assert.Equal(t, 1, report.LeftForNextSync, "waiting.txt is held until its retry is due")
	assert.Equal(t, 1, report.UnsyncedIssues, "the reserved name")
	assert.Equal(t, LeftWork{Failed: 1, WillRetry: 1, StillOpen: 1}, report.LeftWork())

	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Len(t, rows, 2, "the refusal and the held upload: %+v", rows)
	assert.LessOrEqual(t, len(rows), report.LastPassFailed+report.LeftForNextSync,
		"every retry row the store keeps is an action the report already counts")

	snapshot, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, snapshot.unsyncedIssueCount(), report.UnsyncedIssues, "the report counts what status reads")
}

// Validates: R-6.6.28, R-2.10.33
//
// A live pass prunes the store's retry work to its plan's actions: a
// retrying, a blocked and an actionable row for paths the plan does not act
// on are gone after the pass. So no retry row outlives a run without its
// action counted as failed or left for the next sync.
func TestRunOnce_RetryRowsThePlanDoesNotActOnAreGone(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, _ := newTestEngine(t, remote.client(t))
	ctx := t.Context()
	future := time.Now().Add(time.Hour).UnixNano()
	scope := SKQuotaOwn()
	require.NoError(t, eng.baseline.UpsertBlockScope(ctx, &BlockScope{
		Key:           scope,
		TrialInterval: time.Hour,
		NextTrialAt:   time.Now().Add(time.Hour),
	}))
	for _, row := range []*RetryWorkRow{
		{Path: "gone/retrying.txt", ActionType: ActionUpload, AttemptCount: unresolvedTestRetryingAttempts, NextRetryAt: future},
		{Path: "gone/blocked.txt", ActionType: ActionUpload, ScopeKey: scope, Blocked: true, AttemptCount: 1},
		{Path: "gone/deep", ActionType: ActionFolderCreate, ConditionType: issuePathTooLong, AttemptCount: 1, NextRetryAt: future},
	} {
		require.NoError(t, eng.baseline.UpsertRetryWork(ctx, row))
	}

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.Failed+report.LeftForNextSync, "%+v", report)

	rows, err := eng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "no plan action, no retry row")
	scopes, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, scopes, "a scope without blocked work goes with it")
}

// Validates: R-6.6.28
//
// When the store cannot be read after the last pass, RunOnce returns the
// report of the work it did together with the error: nothing proves the run
// left nothing, so the drive fails rather than reporting success.
func TestRunOnce_ALastStoreReadThatFailsReturnsTheReportAndTheError(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	eng.oneShotPassLimit = 1
	writeLocalFile(t, syncRoot, "a.txt", "content")
	db := requireStoreTestRawDB(t, eng.baseline)
	var damageErr error
	eng.SetActivityHook(func(Activity) {
		// An issue type no member has: the snapshot read refuses the row.
		_, damageErr = db.ExecContext(context.Background(),
			`INSERT INTO observation_issues (path, issue_type, scope_key) VALUES ('x.txt', 'not_an_issue_type', '')`)
	})

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, damageErr)
	require.ErrorContains(t, err, "reading the issues the run left")
	require.NotNil(t, report, "the work the run did is still reported")
	assert.Equal(t, 1, report.Succeeded)
}

// Validates: R-6.6.28
//
// A plan whose actions and dependencies disagree is a planner bug: none of
// it runs, and every action counts as failed, which makes the one-shot sync
// exit 1. The violation is logged at Error, so the user can report it.
func TestExecutePreparedPlan_AMismatchedPlanFailsEveryActionLoudly(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	report := &Report{}
	runtime := &runtimePlan{
		Plan: &actionPlan{
			Actions: []Action{{Type: ActionUpload, Path: "a.txt"}, {Type: ActionUpload, Path: "b.txt"}},
			Deps:    [][]int{nil},
		},
		Report: report,
	}

	require.NoError(t, newOneShotRunner(eng.Engine).executePreparedPlan(t.Context(), runtime, &Baseline{}))
	assert.Equal(t, len(runtime.Plan.Actions), report.Failed)
	assert.Equal(t, LeftWork{Failed: len(runtime.Plan.Actions)}, report.LeftWork(),
		"every action of the plan is left as failed, none kept for retry")
	assert.Zero(t, report.LeftForNextSync)
	require.Len(t, report.Errors, 1)
	require.ErrorContains(t, report.Errors[0], "plan invariant violation: 2 actions but 1 deps")
	assert.Contains(t, logBuf.String(), "level=ERROR")
}

// Validates: R-2.1.7, R-6.6.28
//
// The follow-up work of the plan a run ends on is the next sync's: a swap
// the pass bound stops after its first pass leaves the parked item and the
// work after its renames, and exits 1; the same swap with the passes a run
// makes finishes and leaves nothing.
func TestRunOnce_FollowUpWorkTheRunEndsOnIsLeftForTheNextSync(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		limit    int
		wantLeft bool
	}{
		{name: "stopped at the pass bound", limit: 1, wantLeft: true},
		{name: "finished in its own passes", wantLeft: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, syncRoot := newRemoteSwapTestEngine(t)
			eng.oneShotPassLimit = tc.limit
			report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			require.Zero(t, report.Failed, "errors: %v", report.Errors)
			if tc.wantLeft {
				assert.Positive(t, report.LeftForNextSync, "the parked item is the next sync's: %+v", report)
				return
			}
			assert.Zero(t, report.LeftForNextSync, "%+v", report)
			assert.Equal(t, map[string]string{"a.txt": "B\n", "b.txt": "A\n", "keep.txt": "keep\n"},
				harnessLocalTree(t, syncRoot))
		})
	}
}

// Validates: R-2.1.4, R-6.6.28
//
// Follow-up work a one-way mode holds back is not left for the next sync: a
// swap OneDrive made needs moves on this computer, which an upload-only run
// does not make in this pass or the next, so the run leaves nothing.
func TestRunOnce_UploadOnlyLeavesNoFollowUpForMovesTheModeHoldsBack(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newRemoteSwapTestEngine(t)
	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Positive(t, report.DeferredByMode.Moves, "the local moves are held back by the mode: %+v", report)
	assert.Zero(t, report.LeftForNextSync, "%+v", report)
	assert.Equal(t, map[string]string{"a.txt": "A\n", "b.txt": "B\n", "keep.txt": "keep\n"},
		harnessLocalTree(t, syncRoot), "this computer keeps its names")
}

// newRemoteSwapTestEngine is an engine whose drive synced a.txt and b.txt
// before OneDrive swapped their names, with its sync root.
func newRemoteSwapTestEngine(t *testing.T) (*testEngine, string) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-a", "root", "a.txt", "A\n")
	remote.addFile(t, "item-b", "root", "b.txt", "B\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	remote.moveAt(t, "a.txt", "swap.txt")
	remote.moveAt(t, "b.txt", "a.txt")
	remote.moveAt(t, "swap.txt", "b.txt")

	return eng, syncRoot
}

// Validates: R-2.1.7, R-6.6.28
//
// A plan with nothing to execute keeps its follow-up: a plan whose every
// action was dropped until the next pass still leaves that work, and the run
// that ends on it counts it.
func TestPassWithoutActions_KeepsThePlansFollowUp(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	followUp := planFollowUp{Dropped: 2, Unfinished: 2}
	pass := eng.passWithoutActions(eng.nowFunc(), SyncBidirectional, RunOptions{}, &runtimePlan{
		Plan:   &actionPlan{FollowUp: followUp},
		Report: &Report{},
	})

	assert.Equal(t, followUp, pass.followUp)
	assert.False(t, oneShotFollowUpWanted(t.Context(), pass), "a pass without progress runs no follow-up")
	assert.Equal(t, 2, pass.followUp.leftWhenTheRunEnds(false))
}

// Validates: R-6.6.28
//
// The follow-up a run ends on counts its unfinished deferred and dropped
// work always, and its renames only when the pass bound stopped the run.
// Work a one-way mode holds back is not unfinished.
func TestPlanFollowUp_LeftWhenTheRunEnds(t *testing.T) {
	t.Parallel()

	followUp := planFollowUp{Renames: 3, Deferred: 2, Dropped: 2, Unfinished: 1}
	assert.Equal(t, 1, followUp.leftWhenTheRunEnds(false))
	assert.Equal(t, 4, followUp.leftWhenTheRunEnds(true))
	assert.Zero(t, planFollowUp{}.leftWhenTheRunEnds(true))
}

// unresolvedTestRetryingAttempts is the attempt count from which `status`
// lists retry work as "Retrying".
const unresolvedTestRetryingAttempts = 3

// Validates: R-6.6.28
//
// A dry run attempts nothing, so it leaves nothing, even beside an issue the
// store already holds: its exit status does not read it.
func TestRunOnce_DryRunLeavesUnresolvedZero(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	writeLocalFile(t, syncRoot, "waiting.txt", "failed three times before")
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "waiting.txt",
		ActionType:   ActionUpload,
		AttemptCount: unresolvedTestRetryingAttempts,
		NextRetryAt:  time.Now().Add(time.Hour).UnixNano(),
	}))
	seedObservationIssueForTest(t, eng.baseline, "CON", IssueInvalidFilename, ScopeKey{})

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)
	assert.True(t, report.DryRun)
	assert.Zero(t, report.UnsyncedIssues)
	assert.Zero(t, report.LeftForNextSync)

	snapshot, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	assert.Positive(t, snapshot.unsyncedIssueCount(), "the store does hold an issue")
}
