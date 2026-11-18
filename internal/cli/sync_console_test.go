package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func testSyncConsole() (*syncConsole, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return &syncConsole{
		out:     &out,
		errOut:  &errOut,
		now:     func() time.Time { return time.Date(2026, 10, 23, 22, 49, 18, 0, time.Local) },
		home:    "/home/user",
		tallies: make(map[string]*activityTally),
	}, &out, &errOut
}

func mountActivity(a *syncengine.Activity) multisync.MountActivity {
	return multisync.MountActivity{
		Identity:    multisync.MountIdentity{MountID: "m1"},
		DisplayName: "Personal",
		Activity:    *a,
	}
}

func singleMountResult(report *syncengine.Report) multisync.RunOnceResult {
	return multisync.RunOnceResult{
		Startup: multisync.StartupSelectionSummary{Results: []multisync.MountStartupResult{{
			SelectionIndex: 0,
			Identity:       multisync.MountIdentity{MountID: "m1"},
			DisplayName:    "Personal",
			Status:         multisync.MountStartupRunnable,
		}}},
		Reports: []*multisync.MountReport{{
			SelectionIndex: 0,
			Identity:       multisync.MountIdentity{MountID: "m1"},
			DisplayName:    "Personal",
			Report:         report,
		}},
	}
}

// Validates: R-6.6.18
func TestSyncConsole_NamesEveryCompletedFile(t *testing.T) {
	t.Parallel()

	c, out, errOut := testSyncConsole()
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "Docs/a.pdf", Size: 1536, SizeKnown: true, Success: true}))
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "b.txt", Success: true}))
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityRemoteDelete, Path: "old.txt", Success: true}))
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityLocalMove, OldPath: "x.txt", Path: "y.txt", Success: true}))
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityFolderCreate, Path: "Docs", Success: true}))

	assert.Equal(t, strings.Join([]string{
		"Downloaded Docs/a.pdf (1.5 KB)",
		"Uploaded b.txt",
		"Deleted on OneDrive old.txt",
		"Moved locally x.txt -> y.txt",
		"Created folder Docs",
	}, "\n")+"\n", out.String())
	assert.Empty(t, errOut.String())
}

// Validates: R-6.6.18
func TestSyncConsole_FailuresGoToStderrAndRetriesOnlyWhenVerbose(t *testing.T) {
	t.Parallel()

	c, out, errOut := testSyncConsole()
	denied := fmt.Errorf("write file: %w", os.ErrPermission)
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "locked.txt", Err: denied}))
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "busy.txt", WillRetry: true, Err: errors.New("throttled")}))

	assert.Empty(t, out.String())
	assert.Equal(t, "Failed to download locked.txt: permission denied\n", errOut.String(),
		"a retryable failure is not something the user must act on, so default output omits it")

	c.verbose = true
	errOut.Reset()
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "busy.txt", WillRetry: true, Err: errors.New("throttled")}))
	assert.Equal(t, "Could not upload busy.txt, will retry: throttled\n", errOut.String())
}

// A sync failure that a local file's error caused names the file once: the
// activity line names it, below the sync folder, and the reason is the
// error's innermost text, which holds no path.
//
// Validates: R-6.6.18, R-6.6.26
func TestSyncConsole_ALocalFileFailureNamesTheFileOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores file modes")
	}
	t.Parallel()

	locked := filepath.Join(t.TempDir(), "locked.txt")
	require.NoError(t, os.WriteFile(locked, nil, 0o000))
	_, openErr := localpath.OpenRegular(locked)
	require.Error(t, openErr)
	c, _, errOut := testSyncConsole()

	c.Activity(mountActivity(&syncengine.Activity{
		Kind: syncengine.ActivityUpload, Path: "Docs/locked.txt", Err: fmt.Errorf("hashing: %w", openErr),
	}))

	assert.Equal(t, "Failed to upload Docs/locked.txt: permission denied\n", errOut.String())
}

