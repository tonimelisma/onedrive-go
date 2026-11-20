package syncobserve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// Watch tests — file/directory deletion
// ---------------------------------------------------------------------------

// Validates: R-2.1.2
func TestWatch_DetectsFileDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	syncfixture.WriteTestFile(t, dir, "doomed.txt", "goodbye")

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "doomed.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: syncdomain.ItemTypeFile,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	batches := localBatches(obs, 10)
	cancel, done := startLocalWatch(t, obs, dir)

	require.NoError(t, os.Remove(filepath.Join(dir, "doomed.txt")))

	waitForLocalRemoval(t, batches, "doomed.txt")
	syncfixture.StopWatch(t, cancel, done)
}

// TestWatch_DeleteDirectoryRemovesWatch verifies that deleting a watched
// directory removes its subtree from local truth and drops its watch
// (B-112).
// Validates: R-2.1.2
func TestWatch_DeleteDirectoryRemovesWatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o700))

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "subdir", DriveID: driveid.New("d"), ItemID: "d1",
		ItemType: syncdomain.ItemTypeFolder,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	batches := localBatches(obs, 10)
	cancel, done := startLocalWatch(t, obs, dir)
	require.True(t, obs.watchedDirs.has(subDir), "the directory is watched before its delete")

	// Delete the subdirectory.
	require.NoError(t, os.Remove(subDir))

	waitForLocalRemoval(t, batches, "subdir")
	syncfixture.StopWatch(t, cancel, done)

	assert.False(t, obs.watchedDirs.has(subDir), "a deleted directory's watch is dropped")
}

// Validates: R-2.4.6
func TestHandleFsEvent_DefaultSymlinkPolicyIgnoresTransientSymlinkDelete(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	syncfixture.WriteTestFile(t, syncRoot, "real.txt", "content")

	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetFilterConfig(syncdomain.ContentFilterConfig{FollowSymlinks: false})

	watcher := syncfixture.NewRecordingFsWatcher()
	batches := localBatches(obs, 4)
	tree := syncfixture.MustOpenSyncTree(t, syncRoot)
	linkPath := filepath.Join(syncRoot, "link.txt")

	require.NoError(t, os.Symlink(filepath.Join(syncRoot, "real.txt"), linkPath))

	obs.HandleFsEvent(
		t.Context(),
		fsnotify.Event{Name: linkPath, Op: fsnotify.Create},
		watcher,
		tree,
	)

	require.NoError(t, os.Remove(linkPath))

	obs.HandleFsEvent(
		t.Context(),
		fsnotify.Event{Name: linkPath, Op: fsnotify.Remove},
		watcher,
		tree,
	)

	requireNoLocalPatch(t, batches, "the skipped transient symlink's events")
}

// Validates: R-2.4.6
func TestSkipSymlinkDelete_RemainsIgnoredThroughSafetyScan(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	syncfixture.WriteTestFile(t, syncRoot, "real.txt", "content")
	linkPath := filepath.Join(syncRoot, "link.txt")
	require.NoError(t, os.Symlink(filepath.Join(syncRoot, "real.txt"), linkPath))

	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{
			Path: "real.txt", DriveID: driveid.New("d"), ItemID: "real",
			ItemType: syncdomain.ItemTypeFile, LocalHash: hashContent(t, "content"),
		},
		&syncdomain.BaselineEntry{
			Path: "link.txt", DriveID: driveid.New("d"), ItemID: "link",
			ItemType: syncdomain.ItemTypeFile, LocalHash: hashContent(t, "content"),
		},
	)

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.SetFilterConfig(syncdomain.ContentFilterConfig{FollowSymlinks: false})

	watcher := syncfixture.NewRecordingFsWatcher()
	tree := syncfixture.MustOpenSyncTree(t, syncRoot)
	require.NoError(t, obs.AddWatchesRecursive(t.Context(), watcher, tree))

	require.NoError(t, os.Remove(linkPath))

	batches := localBatches(obs, 4)
	obs.HandleFsEvent(
		t.Context(),
		fsnotify.Event{Name: linkPath, Op: fsnotify.Remove},
		watcher,
		tree,
	)

	requireNoLocalPatch(t, batches, "the skipped symlink's delete")

	result, err := obs.FullScan(t.Context(), tree)
	require.NoError(t, err)
	assert.Nil(t, findRow(result.Rows, "link.txt"), "a safety scan holds no row for an excluded symlink")
	assert.NotNil(t, findRow(result.Rows, "real.txt"))
}

