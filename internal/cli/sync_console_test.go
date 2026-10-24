package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
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

func mountActivity(a syncengine.Activity) multisync.MountActivity {
	return multisync.MountActivity{
		Identity:    multisync.MountIdentity{MountID: "m1"},
		DisplayName: "Personal",
		Activity:    a,
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
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "Docs/a.pdf", Size: 1536, SizeKnown: true, Success: true}))
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "b.txt", Success: true}))
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityRemoteDelete, Path: "old.txt", Success: true}))
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityLocalMove, OldPath: "x.txt", Path: "y.txt", Success: true}))
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityFolderCreate, Path: "Docs", Success: true}))

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
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "locked.txt", Err: denied}))
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "busy.txt", WillRetry: true, Err: errors.New("throttled")}))

	assert.Empty(t, out.String())
	assert.Equal(t, "Failed to download locked.txt: permission denied\n", errOut.String(),
		"a retryable failure is not something the user must act on, so default output omits it")

	c.verbose = true
	errOut.Reset()
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "busy.txt", WillRetry: true, Err: errors.New("throttled")}))
	assert.Equal(t, "Could not upload busy.txt, will retry: throttled\n", errOut.String())
}

// Validates: R-6.6.18
func TestSyncConsole_QuietKeepsOnlyFailures(t *testing.T) {
	t.Parallel()

	c, out, errOut := testSyncConsole()
	c.quiet = true
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "a.txt", Success: true}))
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "b.txt", Err: os.ErrPermission}))
	c.Summary(singleMountResult(&syncengine.Report{Downloads: 2, Succeeded: 1, Failed: 1}))

	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "Failed to download b.txt")
}

// Validates: R-6.6.19
func TestSyncConsole_SummaryInHumanUnits(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	for i := range 3 {
		c.Activity(mountActivity(syncengine.Activity{
			Kind: syncengine.ActivityDownload, Path: fmt.Sprintf("f%d", i), Size: sizeMB, SizeKnown: true, Success: true,
		}))
	}
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityFolderCreate, Path: "d", Success: true}))
	out.Reset()

	c.Summary(singleMountResult(&syncengine.Report{Duration: 5571 * time.Millisecond, Downloads: 3, FolderCreates: 1, Succeeded: 4}))

	assert.Equal(t, "Done in 5.6 s: downloaded 3 files (3.0 MB), created 1 folder.\n", out.String())
}

// Validates: R-6.6.19
func TestSyncConsole_SummaryUpToDateAndDryRun(t *testing.T) {
	t.Parallel()

	c, out, _ := testSyncConsole()
	c.Summary(singleMountResult(&syncengine.Report{Duration: 1577 * time.Millisecond}))
	assert.Equal(t, "Up to date (1.6 s).\n", out.String())

	out.Reset()
	c.Summary(singleMountResult(&syncengine.Report{DryRun: true, Downloads: 19, FolderCreates: 2}))
	assert.Equal(t, "Dry run, nothing was changed. Would download 19 files, create 2 folders.\n", out.String())

	out.Reset()
	c.Summary(singleMountResult(&syncengine.Report{DryRun: true}))
	assert.Equal(t, "Dry run: nothing to do.\n", out.String())
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

	c.Summary(result)
	assert.Empty(t, out.String())
	assert.Empty(t, errOut.String(), "the command error prints it; printing here too doubled it")

	err := runOnceResultError(result)
	require.Error(t, err)
	require.ErrorIs(t, err, graph.ErrNotLoggedIn, "the returned error must keep its cause so it is presented as sign-in required")
	assert.Contains(t, commandErrorText(err, false), "Authentication required")
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
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "a.txt", Success: true}))
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
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "a.txt", Err: os.ErrPermission}))

	assert.Equal(t, "Watching /srv/data for user@example.com (download only).\n", out.String(),
		"the journal has no keyboard, so no Ctrl-C hint")
	assert.Equal(t, "<3>Failed to download a.txt: permission denied\n", errOut.String())
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
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityDownload, Path: "a.pdf", Size: 10, SizeKnown: true, Success: true}))
	c.Activity(mountActivity(syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "b.txt", Err: os.ErrPermission}))
	c.Summary(singleMountResult(&syncengine.Report{Duration: 2 * time.Second, Downloads: 1, Uploads: 1, Succeeded: 1, Failed: 1}))

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
	require.NotNil(t, events[3].Failed)
	assert.Equal(t, 1, *events[3].Failed)
	assert.Equal(t, 1, events[3].Counts["download"])
	assert.Empty(t, errOut.String(), "under --json, file failures are data on stdout")
}