// Validates: R-6.6.18
func TestSyncConsole_QuietKeepsOnlyFailures(t *testing.T) {
	t.Parallel()

	c, out, errOut := testSyncConsole()
	c.quiet = true
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "a.txt", Success: true}))
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "b.txt", Err: os.ErrPermission}))
	c.Summary(singleMountResult(&syncengine.Report{Downloads: 2, Succeeded: 1, Failed: 1}))

	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "Failed to download b.txt")
}

// Validates: R-6.6.19
func TestSyncConsole_SummaryInHumanUnits(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	for i := range 3 {
		c.Activity(mountActivity(&syncengine.Activity{
			Kind: syncengine.ActivityDownload, Path: fmt.Sprintf("f%d", i), Size: sizeMB, SizeKnown: true, Success: true,
		}))
	}
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityFolderCreate, Path: "d", Success: true}))
	out.Reset()

	c.Summary(singleMountResult(&syncengine.Report{Duration: 5571 * time.Millisecond, Downloads: 3, FolderCreates: 1, Succeeded: 4}))

	assert.Equal(t, "Done in 5.6 s: downloaded 3 files (3.0 MB), created 1 folder.\n", out.String())
}

// Validates: R-6.6.19, R-6.6.28
func TestSyncConsole_SummaryUpToDateAndDryRun(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	left := c.Summary(singleMountResult(&syncengine.Report{Duration: 1577 * time.Millisecond}))
	assert.Equal(t, "Up to date (1.6 s).\n", out.String())
	assert.False(t, left["m1"].Any())

	// A drive the store still holds conditions for is not up to date, even
	// when this run had nothing to do (R-6.6.28).
	out.Reset()
	left = c.Summary(singleMountResult(&syncengine.Report{
		Duration:       1577 * time.Millisecond,
		UnsyncedIssues: 2,
	}))
	assert.Equal(t, "Done in 1.6 s; 2 still not synced (see 'onedrive-go status').\n", out.String())
	assert.Equal(t, syncengine.LeftWork{StillOpen: 2}, left["m1"])

	out.Reset()
	c.Summary(singleMountResult(&syncengine.Report{DryRun: true, Downloads: 19, FolderCreates: 2}))
	assert.Equal(t, "Dry run, nothing was changed. Would download 19 files, create 2 folders.\n", out.String())

	out.Reset()
	left = c.Summary(singleMountResult(&syncengine.Report{DryRun: true, UnsyncedIssues: 1}))
	assert.Equal(t, "Dry run: nothing to do.\n", out.String())
	assert.False(t, left["m1"].Any(), "a dry run attempted nothing, so it left nothing")
}

// Validates: R-6.6.19, R-6.6.28
//
// The issues that keep a file unsynced add to the last pass's own failures
// and left work: no plan action covers a path that holds one, so nothing is
// counted twice. Three names OneDrive refuses beside two retryable failures
// are five files left.
func TestSyncConsole_StillOpenAddsTheIssuesToTheRunsOwnCounts(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.ActivityHook()
	for _, path := range []string{"a.txt", "b.txt"} {
		c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityUpload, Path: path, WillRetry: true}))
	}
	result := singleMountResult(&syncengine.Report{
		Duration:             2 * time.Second,
		Uploads:              2,
		Failed:               2,
		LastPassFailed:       2,
		LastPassKeptForRetry: 2,
		UnsyncedIssues:       3,
	})
	left := c.Summary(result)

	assert.Equal(t, syncengine.LeftWork{WillRetry: 2, StillOpen: 3}, left["m1"])
	assert.Equal(t, "Done in 2.0 s; 2 will be retried on the next sync; "+
		"3 still not synced (see 'onedrive-go status').\n", out.String())
	err := runOnceResultError(result, left)
	require.ErrorIs(t, err, errSyncLeftWork)
	assert.Equal(t, "sync left 5 files unsynced; run 'onedrive-go status' for details", err.Error())

	// One upload a throttle took back and no issue: one file, no still-open part.
	c, out, _ = testSyncConsole()
	c.ActivityHook()
	result = singleMountResult(&syncengine.Report{Duration: time.Second, Uploads: 1, LeftForNextSync: 1})
	left = c.Summary(result)
	assert.Equal(t, syncengine.LeftWork{WillRetry: 1}, left["m1"])
	assert.NotContains(t, out.String(), "still not synced")
	assert.Equal(t, "sync left 1 file unsynced; run 'onedrive-go status' for details", runOnceResultError(result, left).Error())
}

