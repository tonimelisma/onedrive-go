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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The synced folder X and its child X/a.txt of the engine-level scenarios.
const (
	syncedFolderItemID      = "item-x"
	syncedFolderChildItemID = "item-a"
)

// Validates: R-2.8.10
func TestRunOnce_LocalFolderDeleteKeepsRemoteChildAddedAfterObservation(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-x", "root", "X")
	remote.addFile(t, "item-a", "item-x", "a.txt", "a\n")
	client := remote.client(t)
	injected := false
	baseGetItem := client.GetItemFn
	client.GetItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
		// Someone adds X/new.txt on OneDrive after this pass observed the
		// tree, right before the folder delete's preflight.
		if itemID == syncedFolderItemID && !injected {
			injected = true
			remote.mu.Lock()
			remote.addFile(t, "item-new", "item-x", "new.txt", "added online\n")
			remote.mu.Unlock()
		}
		return baseGetItem(ctx, driveID, itemID)
	}

	eng, syncRoot := newTestEngine(t, client)
	runScenarioPass(t, eng)
	injected = false
	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "X")))

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Equal(t, "added online\n", remote.tree()["X/new.txt"], "the unseen child must survive on OneDrive")
	assert.NotContains(t, remote.deletes, "item-x")

	runScenarioPass(t, eng)
	data, err := os.ReadFile(filepath.Join(syncRoot, "X", "new.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "added online\n", string(data), "the next pass brings the new child down")
}

// Validates: R-2.17.2
func TestRunOnce_RemoteFolderDeleteWithIgnoredLocalFileConverges(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-x", "root", "X")
	remote.addFile(t, "item-a", "item-x", "a.txt", "a\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	filter := syncdomain.ContentFilterConfig{IgnoreDotfiles: true}
	eng.contentFilter = filter
	eng.execCfg.SetContentFilter(filter)
	runScenarioPass(t, eng)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "X", ".env"), []byte("local only\n"), 0o600))

	remote.deleteRemotely("item-x")
	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.Failed, "errors: %v", report.Errors)

	assert.FileExists(t, filepath.Join(syncRoot, "X", ".env"), "ignored content is never deleted")
	assert.NoFileExists(t, filepath.Join(syncRoot, "X", "a.txt"))

	runScenarioPass(t, eng)
	assert.NotContains(t, remote.tree(), "X", "the folder deleted online stays deleted online")
	assert.Empty(t, remote.folderCreates)
	requireScenarioQuietPlan(t, eng)

	// Once it holds something sync sees, the kept folder is local content
	// again: it and the new file are created online.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "X", "new.txt"), []byte("new\n"), 0o600))
	runScenarioPass(t, eng)
	assert.Equal(t, "new\n", remote.tree()["X/new.txt"])
	assert.NotContains(t, remote.tree(), "X/.env", "ignored content is never uploaded")
	requireScenarioQuietPlan(t, eng)
}

