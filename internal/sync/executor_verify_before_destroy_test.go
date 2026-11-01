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
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// The synced folder X and its child X/a.txt of the engine-level scenarios.
const (
	syncedFolderItemID      = "item-x"
	syncedFolderChildItemID = "item-a"
)

func remoteFolderDeleteAction() *Action {
	const (
		path   = "X"
		itemID = "folder-x"
	)
	driveID := driveid.New(synctest.TestDriveID)
	return &Action{
		Type:    ActionRemoteDelete,
		Path:    path,
		ItemID:  itemID,
		DriveID: driveID,
		View: &pathView{
			Path:     path,
			Baseline: &BaselineEntry{Path: path, ItemID: itemID, ItemType: ItemTypeFolder},
			Remote:   &remoteState{DriveID: driveID, ItemID: itemID, ItemType: ItemTypeFolder, ETag: "etag-folder"},
		},
	}
}

func folderDeleteItems(t *testing.T, children map[string][]graph.Item, deleted *[]string) *executorMockItemClient {
	t.Helper()

	driveID := driveid.New(synctest.TestDriveID)
	return &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, Name: "X", ParentID: "root", IsFolder: true, DriveID: driveID, ETag: "etag-live"}, nil
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, parentID string) ([]graph.Item, error) {
			return children[parentID], nil
		},
		deleteItemFn: func(_ context.Context, _ driveid.ID, itemID string) error {
			*deleted = append(*deleted, itemID)
			return nil
		},
		deleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
			*deleted = append(*deleted, itemID)
			return nil
		},
	}
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderWithUnobservedChildIsStale(t *testing.T) {
	t.Parallel()

	var deleted []string
	items := folderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "new-child", Name: "new.txt", ParentID: "folder-x"}},
	}, &deleted)
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})

	outcome := newExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())

	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, errActionPreconditionChanged)
	assert.Contains(t, outcome.Error.Error(), "new.txt")
	assert.Empty(t, deleted, "a folder holding content sync never saw must not be deleted recursively")
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderWithOnlySyncedUnchangedChildrenRuns(t *testing.T) {
	t.Parallel()

	var deleted []string
	items := folderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "known", Name: "a.txt", ParentID: "folder-x", QuickXorHash: "hash-a"}},
	}, &deleted)
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	bl := baselineWith(&BaselineEntry{Path: "X/a.txt", ItemID: "known", ItemType: ItemTypeFile, RemoteHash: "hash-a"})

	outcome := newExecution(cfg, bl).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())

	requireOutcomeSuccess(t, &outcome)
	assert.Equal(t, []string{"folder-x"}, deleted)

	// The same child edited on OneDrive since it was synced is not ours to take.
	deleted = nil
	items = folderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "known", Name: "a.txt", ParentID: "folder-x", QuickXorHash: "hash-edited"}},
	}, &deleted)
	cfg, _ = newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	outcome = newExecution(cfg, bl).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())
	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, errActionPreconditionChanged)
	assert.Empty(t, deleted)
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_LocalPathReappearedIsSuperseded(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	var deleted []string
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, Name: "a.txt", ParentID: "root", DriveID: driveID, ETag: "etag-a", QuickXorHash: "hash-a"}, nil
		},
		deleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
			deleted = append(deleted, itemID)
			return nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	writeExecTestFile(t, syncRoot, "a.txt", "back again\n")

	action := &Action{
		Type: ActionRemoteDelete, Path: "a.txt", ItemID: "item-a", DriveID: driveID,
		View: &pathView{
			Path:   "a.txt",
			Remote: &remoteState{DriveID: driveID, ItemID: "item-a", ItemType: ItemTypeFile, ETag: "etag-a", Hash: "hash-a"},
		},
	}
	outcome := newExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), action)

	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, errActionPreconditionChanged)
	assert.Empty(t, deleted, "a file that is back locally must not be deleted on OneDrive")

	// The sync folder itself was replaced: absence proves nothing.
	require.NoError(t, os.Remove(filepath.Join(syncRoot, "a.txt")))
	identity, err := cfg.syncTree.RootIdentity()
	require.NoError(t, err)
	identity.Inode++
	cfg.SetSyncRootIdentity(fixedSyncRootIdentity(&identity))
	outcome = newExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), action)
	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, ErrMountRootUnavailable)
	assert.Empty(t, deleted)
}

