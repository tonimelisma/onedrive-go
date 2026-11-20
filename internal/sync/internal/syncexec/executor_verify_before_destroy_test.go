package syncexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderWithUnobservedChildIsStale(t *testing.T) {
	t.Parallel()

	var deleted []string
	items := syncfixture.FolderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "new-child", Name: "new.txt", ParentID: "folder-x"}},
	}, &deleted)
	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})

	outcome := NewExecution(cfg, syncfixture.EmptyBaseline()).ExecuteRemoteDelete(t.Context(), syncfixture.RemoteFolderDeleteAction())

	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Contains(t, outcome.Error.Error(), "new.txt")
	assert.Empty(t, deleted, "a folder holding content sync never saw must not be deleted recursively")
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderWithOnlySyncedUnchangedChildrenRuns(t *testing.T) {
	t.Parallel()

	var deleted []string
	items := syncfixture.FolderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "known", Name: "a.txt", ParentID: "folder-x", QuickXorHash: "hash-a"}},
	}, &deleted)
	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	bl := syncfixture.BaselineWith(&syncdomain.BaselineEntry{Path: "X/a.txt", ItemID: "known", ItemType: syncdomain.ItemTypeFile, RemoteHash: "hash-a"})

	outcome := NewExecution(cfg, bl).ExecuteRemoteDelete(t.Context(), syncfixture.RemoteFolderDeleteAction())

	syncfixture.RequireOutcomeSuccess(t, &outcome)
	assert.Equal(t, []string{"folder-x"}, deleted)

	// The same child edited on OneDrive since it was synced is not ours to take.
	deleted = nil
	items = syncfixture.FolderDeleteItems(t, map[string][]graph.Item{
		"folder-x": {{ID: "known", Name: "a.txt", ParentID: "folder-x", QuickXorHash: "hash-edited"}},
	}, &deleted)
	cfg, _ = newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	outcome = NewExecution(cfg, bl).ExecuteRemoteDelete(t.Context(), syncfixture.RemoteFolderDeleteAction())
	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Empty(t, deleted)
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_LocalPathReappearedIsSuperseded(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	var deleted []string
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, Name: "a.txt", ParentID: "root", DriveID: driveID, ETag: "etag-a", QuickXorHash: "hash-a"}, nil
		},
		DeleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
			deleted = append(deleted, itemID)
			return nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	syncfixture.WriteExecTestFile(t, syncRoot, "a.txt", "back again\n")

	action := &syncdomain.Action{
		Type: syncdomain.ActionRemoteDelete, Path: "a.txt", ItemID: "item-a", DriveID: driveID,
		View: &syncdomain.PathView{
			Path:   "a.txt",
			Remote: &syncdomain.RemoteState{DriveID: driveID, ItemID: "item-a", ItemType: syncdomain.ItemTypeFile, ETag: "etag-a", Hash: "hash-a"},
		},
	}
	outcome := NewExecution(cfg, syncfixture.EmptyBaseline()).ExecuteRemoteDelete(t.Context(), action)

	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Empty(t, deleted, "a file that is back locally must not be deleted on OneDrive")

	// The sync folder itself was replaced: absence proves nothing.
	require.NoError(t, os.Remove(filepath.Join(syncRoot, "a.txt")))
	identity, err := cfg.syncTree.RootIdentity()
	require.NoError(t, err)
	identity.Inode++
	cfg.SetSyncRootIdentity(syncdomain.FixedSyncRootIdentity(&identity))
	outcome = NewExecution(cfg, syncfixture.EmptyBaseline()).ExecuteRemoteDelete(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &outcome)
	require.ErrorIs(t, outcome.Error, syncdomain.ErrMountRootUnavailable)
	assert.Empty(t, deleted)
}

