package sync

import (
	"context"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// Watch tests — file/directory deletion
// ---------------------------------------------------------------------------

// Validates: R-2.1.2
func TestWatch_DetectsFileDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "doomed.txt", "goodbye")

	baseline := baselineWith(&BaselineEntry{
		Path: "doomed.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	events := make(chan ChangeEvent, 10)
	cancel, done := startLocalWatch(t, obs, dir, events)

	require.NoError(t, os.Remove(filepath.Join(dir, "doomed.txt")))

	var ev ChangeEvent
	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for delete event")
	}

	cancel()
	<-done

	assert.Equal(t, ChangeDelete, ev.Type)
	assert.Equal(t, "doomed.txt", ev.Path)
	assert.True(t, ev.IsDeleted)
}

// TestWatch_DeleteDirectoryRemovesWatch verifies that deleting a watched
// directory emits a ChangeDelete event and the watch continues to function
// normally for other events (B-112).
// Validates: R-2.1.2
func TestWatch_DeleteDirectoryRemovesWatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o700))

	baseline := baselineWith(&BaselineEntry{
		Path: "subdir", DriveID: driveid.New("d"), ItemID: "d1",
		ItemType: ItemTypeFolder,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	events := make(chan ChangeEvent, 10)
	cancel, done := startLocalWatch(t, obs, dir, events)

	// Delete the subdirectory.
	require.NoError(t, os.Remove(subDir))

	var ev ChangeEvent

	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for delete event")
	}

	cancel()
	<-done

	require.Equal(t, ChangeDelete, ev.Type)
	require.Equal(t, "subdir", ev.Path)
	require.Equal(t, ItemTypeFolder, ev.ItemType)
	require.True(t, ev.IsDeleted)
}

// Validates: R-2.4.6
func TestHandleFsEvent_DefaultSymlinkPolicyIgnoresTransientSymlinkDelete(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	writeTestFile(t, syncRoot, "real.txt", "content")

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetFilterConfig(ContentFilterConfig{FollowSymlinks: false})

	watcher := newRecordingFsWatcher()
	events := make(chan ChangeEvent, 4)
	tree := mustOpenSyncTree(t, syncRoot)
	linkPath := filepath.Join(syncRoot, "link.txt")

	require.NoError(t, os.Symlink(filepath.Join(syncRoot, "real.txt"), linkPath))

	obs.HandleFsEvent(
		t.Context(),
		fsnotify.Event{Name: linkPath, Op: fsnotify.Create},
		watcher,
		tree,
		events,
	)

	require.NoError(t, os.Remove(linkPath))

	obs.HandleFsEvent(
		t.Context(),
		fsnotify.Event{Name: linkPath, Op: fsnotify.Remove},
		watcher,
		tree,
		events,
	)

	select {
	case ev := <-events:
		require.Failf(t, "unexpected event for skipped transient symlink", "%+v", ev)
	default:
	}
}

// Validates: R-2.4.6
func TestSkipSymlinkDelete_RemainsIgnoredThroughSafetyScan(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	writeTestFile(t, syncRoot, "real.txt", "content")
	linkPath := filepath.Join(syncRoot, "link.txt")
	require.NoError(t, os.Symlink(filepath.Join(syncRoot, "real.txt"), linkPath))

	baseline := baselineWith(
		&BaselineEntry{
			Path: "real.txt", DriveID: driveid.New("d"), ItemID: "real",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "content"),
		},
		&BaselineEntry{
			Path: "link.txt", DriveID: driveid.New("d"), ItemID: "link",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "content"),
		},
	)

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.SetFilterConfig(ContentFilterConfig{FollowSymlinks: false})

	watcher := newRecordingFsWatcher()
	tree := mustOpenSyncTree(t, syncRoot)
	require.NoError(t, obs.AddWatchesRecursive(t.Context(), watcher, tree))

	require.NoError(t, os.Remove(linkPath))

	events := make(chan ChangeEvent, 4)
	obs.HandleFsEvent(
		t.Context(),
		fsnotify.Event{Name: linkPath, Op: fsnotify.Remove},
		watcher,
		tree,
		events,
	)

	select {
	case ev := <-events:
		require.Failf(t, "unexpected delete event for skipped symlink", "%+v", ev)
	default:
	}

	result, err := obs.FullScan(t.Context(), tree)
	require.NoError(t, err)
	assert.Nil(t, findEvent(result.Events, "link.txt"), "safety scan should not resurrect a delete for an excluded symlink")
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

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	err := obs.AddWatchesRecursive(t.Context(), tracker, mustOpenSyncTree(t, root))
	require.NoError(t, err)

	assert.True(t, tracker.addedPaths[root], "expected root to be watched")
	assert.True(t, tracker.addedPaths[realDir], "expected realdir to be watched")
	assert.False(t, tracker.addedPaths[symlinkDir], "symlinked directory should be skipped by default")
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

// recordingFsWatcher extends mockFsWatcher to record Remove() calls.
// This allows tests to verify which paths were passed to watcher.Remove().
type recordingFsWatcher struct {
	mockFsWatcher
	mu        stdsync.Mutex
	removed   []string
	removeErr error // injectable error for Remove()
}

func newRecordingFsWatcher() *recordingFsWatcher {
	return &recordingFsWatcher{
		mockFsWatcher: mockFsWatcher{
			events: make(chan fsnotify.Event, 10),
			errs:   make(chan error, 10),
		},
	}
}

func (r *recordingFsWatcher) Remove(name string) error {
	r.mu.Lock()
	r.removed = append(r.removed, name)
	err := r.removeErr
	r.mu.Unlock()
	return err
}