// Validates: R-2.17.2
func TestDeleteLocalFolder_OnlyIgnoredChildrenConverges(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	cfg.SetContentFilter(ContentFilterConfig{IgnoreDotfiles: true})
	writeExecTestFile(t, syncRoot, "X/.env", "secret\n")

	action := &Action{
		Type: ActionLocalDelete, Path: "X", ItemID: "folder-x", DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path:     "X",
			Baseline: &BaselineEntry{Path: "X", ItemID: "folder-x", ItemType: ItemTypeFolder},
			Local:    &localState{ItemType: ItemTypeFolder},
		},
	}
	outcome := newExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), action)

	requireOutcomeSuccess(t, &outcome)
	assert.FileExists(t, filepath.Join(syncRoot, "X", ".env"), "ignored content is never deleted")
	assert.True(t, outcome.KeptLocally, "the outcome names the folder it kept")
	kept, err := mustOpenSyncTree(t, syncRoot).IdentityNoFollow("X")
	require.NoError(t, err)
	assert.True(t, outcome.LocalHasIdentity)
	assert.Equal(t, kept.Inode, outcome.LocalInode, "the kept folder's identity tells it from a later folder at the path")
}

// Validates: R-6.7.20
func TestHandleDelete_PathThatStillExistsEmitsNoDelete(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "file.txt"), []byte("still here\n"), 0o600))
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	events := make(chan changeEvent, 10)

	obs.HandleDelete(t.Context(), newRecordingFsWatcher(), mustOpenSyncTree(t, syncRoot),
		filepath.Join(syncRoot, "file.txt"), "file.txt", "file.txt", events)

	assert.Empty(t, events, "a delete event for a path that still exists must not become a delete")
}