// Validates: R-6.6.18, R-6.6.19, R-6.6.28, R-6.6.33
//
// A file action a condition only the user can clear refused (OneDrive
// refused the path as too long) is said once per path, on stderr, in the
// words status uses for that condition, and counted with what is still not
// synced, not as failed; its retries print nothing. Under --json the activity
// names the condition as status --json does, and its status agrees with the
// summary's still_open.
func TestSyncConsole_AConditionTheUserMustClearIsSaidOnceInStatusWords(t *testing.T) {
	t.Parallel()

	refused := syncengine.Activity{
		Kind:          syncengine.ActivityFolderCreate,
		Path:          "deep",
		ConditionKey:  syncengine.ConditionPathTooLong,
		ConditionType: syncengine.IssueType("path_too_long"),
		Err:           errors.New("graph: bad request"),
	}
	report := &syncengine.Report{
		Duration: 2 * time.Second, FolderCreates: 1, Failed: 1, LastPassFailed: 1, LastPassAwaitingUser: 1,
	}
	words := describeStatusCondition(syncengine.ConditionPathTooLong)

	c, out, errOut := testSyncConsole()
	c.ActivityHook()
	c.Activity(mountActivity(&refused))
	c.Activity(mountActivity(&refused))
	left := c.Summary(singleMountResult(report))

	assert.Equal(t, "Can't create folder deep: "+words.Title+". "+words.Reason+" "+words.Action+"\n", errOut.String(),
		"said once per path, in status's words")
	assert.Equal(t, "Done in 2.0 s; 1 still not synced (see 'onedrive-go status').\n", out.String())
	assert.Equal(t, syncengine.LeftWork{StillOpen: 1}, left["m1"])
	require.ErrorIs(t, runOnceResultError(singleMountResult(report), left), errSyncLeftWork, "the run still left the file")

	c, out, errOut = testSyncConsole()
	c.json = true
	c.ActivityHook()
	c.Activity(mountActivity(&refused))
	c.Activity(mountActivity(&refused))
	c.Summary(singleMountResult(report))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2, "one activity line for the path, then the summary")
	var activity, summary syncJSONEvent
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &activity))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &summary))
	assert.Equal(t, "still_open", activity.Status)
	assert.Equal(t, "path_too_long", activity.Condition)
	assert.Equal(t, "graph: bad request", activity.Error)
	require.NotNil(t, summary.Failed)
	assert.Zero(t, *summary.Failed)
	require.NotNil(t, summary.StillOpen)
	assert.Equal(t, 1, *summary.StillOpen)
	assert.Empty(t, errOut.String())
}