// Validates: R-2.17.2
func TestDeleteLocalFolder_OnlyIgnoredChildrenConverges(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	cfg.SetContentFilter(syncdomain.ContentFilterConfig{IgnoreDotfiles: true})
	syncfixture.WriteExecTestFile(t, syncRoot, "X/.env", "secret\n")

	action := &syncdomain.Action{
		Type: syncdomain.ActionLocalDelete, Path: "X", ItemID: "folder-x", DriveID: driveid.New(synctest.TestDriveID),
		View: &syncdomain.PathView{
			Path:     "X",
			Baseline: &syncdomain.BaselineEntry{Path: "X", ItemID: "folder-x", ItemType: syncdomain.ItemTypeFolder},
			Local:    &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFolder},
		},
	}
	outcome := NewExecution(cfg, syncfixture.EmptyBaseline()).ExecuteLocalDelete(t.Context(), action)

	syncfixture.RequireOutcomeSuccess(t, &outcome)
	assert.FileExists(t, filepath.Join(syncRoot, "X", ".env"), "ignored content is never deleted")
	assert.True(t, outcome.KeptLocally, "the outcome names the folder it kept")
	kept, err := syncfixture.MustOpenSyncTree(t, syncRoot).IdentityNoFollow("X")
	require.NoError(t, err)
	assert.True(t, outcome.LocalHasIdentity)
	assert.Equal(t, kept.Inode, outcome.LocalInode, "the kept folder's identity tells it from a later folder at the path")
}

// Validates: R-6.7.20
func TestHandleDelete_PathThatStillExistsEmitsNoDelete(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "file.txt"), []byte("still here\n"), 0o600))
	obs := syncobserve.NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	batches := localBatches(obs, 10)

	obs.HandleDelete(t.Context(), syncfixture.NewRecordingFsWatcher(), syncfixture.MustOpenSyncTree(t, syncRoot),
		filepath.Join(syncRoot, "file.txt"), "file.txt", "file.txt")

	assert.Empty(t, drainLocalPatches(batches), "a delete event for a path that still exists must not become a delete")
}

// Validates: R-2.8.10
func TestExecuteRemoteDelete_FolderChildChangedOnlyInETagOrSizeIsStale(t *testing.T) {
	t.Parallel()

	known := &syncdomain.BaselineEntry{
		Path: "X/a.txt", ItemID: "known", ItemType: syncdomain.ItemTypeFile, RemoteHash: "hash-a",
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
			items := syncfixture.FolderDeleteItems(t, map[string][]graph.Item{"folder-x": {child}}, &deleted)
			cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})

			outcome := NewExecution(cfg, syncfixture.BaselineWith(known)).ExecuteRemoteDelete(t.Context(), syncfixture.RemoteFolderDeleteAction())

			syncfixture.RequireOutcomeFailure(t, &outcome)
			require.ErrorIs(t, outcome.Error, syncdomain.ErrActionPreconditionChanged)
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
				syncfixture.WriteExecTestFile(t, syncRoot, "X", "now a file\n")
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
			items := &syncfixture.ExecutorMockItemClient{
				GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
					return &graph.Item{ID: itemID, Name: "a.txt", ParentID: "parent", DriveID: driveID, ETag: "etag-a", QuickXorHash: "hash-a"}, nil
				},
				DeleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
					deleted = append(deleted, itemID)
					return nil
				},
			}
			cfg, syncRoot := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
			tc.local(t, syncRoot)
			action := &syncdomain.Action{
				Type: syncdomain.ActionRemoteDelete, Path: tc.path, ItemID: "item-a", DriveID: driveID,
				View: &syncdomain.PathView{
					Path:   tc.path,
					Remote: &syncdomain.RemoteState{DriveID: driveID, ItemID: "item-a", ItemType: syncdomain.ItemTypeFile, ETag: "etag-a", Hash: "hash-a"},
				},
			}

			outcome := NewExecution(cfg, syncfixture.EmptyBaseline()).ExecuteRemoteDelete(t.Context(), action)

			syncfixture.RequireOutcomeSuccess(t, &outcome)
			assert.Equal(t, []string{"item-a"}, deleted)
		})
	}
}