// Validates: R-2.8.10
func TestRunOnce_LocalFolderDeleteKeepsRemoteChildAddedAfterObservation(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-x", "root", "X")
	remote.addFile(t, "item-a", "item-x", "a.txt", "a\n")
	client := remote.client(t)
	injected := false
	baseGetItem := client.getItemFn
	client.getItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
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

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
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

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-x", "root", "X")
	remote.addFile(t, "item-a", "item-x", "a.txt", "a\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	filter := ContentFilterConfig{IgnoreDotfiles: true}
	eng.contentFilter = filter
	eng.execCfg.SetContentFilter(filter)
	runScenarioPass(t, eng)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "X", ".env"), []byte("local only\n"), 0o600))

	remote.deleteRemotely("item-x")
	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
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

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-x", "root", "X")
	remote.addFile(t, "item-a", "item-x", "a.txt", "a\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	filter := ContentFilterConfig{IgnoreDotfiles: true}
	eng.contentFilter = filter
	eng.execCfg.SetContentFilter(filter)
	runScenarioPass(t, eng)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "X", ".env"), []byte("local only\n"), 0o600))
	remote.deleteRemotely("item-x")
	runScenarioPass(t, eng)
	require.NoFileExists(t, filepath.Join(syncRoot, "X", "a.txt"))
	require.NotContains(t, remote.tree(), "X", "the kept folder stays deleted online")

	kept, err := mustOpenSyncTree(t, syncRoot).IdentityNoFollow("X")
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "X")))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "X"), 0o700))
	recreated, err := mustOpenSyncTree(t, syncRoot).IdentityNoFollow("X")
	require.NoError(t, err)
	if kept.BirthTime == 0 || recreated.BirthTime == 0 {
		t.Skip("the test filesystem reports no birth time, so a reused inode cannot be told apart")
	}

	runScenarioPass(t, eng)
	assert.Equal(t, "/", remote.tree()["X"], "a new folder at the path is created online")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderChildChangedOnlyInETagOrSizeIsStale(t *testing.T) {
	t.Parallel()

	known := &BaselineEntry{
		Path: "X/a.txt", ItemID: "known", ItemType: ItemTypeFile, RemoteHash: "hash-a",
		ETag: "etag-synced", RemoteSize: 10, RemoteSizeKnown: true,
	}
	cases := map[string]graph.Item{
		"eTag changed": {ID: "known", Name: "a.txt", ParentID: "folder-x", QuickXorHash: "hash-a", ETag: "etag-new", Size: 10},
		"size changed": {ID: "known", Name: "a.txt", ParentID: "folder-x", ETag: "etag-synced", Size: 11},
	}
	for name, child := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var deleted []string
			items := folderDeleteItems(t, map[string][]graph.Item{"folder-x": {child}}, &deleted)
			cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})

			outcome := newExecution(cfg, baselineWith(known)).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())

			requireOutcomeFailure(t, &outcome)
			require.ErrorIs(t, outcome.Error, errActionPreconditionChanged)
			assert.Empty(t, deleted)
		})
	}
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderOverListingCapIsDurableIssueNotStale(t *testing.T) {
	t.Parallel()

	children := make([]graph.Item, 0, remoteFolderDeleteListLimit+1)
	entries := make([]*BaselineEntry, 0, remoteFolderDeleteListLimit+1)
	for i := range remoteFolderDeleteListLimit + 1 {
		id := fmt.Sprintf("c%05d", i)
		children = append(children, graph.Item{ID: id, Name: id, ParentID: "folder-x", IsFolder: true})
		entries = append(entries, &BaselineEntry{Path: "X/" + id, ItemID: id, ItemType: ItemTypeFolder})
	}
	var deleted []string
	items := folderDeleteItems(t, map[string][]graph.Item{"folder-x": children}, &deleted)
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})

	outcome := newExecution(cfg, baselineWith(entries...)).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())

	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, errRemoteFolderTooLargeToVerify)
	require.NotErrorIs(t, outcome.Error, errActionPreconditionChanged, "a stale verdict would relist the folder every pass")
	issue, ok := issueTypeForFilesystemResult(&actionCompletion{Err: outcome.Error})
	require.True(t, ok, "the failure maps to a durable issue")
	assert.Equal(t, issueRemoteFolderUnverifiable, issue)
	assert.Empty(t, deleted)
}

// syncedFolderScenario is a synced tree of keep.txt and X/a.txt whose folder
// X the user then removes locally, so the next pass plans remote deletes of
// X/a.txt and then X.
func syncedFolderScenario(t *testing.T, client *engineMockClient, remote *moveScenarioRemote) (*testEngine, string) {
	t.Helper()

	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFolder("item-x", "root", "X")
	remote.addFile(t, "item-a", "item-x", "a.txt", "a\n")
	eng, syncRoot := newTestEngine(t, client)
	eng.serializeWorkers = true
	runScenarioPass(t, eng)
	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "X")))

	return eng, syncRoot
}

// overListedFolderClient is remote's client with X answering its listing
// with more live children than the cap allows, and a count of those
// listings. Each child is a synced, unchanged item, so nothing but their
// number stops the delete; the cap is counted before any child is looked at.
func overListedFolderClient(t *testing.T, remote *moveScenarioRemote) (*engineMockClient, *int) {
	t.Helper()

	client := remote.client(t)
	baseList := client.listChildrenFn
	folderListings := new(int)
	client.listChildrenFn = func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
		if parentID != syncedFolderItemID {
			return baseList(ctx, driveID, parentID)
		}
		*folderListings++
		keep, err := remote.getItem("item-keep")
		require.NoError(t, err)
		children := make([]graph.Item, 0, remoteFolderDeleteListLimit+1)
		for range remoteFolderDeleteListLimit + 1 {
			children = append(children, *keep)
		}
		return children, nil
	}

	return client, folderListings
}