// Validates: R-2.4.6
func TestAddWatchesRecursive_SkipsSymlinksByDefault(t *testing.T) {
	root := t.TempDir()

	// Create a real directory and a symlink to it.
	realDir := filepath.Join(root, "realdir")
	require.NoError(t, os.MkdirAll(realDir, 0o700))

	symlinkDir := filepath.Join(root, "linkdir")
	require.NoError(t, os.Symlink(realDir, symlinkDir))

	// Track which paths were added to the watcher.
	tracker := &addTrackingWatcher{
		events:     make(chan fsnotify.Event, 10),
		errs:       make(chan error, 10),
		addedPaths: make(map[string]bool),
	}

	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)

	err := obs.AddWatchesRecursive(t.Context(), tracker, syncfixture.MustOpenSyncTree(t, root))
	require.NoError(t, err)

	assert.True(t, tracker.addedPaths[root], "expected root to be watched")
	assert.True(t, tracker.addedPaths[realDir], "expected realdir to be watched")
	assert.False(t, tracker.addedPaths[symlinkDir], "symlinked directory should be skipped by default")
}

// Validates: R-2.4.2, R-2.4.5
func TestAddWatchesRecursive_RespectsIncludeAndIgnoreDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, rel := range []string{
		"Projects/src",
		"Projects/build",
		"Archive",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o700))
	}

	tracker := &addTrackingWatcher{
		events:     make(chan fsnotify.Event, 10),
		errs:       make(chan error, 10),
		addedPaths: make(map[string]bool),
	}

	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetFilterConfig(syncdomain.ContentFilterConfig{
		IncludedDirs: []string{"Projects"},
		IgnoredDirs:  []string{"Projects/build"},
	})

	err := obs.AddWatchesRecursive(t.Context(), tracker, syncfixture.MustOpenSyncTree(t, root))
	require.NoError(t, err)

	assert.True(t, tracker.addedPaths[root], "expected root to be watched")
	assert.True(t, tracker.addedPaths[filepath.Join(root, "Projects")], "expected included root to be watched")
	assert.True(t, tracker.addedPaths[filepath.Join(root, "Projects", "src")], "expected included child to be watched")
	assert.False(t, tracker.addedPaths[filepath.Join(root, "Projects", "build")], "ignored dir should not be watched")
	assert.False(t, tracker.addedPaths[filepath.Join(root, "Archive")], "out-of-include dir should not be watched")
}

// addTrackingWatcher implements FsWatcher and records which paths are added.
type addTrackingWatcher struct {
	events     chan fsnotify.Event
	errs       chan error
	addedPaths map[string]bool
}

func (a *addTrackingWatcher) Add(name string) error {
	a.addedPaths[name] = true
	return nil
}

func (a *addTrackingWatcher) Remove(string) error           { return nil }
func (a *addTrackingWatcher) Close() error                  { return nil }
func (a *addTrackingWatcher) Events() <-chan fsnotify.Event { return a.events }
func (a *addTrackingWatcher) Errors() <-chan error          { return a.errs }

// ---------------------------------------------------------------------------
// handleDelete NFC fix tests
// ---------------------------------------------------------------------------

// Validates: R-6.7.20
func TestHandleDelete_UsesOriginalFsPath(t *testing.T) {
	t.Parallel()

	// On macOS HFS+, fsnotify delivers NFD paths. handleDelete must pass the
	// original filesystem path (from fsEvent.Name) to watcher.Remove(), not a
	// reconstructed NFC path. Otherwise watcher.Remove() silently fails and
	// the watch leaks.

	tests := []struct {
		name       string
		fsPath     string // original path from fsnotify (potentially NFD)
		dbRelPath  string // NFC-normalized for baseline lookup
		baseType   syncdomain.ItemType
		wantRemove bool   // expect watcher.Remove() to be called
		wantPath   string // expected path passed to watcher.Remove()
	}{
		{
			name:       "NFC folder path",
			fsPath:     "/sync/r\u00e9sum\u00e9",
			dbRelPath:  "r\u00e9sum\u00e9",
			baseType:   syncdomain.ItemTypeFolder,
			wantRemove: true,
			wantPath:   "/sync/r\u00e9sum\u00e9",
		},
		{
			name:       "NFD folder path (decomposed)",
			fsPath:     "/sync/re\u0301sume\u0301",
			dbRelPath:  "r\u00e9sum\u00e9",
			baseType:   syncdomain.ItemTypeFolder,
			wantRemove: true,
			wantPath:   "/sync/re\u0301sume\u0301", // must use original, NOT reconstructed NFC
		},
		{
			name:       "ASCII folder path",
			fsPath:     "/sync/photos",
			dbRelPath:  "photos",
			baseType:   syncdomain.ItemTypeFolder,
			wantRemove: true,
			wantPath:   "/sync/photos",
		},
		{
			name:       "file (not folder) — no Remove call",
			fsPath:     "/sync/file.txt",
			dbRelPath:  "file.txt",
			baseType:   syncdomain.ItemTypeFile,
			wantRemove: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			watcher := syncfixture.NewRecordingFsWatcher()
			baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
				Path:     tt.dbRelPath,
				DriveID:  driveid.New("d"),
				ItemID:   "item1",
				ItemType: tt.baseType,
			})

			obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
			obs.PendingTimers = make(map[string]syncdomain.SyncTimer)
			obs.HashRequests = make(chan HashRequest, HashRequestBufSize)

			if tt.baseType == syncdomain.ItemTypeFolder {
				registerWatchedForTest(obs, tt.fsPath)
			}
			name := filepath.Base(tt.dbRelPath)

			obs.HandleDelete(t.Context(), watcher, syncfixture.MustOpenSyncTree(t, "/sync"), tt.fsPath, tt.dbRelPath, name)

			removed := watcher.GetRemovedPaths()
			if tt.wantRemove {
				require.Len(t, removed, 1, "expected exactly one Remove call")
				assert.Equal(t, tt.wantPath, removed[0],
					"watcher.Remove() should receive the original filesystem path")
			} else {
				assert.Empty(t, removed, "file deletes should not call watcher.Remove()")
			}
		})
	}
}