// The folder kept after its online deletion is remembered as that folder, not
// as its path: removed and created again by the user, the new folder is
// ordinary local content and is created online.
//
// Validates: R-2.17.2
func TestRunOnce_FolderRecreatedWhereAKeptFolderWasIsCreatedOnline(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-x", "root", "X")
	remote.addFile(t, "item-a", "item-x", "a.txt", "a\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	filter := syncdomain.ContentFilterConfig{IgnoreDotfiles: true}
	eng.contentFilter = filter
	eng.execCfg.SetContentFilter(filter)
	runScenarioPass(t, eng)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "X", ".env"), []byte("local only\n"), 0o600))
	remote.deleteRemotely("item-x")
	runScenarioPass(t, eng)
	require.NoFileExists(t, filepath.Join(syncRoot, "X", "a.txt"))
	require.NotContains(t, remote.tree(), "X", "the kept folder stays deleted online")

	kept, err := syncfixture.MustOpenSyncTree(t, syncRoot).IdentityNoFollow("X")
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "X")))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "X"), 0o700))
	recreated, err := syncfixture.MustOpenSyncTree(t, syncRoot).IdentityNoFollow("X")
	require.NoError(t, err)
	if kept.BirthTime == 0 || recreated.BirthTime == 0 {
		t.Skip("the test filesystem reports no birth time, so a reused inode cannot be told apart")
	}

	runScenarioPass(t, eng)
	assert.Equal(t, "/", remote.tree()["X"], "a new folder at the path is created online")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderOverListingCapIsDurableIssueNotStale(t *testing.T) {
	t.Parallel()

	children := make([]graph.Item, 0, syncexec.RemoteFolderDeleteListLimit+1)
	entries := make([]*syncdomain.BaselineEntry, 0, syncexec.RemoteFolderDeleteListLimit+1)
	for i := range syncexec.RemoteFolderDeleteListLimit + 1 {
		id := fmt.Sprintf("c%05d", i)
		children = append(children, graph.Item{ID: id, Name: id, ParentID: "folder-x", IsFolder: true})
		entries = append(entries, &syncdomain.BaselineEntry{Path: "X/" + id, ItemID: id, ItemType: syncdomain.ItemTypeFolder})
	}
	var deleted []string
	items := syncfixture.FolderDeleteItems(t, map[string][]graph.Item{"folder-x": children}, &deleted)
	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})

	outcome := syncexec.NewExecution(cfg, syncfixture.BaselineWith(entries...)).ExecuteRemoteDelete(t.Context(), syncfixture.RemoteFolderDeleteAction())

	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, syncexec.ErrRemoteFolderTooLargeToVerify)
	require.NotErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged, "a stale verdict would relist the folder every pass")
	issue, ok := issueTypeForFilesystemResult(&syncdomain.ActionCompletion{Err: outcome.Error})
	require.True(t, ok, "the failure maps to a durable issue")
	assert.Equal(t, syncdomain.IssueRemoteFolderUnverifiable, issue)
	assert.Empty(t, deleted)
}

// syncedFolderScenario is a synced tree of keep.txt and X/a.txt whose folder
// X the user then removes locally, so the next pass plans remote deletes of
// X/a.txt and then X.
func syncedFolderScenario(t *testing.T, client *syncfixture.EngineMockClient, remote *moveScenarioRemote) (*testEngine, string) {
	t.Helper()

	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-x", "root", "X")
	remote.addFile(t, "item-a", "item-x", "a.txt", "a\n")
	eng, syncRoot := newTestEngine(t, client)
	eng.hooks.serializeWorkers = true
	runScenarioPass(t, eng)
	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "X")))

	return eng, syncRoot
}

// overListedFolderClient is remote's client with X answering its listing
// with more live children than the cap allows, and a count of those
// listings. Each child is a synced, unchanged item, so nothing but their
// number stops the delete; the cap is counted before any child is looked at.
func overListedFolderClient(t *testing.T, remote *moveScenarioRemote) (*syncfixture.EngineMockClient, *int) {
	t.Helper()

	client := remote.client(t)
	baseList := client.ListChildrenFn
	folderListings := new(int)
	client.ListChildrenFn = func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
		if parentID != syncedFolderItemID {
			return baseList(ctx, driveID, parentID)
		}
		*folderListings++
		keep, err := remote.getItem("item-keep")
		require.NoError(t, err)
		children := make([]graph.Item, 0, syncexec.RemoteFolderDeleteListLimit+1)
		for range syncexec.RemoteFolderDeleteListLimit + 1 {
			children = append(children, *keep)
		}
		return children, nil
	}

	return client, folderListings
}

