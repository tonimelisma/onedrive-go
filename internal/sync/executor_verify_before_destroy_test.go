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

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderWithUnobservedChildIsStale(t *testing.T) {
	t.Parallel()

	var deleted []string
	items := folderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "new-child", Name: "new.txt", ParentID: "folder-x"}},
	}, &deleted)
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})

	outcome := NewExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())

	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, ErrActionPreconditionChanged)
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

	outcome := NewExecution(cfg, bl).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())

	requireOutcomeSuccess(t, &outcome)
	assert.Equal(t, []string{"folder-x"}, deleted)

	// The same child edited on OneDrive since it was synced is not ours to take.
	deleted = nil
	items = folderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "known", Name: "a.txt", ParentID: "folder-x", QuickXorHash: "hash-edited"}},
	}, &deleted)
	cfg, _ = newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	outcome = NewExecution(cfg, bl).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())
	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, ErrActionPreconditionChanged)
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
		View: &PathView{
			Path:   "a.txt",
			Remote: &RemoteState{DriveID: driveID, ItemID: "item-a", ItemType: ItemTypeFile, ETag: "etag-a", Hash: "hash-a"},
		},
	}
	outcome := NewExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), action)

	requireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, ErrActionPreconditionChanged)
	assert.Empty(t, deleted, "a file that is back locally must not be deleted on OneDrive")

	// The sync folder itself was replaced: absence proves nothing.
	require.NoError(t, os.Remove(filepath.Join(syncRoot, "a.txt")))
	identity, err := cfg.syncTree.RootIdentity()
	require.NoError(t, err)
	identity.Inode++
	cfg.SetSyncRootIdentity(FixedSyncRootIdentity(&identity))
	outcome = NewExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), action)
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
		View: &PathView{
			Path:     "X",
			Baseline: &BaselineEntry{Path: "X", ItemID: "folder-x", ItemType: ItemTypeFolder},
			Local:    &LocalState{ItemType: ItemTypeFolder},
		},
	}
	outcome := NewExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), action)

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
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	batches := localBatches(obs, 10)

	obs.HandleDelete(t.Context(), newRecordingFsWatcher(), mustOpenSyncTree(t, syncRoot),
		filepath.Join(syncRoot, "file.txt"), "file.txt", "file.txt")

	assert.Empty(t, drainLocalPatches(batches), "a delete event for a path that still exists must not become a delete")
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

			outcome := NewExecution(cfg, baselineWith(known)).ExecuteRemoteDelete(t.Context(), remoteFolderDeleteAction())

			requireOutcomeFailure(t, &outcome)
			require.ErrorIs(t, outcome.Error, ErrActionPreconditionChanged)
			assert.Empty(t, deleted)
		})
	}
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
				View: &PathView{
					Path:   tc.path,
					Remote: &RemoteState{DriveID: driveID, ItemID: "item-a", ItemType: ItemTypeFile, ETag: "etag-a", Hash: "hash-a"},
				},
			}

			outcome := NewExecution(cfg, emptyBaseline()).ExecuteRemoteDelete(t.Context(), action)

			requireOutcomeSuccess(t, &outcome)
			assert.Equal(t, []string{"item-a"}, deleted)
		})
	}
}