// Validates: R-2.10.71, R-6.6.18, R-6.6.30
//
// A condition is said when it starts. A path that synced after a condition
// refused it starts over: when the condition refuses it again, the console
// says so again, in text and under --json alike, as one console lives for a
// whole `sync --watch`.
func TestSyncConsole_AConditionIsSaidAgainOnceItsPathSyncedInBetween(t *testing.T) {
	t.Parallel()

	refused := syncengine.Activity{
		Kind:          syncengine.ActivityFolderCreate,
		Path:          "deep",
		ConditionKey:  syncengine.ConditionPathTooLong,
		ConditionType: syncengine.IssueType("path_too_long"),
		Err:           errors.New("graph: bad request"),
	}
	synced := syncengine.Activity{Kind: syncengine.ActivityFolderCreate, Path: "deep", Success: true}
	words := describeStatusCondition(syncengine.ConditionPathTooLong)
	conditionLine := "Can't create folder deep: " + words.Title + ". " + words.Reason + " " + words.Action + "\n"

	c, out, errOut := testSyncConsole()
	for _, a := range []*syncengine.Activity{&refused, &refused, &synced, &refused} {
		c.Activity(mountActivity(a))
	}
	assert.Equal(t, conditionLine+conditionLine, errOut.String())
	assert.Equal(t, "Created folder deep\n", out.String())

	c, out, _ = testSyncConsole()
	c.json = true
	for _, a := range []*syncengine.Activity{&refused, &refused, &synced, &refused} {
		c.Activity(mountActivity(a))
	}
	var statuses []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var event syncJSONEvent
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		statuses = append(statuses, event.Status)
	}
	assert.Equal(t, []string{"still_open", "done", "still_open"}, statuses)
}

// Validates: R-2.10.71, R-6.6.18, R-6.6.30
//
// A condition can start again with no activity of its path in between: the
// plan dropped the path's retry work (the refused folder was deleted, then
// made again), and the next refusal starts the condition anew. The engine
// marks such a refusal as the condition starting, and the console says it
// again although the last thing it said of the path was the same condition.
// A refusal the engine does not mark is a retry: the console says it only the
// first time this run sees the path refused.
func TestSyncConsole_AConditionThatStartsAgainWithNoActivityBetweenIsSaidAgain(t *testing.T) {
	t.Parallel()

	starts := syncengine.Activity{
		Kind:            syncengine.ActivityFolderCreate,
		Path:            "deep",
		ConditionStarts: true,
		ConditionKey:    syncengine.ConditionPathTooLong,
		ConditionType:   syncengine.IssueType("path_too_long"),
		Err:             errors.New("graph: bad request"),
	}
	retry := starts
	retry.ConditionStarts = false
	words := describeStatusCondition(syncengine.ConditionPathTooLong)
	conditionLine := "Can't create folder deep: " + words.Title + ". " + words.Reason + " " + words.Action + "\n"

	c, _, errOut := testSyncConsole()
	for _, a := range []*syncengine.Activity{&starts, &retry, &starts, &retry} {
		c.Activity(mountActivity(a))
	}
	assert.Equal(t, conditionLine+conditionLine, errOut.String(), "said when it starts, each time")

	c, out, _ := testSyncConsole()
	c.json = true
	for _, a := range []*syncengine.Activity{&starts, &retry, &starts, &retry} {
		c.Activity(mountActivity(a))
	}
	assert.Equal(t, 2, strings.Count(out.String(), `"status":"still_open"`), out.String())

	c, _, errOut = testSyncConsole()
	c.Activity(mountActivity(&retry))
	c.Activity(mountActivity(&retry))
	assert.Equal(t, conditionLine, errOut.String(), "a run that first sees the path on a retry says it once")
}

// Validates: R-6.6.19
func TestSyncConsole_SummaryExplainsHeldBackWork(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.Summary(singleMountResult(&syncengine.Report{
		Mode:           syncengine.SyncDownloadOnly,
		DeferredByMode: syncengine.DeferredCounts{Uploads: 3},
	}))

	assert.Contains(t, out.String(), "Held back by --download-only: 3 uploads.")
}

// Validates: R-6.6.19
func TestHeldBackSummary_CountsOneOfEachInTheSingular(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Held back by --download-only: 1 upload, 1 download, 1 delete on OneDrive, "+
		"1 local delete, 1 move, 1 folder create.", heldBackSummary(&syncengine.Report{
		Mode: syncengine.SyncDownloadOnly,
		DeferredByMode: syncengine.DeferredCounts{
			Uploads: 1, Downloads: 1, RemoteDeletes: 1, LocalDeletes: 1, Moves: 1, FolderCreates: 1,
		},
	}))
}