// Validates: R-2.8.10
func TestRunOnce_RemoteFolderDeleteOverListingCapIsADurableIssueNotRelistedEveryPass(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client, folderListings := overListedFolderClient(t, remote)
	eng, syncRoot := syncedFolderScenario(t, client, remote)
	// The retry row's due time and the passes read one clock the test
	// advances, so whether the second pass finds the row due is decided
	// here, not by how long a pass takes on the host.
	clock := syncfixture.NewManualClock(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	require.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	require.Len(t, report.Errors, 1)
	require.ErrorIs(t, report.Errors[0], syncexec.ErrRemoteFolderTooLargeToVerify)
	assert.NotContains(t, remote.deletes, syncedFolderItemID, "the folder is not deleted recursively")
	assert.Contains(t, remote.tree(), "X")
	assert.Equal(t, 1, *folderListings)
	retryRows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, retryRows, 1, "the refused delete is recorded as a durable issue")
	assert.Equal(t, "X", retryRows[0].Path)
	assert.Equal(t, syncdomain.ActionRemoteDelete, retryRows[0].ActionType)
	assert.False(t, retryRows[0].Blocked)
	assert.Equal(t, 1, retryRows[0].AttemptCount)
	assert.Greater(t, retryRows[0].NextRetryAt, clock.Now().UnixNano(), "the issue backs off before it is tried again")

	// A pass before the row is due neither relists nor deletes the folder.
	// The issue does not clear by itself, so it waits on the durable curve
	// (30 s doubling to an hour): a pass fifteen seconds later, which the
	// transient curve's one-second first step would have retried, still
	// does not relist the 10,000 items.
	clock.Advance(durableRecheckDelay / 2)
	report, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.Failed, "errors: %v", report.Errors)
	assert.Equal(t, 1, *folderListings, "a folder too large to verify is not relisted while its retry backs off")
	assert.NotContains(t, remote.deletes, syncedFolderItemID)
	assert.NoDirExists(t, filepath.Join(syncRoot, "X"), "the local delete stands; nothing is downloaded back")

	// Once due, the retry lists the folder once more, is refused again and
	// backs off further; the folder is still not deleted.
	clock.Advance(retryBackoffCeilingForTest)
	report, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	assert.Equal(t, 2, *folderListings, "a due retry lists the folder once")
	assert.NotContains(t, remote.deletes, syncedFolderItemID)
	retryRows, err = eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, retryRows, 1)
	assert.Equal(t, 2, retryRows[0].AttemptCount)
	assert.Greater(t, retryRows[0].NextRetryAt, clock.Now().UnixNano())
}

// retryBackoffCeilingForTest is longer than any retry backoff (the reconcile
// curve tops out at an hour), so advancing the clock by it makes every
// retry row due.
const retryBackoffCeilingForTest = 2 * time.Hour

// The child's own delete is what sees the edit first: its preflight
// supersedes it, and the folder delete, which depends on it, is never
// dispatched. The listing gate's "changed since synced" check is the backstop
// for a child the plan did not delete, exercised by the executor tests above.
//
// Validates: R-2.8.10
func TestRunOnce_RemoteFolderDeleteRefusesAChildEditedOnlineAfterObservation(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client := remote.client(t)
	baseGetItem := client.GetItemFn
	armed := false
	client.GetItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
		// Someone saves X/a.txt on OneDrive after this pass observed the
		// tree, right before its delete's preflight.
		if itemID == syncedFolderChildItemID && armed {
			armed = false
			remote.mu.Lock()
			remote.setContent(t, "item-a", "edited online\n")
			remote.mu.Unlock()
		}
		return baseGetItem(ctx, driveID, itemID)
	}
	eng, syncRoot := syncedFolderScenario(t, client, remote)
	armed = true

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Zero(t, report.Failed, "a superseded delete is not a failure: %v", report.Errors)
	assert.Empty(t, remote.deletes, "neither the edited child nor the folder holding it is deleted")
	assert.Equal(t, "edited online\n", remote.tree()["X/a.txt"], "the online edit survives")

	// The edit wins over the local delete: the next pass brings it down.
	runScenarioPass(t, eng)
	data, err := os.ReadFile(filepath.Join(syncRoot, "X", "a.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "edited online\n", string(data))
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.8.10
func TestRunOnce_RemoteFolderDeleteRefusedWhenTheFolderIsBackLocally(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client := remote.client(t)
	baseGetItem := client.GetItemFn
	var syncRoot string
	armed := false
	client.GetItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
		// The user creates X again after this pass observed it gone, before
		// the folder's own delete re-checks its absence.
		if itemID == syncedFolderChildItemID && armed {
			armed = false
			require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "X"), 0o700))
		}
		return baseGetItem(ctx, driveID, itemID)
	}
	var eng *testEngine
	eng, syncRoot = syncedFolderScenario(t, client, remote)
	armed = true

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Zero(t, report.Failed, "a superseded delete is not a failure: %v", report.Errors)
	assert.Equal(t, []string{"item-a"}, remote.deletes, "the child, still absent, is deleted; the folder that is back is not")
	assert.Contains(t, remote.tree(), "X")
	assert.DirExists(t, filepath.Join(syncRoot, "X"))

	runScenarioPass(t, eng)
	assert.NotContains(t, remote.deletes, "item-x")
	requireScenarioQuietPlan(t, eng)
}