// Validates: R-2.8.10
func TestRunOnce_RemoteFolderDeleteOverListingCapIsADurableIssueNotRelistedEveryPass(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client, folderListings := overListedFolderClient(t, remote)
	eng, syncRoot := syncedFolderScenario(t, client, remote)
	// The retry row's due time and the passes read one clock the test
	// advances, so whether the second pass finds the row due is decided
	// here, not by how long a pass takes on the host.
	clock := newManualClock(time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	require.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	require.Len(t, report.Errors, 1)
	require.ErrorIs(t, report.Errors[0], errRemoteFolderTooLargeToVerify)
	assert.NotContains(t, remote.deletes, syncedFolderItemID, "the folder is not deleted recursively")
	assert.Contains(t, remote.tree(), "X")
	assert.Equal(t, 1, *folderListings)
	retryRows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, retryRows, 1, "the refused delete is recorded as a durable issue")
	assert.Equal(t, "X", retryRows[0].Path)
	assert.Equal(t, ActionRemoteDelete, retryRows[0].ActionType)
	assert.False(t, retryRows[0].Blocked)
	assert.Equal(t, 1, retryRows[0].AttemptCount)
	assert.Greater(t, retryRows[0].NextRetryAt, clock.Now().UnixNano(), "the issue backs off before it is tried again")

	// A pass before the row is due neither relists nor deletes the folder.
	report, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.Failed, "errors: %v", report.Errors)
	assert.Equal(t, 1, *folderListings, "a folder too large to verify is not relisted while its retry backs off")
	assert.NotContains(t, remote.deletes, syncedFolderItemID)
	assert.NoDirExists(t, filepath.Join(syncRoot, "X"), "the local delete stands; nothing is downloaded back")

	// Once due, the retry lists the folder once more, is refused again and
	// backs off further; the folder is still not deleted.
	clock.Advance(retryBackoffCeilingForTest)
	report, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
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

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	baseGetItem := client.getItemFn
	armed := false
	client.getItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
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

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
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

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	baseGetItem := client.getItemFn
	var syncRoot string
	armed := false
	client.getItemFn = func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
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

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Zero(t, report.Failed, "a superseded delete is not a failure: %v", report.Errors)
	assert.Equal(t, []string{"item-a"}, remote.deletes, "the child, still absent, is deleted; the folder that is back is not")
	assert.Contains(t, remote.tree(), "X")
	assert.DirExists(t, filepath.Join(syncRoot, "X"))

	runScenarioPass(t, eng)
	assert.NotContains(t, remote.deletes, "item-x")
	requireScenarioQuietPlan(t, eng)
}

// Below a regular file nothing can exist, and an entry of the other kind at
// the path is a type-change replacement: neither is the deleted item back,
// so neither blocks its remote delete.
//
// Validates: R-2.8.10
func TestExecuteRemoteDelete_LocalAbsenceHoldsBelowAFileAndAcrossATypeChange(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	cases := map[string]struct {
		path  string
		local func(t *testing.T, syncRoot string)
	}{
		"folder replaced by a file": {
			path: "X/a.txt",
			local: func(t *testing.T, syncRoot string) {
				t.Helper()
				writeExecTestFile(t, syncRoot, "X", "now a file\n")
			},
		},
		"file replaced by a folder": {
			path: "a.txt",
			local: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "a.txt"), 0o700))
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var deleted []string
			items := &executorMockItemClient{
				getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
					return &graph.Item{ID: itemID, Name: "a.txt", ParentID: "parent", DriveID: driveID, ETag: "etag-a", QuickXorHash: "hash-a"}, nil
				},
				deleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
					deleted = append(deleted, itemID)
					return nil
				},
			}
			cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
			tc.local(t, syncRoot)
			action := &Action{
				Type: ActionRemoteDelete, Path: tc.path, ItemID: "item-a", DriveID: driveID,
				View: &pathView{
					Path:   tc.path,
					Remote: &remoteState{DriveID: driveID, ItemID: "item-a", ItemType: ItemTypeFile, ETag: "etag-a", Hash: "hash-a"},
				},
			}

			outcome := newExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), action)

			requireOutcomeSuccess(t, &outcome)
			assert.Equal(t, []string{"item-a"}, deleted)
		})
	}
}