// Validates: R-6.6.19, R-6.6.33
//
// Live, 2026-11-02: a one-shot confirm of 6,000 deletes met a 429 after
// 5,557; the scope took the remaining 442 back and the pass ended with
// "1 will be retried", counting only the failed one. The work a throttle
// leaves for the next run is counted too.
func TestSyncConsole_SummaryCountsWorkAThrottleLeftForTheNextSync(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.Summary(singleMountResult(&syncengine.Report{
		Mode:            syncengine.SyncBidirectional,
		Duration:        16 * time.Minute,
		RemoteDeletes:   5557,
		Succeeded:       5557,
		Failed:          1,
		LastPassFailed:  1,
		LeftForNextSync: 442,
	}))

	// The one failure the engine does not retry by itself is reported as
	// failed; the 442 the scope took back are counted as retried.
	assert.Contains(t, out.String(), "deleted 5557 files on OneDrive; 1 failed; 442 will be retried on the next sync.")

	// The JSON summary counts the same work: will_retry includes what the
	// throttle took back, and the drive's status is "incomplete" (R-6.6.33).
	c, out, _ = testSyncConsole()
	c.json = true
	c.Summary(singleMountResult(&syncengine.Report{
		Mode:            syncengine.SyncBidirectional,
		RemoteDeletes:   5557,
		Succeeded:       5557,
		Failed:          1,
		LastPassFailed:  1,
		LeftForNextSync: 442,
	}))
	var summary syncJSONEvent
	require.NoError(t, json.Unmarshal(out.Bytes(), &summary))
	assert.Equal(t, "incomplete", summary.Status)
	require.NotNil(t, summary.Failed)
	assert.Equal(t, 1, *summary.Failed)
	require.NotNil(t, summary.WillRetry)
	assert.Equal(t, 442, *summary.WillRetry)
}

// Validates: R-6.6.22
func TestSyncConsole_SingleDriveErrorIsLeftToTheReturnedError(t *testing.T) {
	t.Parallel()

	c, out, errOut := testSyncConsole()
	loginErr := fmt.Errorf("session error for mount personal:user@example.com: %w", graph.ErrNotLoggedIn)
	result := multisync.RunOnceResult{
		Startup: multisync.StartupSelectionSummary{Results: []multisync.MountStartupResult{{
			DisplayName: "Personal",
			Status:      multisync.MountStartupFatal,
			Err:         loginErr,
		}}},
	}

	left := c.Summary(result)
	assert.Empty(t, out.String())
	assert.Empty(t, errOut.String(), "the command error prints it; printing here too doubled it")
	assert.Empty(t, left, "a drive that could not start left no counted work; its error is the result")

	err := runOnceResultError(result, left)
	require.Error(t, err)
	require.ErrorIs(t, err, graph.ErrNotLoggedIn, "the returned error must keep its cause so it is presented as sign-in required")
	assert.Contains(t, commandErrorText(err, false), "Authentication required")
}

// A pass whose every drive synced still fails when a drive's sync state did
// not close afterwards: the engine may still hold its store, and only the
// process exit releases it. A user's stop does not turn that into 130.
//
// Validates: R-6.3.9, R-6.6.28
func TestRunOnceResultError_AnEngineThatDidNotCloseFailsAPassThatSynced(t *testing.T) {
	t.Parallel()

	result := multisync.RunOnceResult{
		Startup: multisync.StartupSelectionSummary{Results: []multisync.MountStartupResult{{
			DisplayName: "Personal",
			Status:      multisync.MountStartupRunnable,
		}}},
		Reports: []*multisync.MountReport{{DisplayName: "Personal", Report: &syncengine.Report{}}},
		Stuck:   fmt.Errorf("Personal: %w (its sync state did not close within 15 s)", multisync.ErrSyncStuck),
	}

	err := runOnceResultError(result, map[string]syncengine.LeftWork{"": {}})

	require.ErrorIs(t, err, multisync.ErrSyncStuck)
	assert.Equal(t, exitFailure, commandExitCode(err, false))
	assert.Equal(t, exitFailure, commandExitCode(err, true))

	// Work left and an engine that did not close are both reported.
	err = runOnceResultError(result, map[string]syncengine.LeftWork{"": {Failed: 1}})
	require.ErrorIs(t, err, multisync.ErrSyncStuck)
	require.ErrorIs(t, err, errSyncLeftWork)
	assert.Equal(t, exitFailure, commandExitCode(err, true))
}

