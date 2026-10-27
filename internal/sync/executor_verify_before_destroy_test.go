package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func remoteFolderDeleteAction(path, itemID string) *Action {
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

	outcome := newExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction("X", "folder-x"))

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

	outcome := newExecution(cfg, bl).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction("X", "folder-x"))

	requireOutcomeSuccess(t, &outcome)
	assert.Equal(t, []string{"folder-x"}, deleted)

	// The same child edited on OneDrive since it was synced is not ours to take.
	deleted = nil
	items = folderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "known", Name: "a.txt", ParentID: "folder-x", QuickXorHash: "hash-edited"}},
	}, &deleted)
	cfg, _ = newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	outcome = newExecution(cfg, bl).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction("X", "folder-x"))
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
	cfg.SetExpectedSyncRoot(&identity)
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
		if itemID == "item-x" && !injected {
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
	data, err := os.ReadFile(filepath.Join(syncRoot, "X", "new.txt"))
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
	assert.Equal(t, "/", remote.tree()["X"], "the kept folder is re-created, empty, on OneDrive")
	requireScenarioQuietPlan(t, eng)
}