// Validates: R-6.7.20
func TestHandleDelete_SendsTheRemovalOfThePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		baseType syncdomain.ItemType
	}{
		{"file deletion", syncdomain.ItemTypeFile},
		{"folder deletion", syncdomain.ItemTypeFolder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			watcher := syncfixture.NewRecordingFsWatcher()
			baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
				Path:     "target",
				DriveID:  driveid.New("d"),
				ItemID:   "item1",
				ItemType: tt.baseType,
			})

			obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
			obs.PendingTimers = make(map[string]syncdomain.SyncTimer)
			obs.HashRequests = make(chan HashRequest, HashRequestBufSize)

			batches := localBatches(obs, 10)
			obs.HandleDelete(t.Context(), watcher, syncfixture.MustOpenSyncTree(t, "/sync"), "/sync/target", "target", "target")

			patches := drainLocalPatches(batches)
			require.Len(t, patches, 1, "one delete sends one patch")
			assert.Equal(t, []string{"target"}, patches[0].DeletedPrefixes)
			assert.Empty(t, patches[0].Rows)
			assert.True(t, patches[0].Dirty)
		})
	}
}

// Validates: R-2.8.16
// TestHandleDelete_CancelsCoalesceTimer verifies that handleDelete cancels
// any pending write coalesce timer for the deleted path (B-107).
func TestHandleDelete_CancelsCoalesceTimer(t *testing.T) {
	t.Parallel()

	watcher := syncfixture.NewRecordingFsWatcher()
	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.PendingTimers = make(map[string]syncdomain.SyncTimer)
	obs.HashRequests = make(chan HashRequest, HashRequestBufSize)

	// Set up a pending timer for "file.txt".
	obs.PendingTimers["file.txt"] = syncdomain.RealClock{}.AfterFunc(time.Hour, func() {})

	obs.HandleDelete(t.Context(), watcher, syncfixture.MustOpenSyncTree(t, "/sync"), "/sync/file.txt", "file.txt", "file.txt")

	assert.Empty(t, obs.PendingTimers, "handleDelete should cancel pending timer")
}

// Validates: R-6.7.20
func TestHandleFsEvent_DeletePassesFsPath(t *testing.T) {
	t.Parallel()

	// This test verifies the full call chain: handleFsEvent → handleDelete
	// passes fsEvent.Name (the original filesystem path) to watcher.Remove().

	syncRoot := t.TempDir()
	dirPath := filepath.Join(syncRoot, "folder")
	require.NoError(t, os.MkdirAll(dirPath, 0o700))

	watcher := syncfixture.NewRecordingFsWatcher()
	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		DriveID:  driveid.New("d"),
		ItemID:   "f1",
		ItemType: syncdomain.ItemTypeFolder,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.PendingTimers = make(map[string]syncdomain.SyncTimer)
	obs.HashRequests = make(chan HashRequest, HashRequestBufSize)

	registerWatchedForTest(obs, dirPath)

	fsEvent := fsnotify.Event{
		Name: dirPath,
		Op:   fsnotify.Remove,
	}
	// The event reports a removal that happened: a delete event for a path
	// that still exists is ignored (D-011).
	require.NoError(t, os.Remove(dirPath))

	obs.HandleFsEvent(t.Context(), fsEvent, watcher, syncfixture.MustOpenSyncTree(t, syncRoot))

	removed := watcher.GetRemovedPaths()
	require.Len(t, removed, 1, "expected Remove call for folder delete")
	assert.Equal(t, dirPath, removed[0],
		"should pass original fsEvent.Name to watcher.Remove()")
}

// Validates: R-2.8.1
func TestHandleFsEvent_SyncRootLifecycleEventDoesNotEmitContentDelete(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	watcher := syncfixture.NewRecordingFsWatcher()
	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "child.txt",
		DriveID:  driveid.New("d"),
		ItemID:   "child-id",
		ItemType: syncdomain.ItemTypeFile,
	})
	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	batches := localBatches(obs, 10)

	obs.HandleFsEvent(t.Context(), fsnotify.Event{
		Name: syncRoot,
		Op:   fsnotify.Remove,
	}, watcher, syncfixture.MustOpenSyncTree(t, syncRoot))

	assert.Empty(t, drainLocalPatches(batches))
	assert.Empty(t, watcher.GetRemovedPaths())
}

// Needed for context usage in test helpers — keep the import.
var _ = context.Background