// Validates: R-6.6.22, R-6.6.28
//
// A drive whose run returned a report and an error (a store it could not
// read after the last pass) failed: its error is the command's, and its
// report's counts are not printed or counted as work left.
func TestRunOnceResultError_ADriveWithAnErrorAndAReportIsADriveError(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	readErr := errors.New("sync: reading the issues the run left: disk I/O error")
	result := singleMountResult(&syncengine.Report{Uploads: 1, Failed: 1})
	result.Reports[0].Err = readErr

	left := c.Summary(result)
	assert.Empty(t, left)
	assert.NotContains(t, out.String(), "Done in")

	err := runOnceResultError(result, left)
	require.ErrorIs(t, err, readErr)
	require.NotErrorIs(t, err, errSyncLeftWork)
}

// Validates: R-6.6.22
func TestSyncConsole_MultiDriveNamesEachDrive(t *testing.T) {
	t.Parallel()

	c, out, errOut := testSyncConsole()
	c.Summary(multisync.RunOnceResult{
		Startup: multisync.StartupSelectionSummary{Results: []multisync.MountStartupResult{
			{SelectionIndex: 0, DisplayName: "Work", Status: multisync.MountStartupFatal, Err: errors.New("boom")},
			{SelectionIndex: 1, DisplayName: "Home", Status: multisync.MountStartupPaused},
			{SelectionIndex: 2, DisplayName: "Family", Status: multisync.MountStartupRunnable},
		}},
		Reports: []*multisync.MountReport{{SelectionIndex: 2, Report: &syncengine.Report{}}},
	})

	assert.Equal(t, "Work: Error: boom\n", errOut.String())
	assert.Contains(t, out.String(), "Home: Skipped (paused).")
	assert.Contains(t, out.String(), "Family: Up to date")
}

// Validates: R-6.6.20
func TestSyncConsole_WatchLinesAreTimestampedAndAnnounceStartAndStop(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.stamp = true
	c.Watching([]multisync.StandaloneMountConfig{{SyncRoot: "/home/user/OneDrive", AccountEmail: "user@example.com"}}, syncengine.SyncBidirectional)
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "a.txt", Success: true}))
	c.Stopped()

	assert.Equal(t, strings.Join([]string{
		"[22:49:18] Watching ~/OneDrive for user@example.com.",
		"[22:49:18] Press Ctrl-C to stop.",
		"[22:49:18] Uploaded a.txt",
		"[22:49:18] Stopped.",
	}, "\n")+"\n", out.String())
}

// Validates: R-6.6.21
func TestSyncConsole_JournalMarksFailuresWithErrorPriority(t *testing.T) {
	t.Parallel()

	c, out, errOut := testSyncConsole()
	c.journal = true
	c.Watching([]multisync.StandaloneMountConfig{{SyncRoot: "/srv/data", AccountEmail: "user@example.com"}}, syncengine.SyncDownloadOnly)
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "a.txt", Err: os.ErrPermission}))

	assert.Equal(t, "Watching /srv/data for user@example.com (download only).\n", out.String(),
		"the journal has no keyboard, so no Ctrl-C hint")
	assert.Equal(t, "<3>Failed to download a.txt: permission denied\n", errOut.String())
}

// Validates: R-3.4.2, R-6.6.19
func TestSyncConsole_WatchingEveryDrivePausedSaysWhatItWaitsFor(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.Watching([]multisync.StandaloneMountConfig{
		{SyncRoot: "/home/user/OneDrive", AccountEmail: "user@example.com", Paused: true},
	}, syncengine.SyncBidirectional)

	assert.Equal(t, "Watching ~/OneDrive for user@example.com (paused).\n"+
		"All drives are paused; waiting until one is resumed ('onedrive-go resume').\n"+
		"Press Ctrl-C to stop.\n", out.String())
}