func (r *recordingFsWatcher) getRemovedPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]string, len(r.removed))
	copy(result, r.removed)
	return result
}

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
		baseType   ItemType
		wantRemove bool   // expect watcher.Remove() to be called
		wantPath   string // expected path passed to watcher.Remove()
	}{
		{
			name:       "NFC folder path",
			fsPath:     "/sync/r\u00e9sum\u00e9",
			dbRelPath:  "r\u00e9sum\u00e9",
			baseType:   ItemTypeFolder,
			wantRemove: true,
			wantPath:   "/sync/r\u00e9sum\u00e9",
		},
		{
			name:       "NFD folder path (decomposed)",
			fsPath:     "/sync/re\u0301sume\u0301",
			dbRelPath:  "r\u00e9sum\u00e9",
			baseType:   ItemTypeFolder,
			wantRemove: true,
			wantPath:   "/sync/re\u0301sume\u0301", // must use original, NOT reconstructed NFC
		},
		{
			name:       "ASCII folder path",
			fsPath:     "/sync/photos",
			dbRelPath:  "photos",
			baseType:   ItemTypeFolder,
			wantRemove: true,
			wantPath:   "/sync/photos",
		},
		{
			name:       "file (not folder) — no Remove call",
			fsPath:     "/sync/file.txt",
			dbRelPath:  "file.txt",
			baseType:   ItemTypeFile,
			wantRemove: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			watcher := newRecordingFsWatcher()
			baseline := baselineWith(&BaselineEntry{
				Path:     tt.dbRelPath,
				DriveID:  driveid.New("d"),
				ItemID:   "item1",
				ItemType: tt.baseType,
			})

			obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
			obs.PendingTimers = make(map[string]syncTimer)
			obs.HashRequests = make(chan HashRequest, HashRequestBufSize)

			events := make(chan ChangeEvent, 10)
			name := filepath.Base(tt.dbRelPath)

			obs.HandleDelete(t.Context(), watcher, mustOpenSyncTree(t, "/sync"), tt.fsPath, tt.dbRelPath, name, events)

			removed := watcher.getRemovedPaths()
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
func TestHandleDelete_EmitsDeleteEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		baseType ItemType
	}{
		{"file deletion", ItemTypeFile},
		{"folder deletion", ItemTypeFolder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			watcher := newRecordingFsWatcher()
			baseline := baselineWith(&BaselineEntry{
				Path:     "target",
				DriveID:  driveid.New("d"),
				ItemID:   "item1",
				ItemType: tt.baseType,
			})

			obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
			obs.PendingTimers = make(map[string]syncTimer)
			obs.HashRequests = make(chan HashRequest, HashRequestBufSize)

			events := make(chan ChangeEvent, 10)
			obs.HandleDelete(t.Context(), watcher, mustOpenSyncTree(t, "/sync"), "/sync/target", "target", "target", events)

			select {
			case ev := <-events:
				assert.Equal(t, ChangeDelete, ev.Type)
				assert.Equal(t, "target", ev.Path)
				assert.Equal(t, "target", ev.Name)
				assert.Equal(t, tt.baseType, ev.ItemType)
				assert.True(t, ev.IsDeleted)
				assert.Equal(t, SourceLocal, ev.Source)
			default:
				require.Fail(t, "expected a ChangeDelete event")
			}
		})
	}
}

// TestHandleDelete_CancelsCoalesceTimer verifies that handleDelete cancels
// any pending write coalesce timer for the deleted path (B-107).
func TestHandleDelete_CancelsCoalesceTimer(t *testing.T) {
	t.Parallel()

	watcher := newRecordingFsWatcher()
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.PendingTimers = make(map[string]syncTimer)
	obs.HashRequests = make(chan HashRequest, HashRequestBufSize)

	// Set up a pending timer for "file.txt".
	obs.PendingTimers["file.txt"] = realAfterFunc(time.Hour, func() {})

	events := make(chan ChangeEvent, 10)
	obs.HandleDelete(t.Context(), watcher, mustOpenSyncTree(t, "/sync"), "/sync/file.txt", "file.txt", "file.txt", events)

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

	watcher := newRecordingFsWatcher()
	baseline := baselineWith(&BaselineEntry{
		Path:     "folder",
		DriveID:  driveid.New("d"),
		ItemID:   "f1",
		ItemType: ItemTypeFolder,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.PendingTimers = make(map[string]syncTimer)
	obs.HashRequests = make(chan HashRequest, HashRequestBufSize)

	events := make(chan ChangeEvent, 10)

	fsEvent := fsnotify.Event{
		Name: dirPath,
		Op:   fsnotify.Remove,
	}

	obs.HandleFsEvent(t.Context(), fsEvent, watcher, mustOpenSyncTree(t, syncRoot), events)

	removed := watcher.getRemovedPaths()
	require.Len(t, removed, 1, "expected Remove call for folder delete")
	assert.Equal(t, dirPath, removed[0],
		"should pass original fsEvent.Name to watcher.Remove()")
}

// Validates: R-2.8.1
func TestHandleFsEvent_SyncRootLifecycleEventDoesNotEmitContentDelete(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	watcher := newRecordingFsWatcher()
	baseline := baselineWith(&BaselineEntry{
		Path:     "child.txt",
		DriveID:  driveid.New("d"),
		ItemID:   "child-id",
		ItemType: ItemTypeFile,
	})
	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	events := make(chan ChangeEvent, 10)

	obs.HandleFsEvent(t.Context(), fsnotify.Event{
		Name: syncRoot,
		Op:   fsnotify.Remove,
	}, watcher, mustOpenSyncTree(t, syncRoot), events)

	assert.Empty(t, events)
	assert.Empty(t, watcher.getRemovedPaths())
}

// Needed for context usage in test helpers — keep the import.
var _ = context.Background