// Validates: R-6.6.19
func TestSyncConsole_StartingNamesFolderAndAccount(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.Starting([]multisync.StandaloneMountConfig{
		{SyncRoot: "/home/user/OneDrive", AccountEmail: "user@example.com"},
		{SyncRoot: "/home/user/Paused", AccountEmail: "user@example.com", Paused: true},
	}, syncengine.SyncBidirectional, false)

	assert.Equal(t, "Syncing ~/OneDrive with user@example.com...\n", out.String())
}

// Validates: R-6.6.22
func TestCommandErrorText_NetworkFailureIsOnePlainLine(t *testing.T) {
	t.Parallel()

	refused := &net.OpError{Op: "proxyconnect", Net: "tcp", Err: os.NewSyscallError("connect", errors.New("connection refused"))}
	err := fmt.Errorf("run sync engine once: sync: verifying drive identity: graph: GET /drives/x: %w", refused)

	text := commandErrorText(err, false)
	assert.Equal(t, "Error: can't reach OneDrive through the configured proxy (connection refused). Check your network connection, then run the command again.", text)

	assert.Contains(t, commandErrorText(err, true), "\nDetails: run sync engine once:", "--verbose keeps the full chain")
}

func TestHumanDuration(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0.4 s", humanDuration(400*time.Millisecond))
	assert.Equal(t, "2 min 10 s", humanDuration(130*time.Second))
	assert.Equal(t, "1 h 5 min", humanDuration(65*time.Minute))
}

// Validates: R-6.6.28
func TestSyncConsole_JSONWritesOnlyJSONLines(t *testing.T) {
	t.Parallel()

	c, out, errOut := testSyncConsole()
	c.json = true
	c.Starting([]multisync.StandaloneMountConfig{{DisplayName: "Personal", SyncRoot: "/home/user/OneDrive", AccountEmail: "user@example.com"}},
		syncengine.SyncBidirectional, false)
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "a.pdf", Size: 10, SizeKnown: true, Success: true}))
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "b.txt", Err: os.ErrPermission}))
	c.Summary(singleMountResult(&syncengine.Report{
		Duration: 2 * time.Second, Downloads: 1, Uploads: 1, Succeeded: 1, Failed: 1, LastPassFailed: 1,
		UnsyncedIssues: 1,
	}))

	var events []syncJSONEvent
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var event syncJSONEvent
		require.NoError(t, json.Unmarshal([]byte(line), &event), "every stdout line must be JSON: %q", line)
		events = append(events, event)
	}

	require.Len(t, events, 4)
	assert.Equal(t, "start", events[0].Event)
	assert.Equal(t, "user@example.com", events[0].Account)
	assert.Equal(t, "activity", events[1].Event)
	assert.Equal(t, "download", events[1].Action)
	assert.Equal(t, "done", events[1].Status)
	assert.Equal(t, "failed", events[2].Status)
	assert.Equal(t, "permission denied", events[2].Error)
	assert.Equal(t, "summary", events[3].Event)
	assert.Equal(t, "incomplete", events[3].Status, "a drive that left work is not done")
	require.NotNil(t, events[3].Failed)
	assert.Equal(t, 1, *events[3].Failed)
	require.NotNil(t, events[3].WillRetry)
	assert.Zero(t, *events[3].WillRetry)
	require.NotNil(t, events[3].StillOpen)
	assert.Equal(t, 1, *events[3].StillOpen, "the issue that keeps a file unsynced")
	assert.Equal(t, 1, events[3].Counts["download"])
	assert.Empty(t, errOut.String(), "under --json, file failures are data on stdout")
}

// Validates: R-6.6.28, R-6.6.33
func TestSyncConsole_JSONSummaryOfADriveThatLeftNothingIsDone(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.json = true
	left := c.Summary(singleMountResult(&syncengine.Report{Duration: time.Second, Downloads: 1, Succeeded: 1}))

	var summary syncJSONEvent
	require.NoError(t, json.Unmarshal(out.Bytes(), &summary))
	assert.Equal(t, "done", summary.Status)
	require.NotNil(t, summary.StillOpen)
	assert.Zero(t, *summary.StillOpen)
	assert.False(t, left["m1"].Any())
}

// flakyWriter refuses the first writes, as many as failures says, with err,
// and keeps everything written after them.
type flakyWriter struct {
	failures int
	err      error
	written  bytes.Buffer
}

func (w *flakyWriter) Write(p []byte) (int, error) {
	if w.failures > 0 {
		w.failures--
		return 0, w.err
	}

	return w.written.Write(p) //nolint:wrapcheck // a writer hands back its buffer's own result, as io.Writer expects
}

// A sync --json line that cannot be written is reported on stderr, once
// however many lines fail, and the sync goes on writing the lines after it.
//
// Validates: R-6.6.28, R-6.6.33
func TestSyncConsole_EmitJSONFailureIsReported(t *testing.T) {
	t.Parallel()

	c, _, errOut := testSyncConsole()
	out := &flakyWriter{failures: 2, err: errors.New("broken pipe")}
	c.out = out
	c.json = true

	c.emitJSON(&syncJSONEvent{Event: "start", Drive: "Personal"})
	c.emitJSON(&syncJSONEvent{Event: "activity", Drive: "Personal"})
	c.emitJSON(&syncJSONEvent{Event: "stopped"})

	assert.Equal(t, "Warning: sync --json output is incomplete: write JSON output: broken pipe\n", errOut.String(),
		"two failed lines are reported once")
	//nolint:testifylint // byte-exact on purpose: the one line written after the failures, newline included
	assert.Equal(t, "{\"event\":\"stopped\"}\n", out.written.String(), "the lines after a failure are still written")
}

// A pass that waits out a throttle and then uploads the file (R-2.1.7) ends
// "uploaded 1 file (22 B).", with no retry promised and nothing failed: the
// retry its first attempt recorded no longer counts once the same file
// uploads.
//
// Validates: R-6.6.19, R-6.6.28
func TestSyncConsole_SummaryDoesNotPromiseARetryOfWorkTheRunFinished(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.ActivityHook()
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "beta/rl-1.txt", WillRetry: true}))
	c.Activity(mountActivity(&syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "beta/rl-1.txt", Size: 22, SizeKnown: true, Success: true}))
	left := c.Summary(singleMountResult(&syncengine.Report{
		Mode:      syncengine.SyncBidirectional,
		Duration:  136 * time.Second,
		Uploads:   1,
		Succeeded: 1,
		Failed:    1,
	}))

	assert.Contains(t, out.String(), "Done in 2 min 16 s: uploaded 1 file (22 B).")
	assert.NotContains(t, out.String(), "will be retried")
	assert.NotContains(t, out.String(), "failed")
	assert.False(t, left["m1"].Any(), "a retry the run finished does not fail the run (R-6.6.28)")
}

// With the activity feed wired, a pass whose only planned action was held as
// retry work did nothing, and its summary says so instead of falling back to
// the planner's counts ("uploaded 1 file").
//
// Validates: R-6.6.19
func TestSyncConsole_SummaryWithAWiredFeedAndNoActivityDoesNotUsePlannerCounts(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.ActivityHook()
	c.Summary(singleMountResult(&syncengine.Report{
		Mode:            syncengine.SyncBidirectional,
		Duration:        1900 * time.Millisecond,
		Uploads:         1,
		LeftForNextSync: 1,
	}))

	assert.Contains(t, out.String(), "Done in 1.9 s; 1 will be retried on the next sync.")
	assert.NotContains(t, out.String(), "uploaded")
}
