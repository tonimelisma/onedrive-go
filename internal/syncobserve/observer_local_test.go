package syncobserve

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// itemTypeFromDirEntry maps a DirEntry to the sync engine's ItemType.
// Test-only helper — not used in production code.
func itemTypeFromDirEntry(d fs.DirEntry) synctypes.ItemType {
	if d.IsDir() {
		return synctypes.ItemTypeFolder
	}

	return synctypes.ItemTypeFile
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// writeTestFile creates a file with the given content under dir/relPath,
// creating parent directories as needed. Returns the absolute path.
func writeTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	fullPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o700), "MkdirAll(%s)", filepath.Dir(fullPath))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600), "WriteFile(%s)", fullPath)

	return fullPath
}

type readySignalWatcher struct {
	inner *fsnotify.Watcher
	ready chan struct{}
	once  sync.Once
}

func newReadySignalWatcher(ready chan struct{}) (*readySignalWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("new watcher: %w", err)
	}

	return &readySignalWatcher{
		inner: watcher,
		ready: ready,
	}, nil
}

func (w *readySignalWatcher) Add(name string) error {
	if err := w.inner.Add(name); err != nil {
		return fmt.Errorf("add watch %q: %w", name, err)
	}

	return nil
}

func (w *readySignalWatcher) Remove(name string) error {
	if err := w.inner.Remove(name); err != nil {
		return fmt.Errorf("remove watch %q: %w", name, err)
	}

	return nil
}

func (w *readySignalWatcher) Close() error {
	if err := w.inner.Close(); err != nil {
		return fmt.Errorf("close watcher: %w", err)
	}

	return nil
}

func (w *readySignalWatcher) Events() <-chan fsnotify.Event {
	w.once.Do(func() {
		close(w.ready)
	})

	return w.inner.Events
}

func (w *readySignalWatcher) Errors() <-chan error {
	return w.inner.Errors
}

func startLocalWatch(t *testing.T, obs *LocalObserver, dir string, events chan<- synctypes.ChangeEvent) (context.CancelFunc, <-chan error) {
	t.Helper()

	ready := make(chan struct{})
	obs.SetWatcherFactory(func() (FsWatcher, error) {
		return newReadySignalWatcher(ready)
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "watch exited before becoming ready")
	case <-ready:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for watch loop readiness")
	}

	return cancel, done
}

// hashContent computes the QuickXorHash of a string, returning the
// base64-encoded digest. Matches the output of driveops.ComputeQuickXorHash
// for the same content written to a file.
func hashContent(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	_, err := h.Write([]byte(content))
	require.NoError(t, err, "hash.Write")

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// findEvent returns the first ChangeEvent with the given path, or nil.
func findEvent(events []synctypes.ChangeEvent, path string) *synctypes.ChangeEvent {
	for i := range events {
		if events[i].Path == path {
			return &events[i]
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// FullScan tests
// ---------------------------------------------------------------------------

func TestFullScan_NewFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "hello.txt", "hello world")
	writeTestFile(t, dir, "data.csv", "a,b,c")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 2)

	ev := findEvent(result.Events, "hello.txt")
	require.NotNil(t, ev, "hello.txt event not found")

	assert.Equal(t, synctypes.SourceLocal, ev.Source)
	assert.Equal(t, synctypes.ChangeCreate, ev.Type)
	assert.Equal(t, "hello.txt", ev.Name)
	assert.Equal(t, hashContent(t, "hello world"), ev.Hash)
	assert.Equal(t, int64(len("hello world")), ev.Size)
	assert.NotZero(t, ev.Mtime, "Mtime should be non-zero")
	assert.Equal(t, synctypes.ItemTypeFile, ev.ItemType)
}

func TestFullScan_NewFolder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "photos"), 0o700), "Mkdir")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 1)

	ev := result.Events[0]
	assert.Equal(t, synctypes.ChangeCreate, ev.Type)
	assert.Equal(t, synctypes.ItemTypeFolder, ev.ItemType)
	assert.Empty(t, ev.Hash, "folders have no hash")
	assert.Equal(t, "photos", ev.Path)
}

func TestFullScan_ModifiedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "doc.txt", "updated content")

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "doc.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, "original content"),
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 1)

	ev := result.Events[0]
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, hashContent(t, "updated content"), ev.Hash)
	assert.Equal(t, synctypes.SourceLocal, ev.Source)
}

func TestFullScan_UnchangedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "same content"
	writeTestFile(t, dir, "stable.txt", content)

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "stable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, content),
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	assert.Empty(t, result.Events, "file unchanged")
}

func TestFullScan_DeletedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// File is in baseline but NOT on disk.

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "gone.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: "some-hash",
		LocalSize: 42, LocalSizeKnown: true,
		LocalMtime: 1234567890,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 1)

	ev := result.Events[0]
	assert.Equal(t, synctypes.ChangeDelete, ev.Type)
	assert.True(t, ev.IsDeleted)
	assert.Equal(t, "gone.txt", ev.Path)
	assert.Equal(t, "gone.txt", ev.Name)

	// Size and Mtime should be populated from baseline.
	assert.Equal(t, int64(42), ev.Size)
	assert.Equal(t, int64(1234567890), ev.Mtime)
}

func TestFullScan_DeletedFolder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "old-folder", DriveID: driveid.New("d"), ItemID: "f1",
		ItemType: synctypes.ItemTypeFolder,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 1)

	ev := result.Events[0]
	assert.Equal(t, synctypes.ChangeDelete, ev.Type)
	assert.Equal(t, synctypes.ItemTypeFolder, ev.ItemType)
}

func TestFullScan_MtimeChangeNoContentChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "unchanged"
	writeTestFile(t, dir, "stable.txt", content)

	// Baseline has a different mtime but the same hash.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "stable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, content),
		LocalMtime: 999, // intentionally different from actual file mtime
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	assert.Empty(t, result.Events, "mtime change only, hash matches")
}

func TestFullScan_MtimeSizeFastPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "fast path content"
	writeTestFile(t, dir, "cached.txt", content)

	// Set mtime to 1 hour ago so it's well outside the racily-clean window.
	oldTime := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "cached.txt"), oldTime, oldTime), "Chtimes")

	info, err := os.Stat(filepath.Join(dir, "cached.txt"))
	require.NoError(t, err, "Stat")

	// Baseline matches file's actual mtime, size, and hash — fast path should skip hashing.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, content),
		LocalSize: info.Size(), LocalSizeKnown: true,
		LocalMtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	assert.Empty(t, result.Events, "fast path should skip unchanged file")
}

// Validates: R-6.7.15
func TestFullScan_SameSecondSubsecondDifferenceSkipsFalseModify(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "whole-second-stable"
	writeTestFile(t, dir, "stable.txt", content)

	fileTime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second).Add(200 * time.Millisecond)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "stable.txt"), fileTime, fileTime), "Chtimes")

	info, err := os.Stat(filepath.Join(dir, "stable.txt"))
	require.NoError(t, err, "Stat")

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "stable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, content),
		LocalSize: info.Size(), LocalSizeKnown: true,
		LocalMtime: fileTime.Add(700 * time.Millisecond).UnixNano(),
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.HashFunc = func(string) (string, error) {
		return "", errors.New("hash function should not run for same-second timestamp drift")
	}

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	assert.Empty(t, result.Events, "same-second fractional drift should not emit ChangeModify")
	assert.Empty(t, result.Skipped, "same-second fractional drift should not force a fallback hash path")
}

func TestFullScan_RacilyCleanForcesHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// File content differs from baseline hash, but mtime+size will match.
	// Since the file was just created, mtime is within 1 second of scan start,
	// so the racily-clean guard should force a hash check and detect the change.
	actualContent := "actual_xx"
	baselineContent := "baseline_"
	writeTestFile(t, dir, "racy.txt", actualContent)

	info, err := os.Stat(filepath.Join(dir, "racy.txt"))
	require.NoError(t, err, "Stat")

	// Baseline has same mtime and size but different hash.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "racy.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, baselineContent),
		LocalSize: info.Size(), LocalSizeKnown: true,
		LocalMtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	// Racily-clean guard should force hash, detect the mismatch -> ChangeModify.
	require.Len(t, result.Events, 1, "racily clean should force hash")

	ev := result.Events[0]
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, hashContent(t, actualContent), ev.Hash)
}

func TestFullScan_SizeChangeForcesHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "new longer content"
	writeTestFile(t, dir, "grown.txt", content)

	// Set mtime to 1 hour ago.
	oldTime := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "grown.txt"), oldTime, oldTime), "Chtimes")

	info, err := os.Stat(filepath.Join(dir, "grown.txt"))
	require.NoError(t, err, "Stat")

	// Baseline has same mtime but different size — should force hash.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "grown.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, "short"),
		LocalSize: 5, LocalSizeKnown: true,
		LocalMtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 1, "size change should force hash")
	assert.Equal(t, synctypes.ChangeModify, result.Events[0].Type)
}

func TestFullScan_NosyncGuard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, ".nosync", "")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	_, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))

	assert.ErrorIs(t, err, synctypes.ErrNosyncGuard)
}

func TestFullScan_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	assert.Empty(t, result.Events)
}

// Validates: R-2.4.6
func TestFullScan_SymlinkFollowedByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "real.txt", "content")

	require.NoError(t, os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")), "Symlink")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 2, "default symlink policy should follow symlinks")
	assert.NotNil(t, findEvent(result.Events, "real.txt"))

	linkEvent := findEvent(result.Events, "link.txt")
	require.NotNil(t, linkEvent)
	assert.Equal(t, synctypes.ItemTypeFile, linkEvent.ItemType)
	assert.Equal(t, hashContent(t, "content"), linkEvent.Hash)
}

// Validates: R-2.4.6
func TestFullScan_SymlinkDirectoryFollowedByDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "real/nested.txt", "payload")

	require.NoError(t, os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "alias")), "Symlink")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	assert.NotNil(t, findEvent(result.Events, "real"))
	assert.NotNil(t, findEvent(result.Events, "real/nested.txt"))
	assert.NotNil(t, findEvent(result.Events, "alias"))
	assert.NotNil(t, findEvent(result.Events, "alias/nested.txt"))
}

// Validates: R-2.4.6
func TestFullScan_SymlinkDirectoryCycleStopsAtAliasBoundary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "real.txt", "content")

	require.NoError(t, os.Symlink(dir, filepath.Join(dir, "loop")), "Symlink")

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(ctx, mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	assert.NotNil(t, findEvent(result.Events, "real.txt"))
	assert.NotNil(t, findEvent(result.Events, "loop"))
	assert.Nil(t, findEvent(result.Events, "loop/real.txt"))
}

// Validates: R-2.11.2, R-2.11.3, R-2.11.5
func TestFullScan_InvalidName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "CON", "reserved")
	writeTestFile(t, dir, "~$Budget.xlsx", "reserved pattern")
	writeTestFile(t, dir, "valid.txt", "ok")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	// OneDrive-invalid names should be skipped; only valid.txt produces an event.
	require.Len(t, result.Events, 1)
	assert.Equal(t, "valid.txt", result.Events[0].Path)

	require.Len(t, result.Skipped, 2, "invalid OneDrive names should be reported in Skipped")

	skipped := make(map[string]synctypes.SkippedItem, len(result.Skipped))
	for i := range result.Skipped {
		skipped[result.Skipped[i].Path] = result.Skipped[i]
	}

	for _, path := range []string{"CON", "~$Budget.xlsx"} {
		item, ok := skipped[path]
		require.True(t, ok, "%s should be in Skipped", path)
		assert.Equal(t, synctypes.IssueInvalidFilename, item.Reason)
		assert.NotEmpty(t, item.Detail)
	}
}

// Validates: R-2.11.3, R-2.11.5
func TestFullScan_SharePointRootFormsProducesSkippedItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "forms", "reserved root name")
	writeTestFile(t, dir, "docs/forms", "allowed below root")
	writeTestFile(t, dir, "valid.txt", "ok")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetObservationRules(synctypes.LocalObservationRules{RejectSharePointRootForms: true})

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	assert.NotNil(t, findEvent(result.Events, "valid.txt"))
	assert.NotNil(t, findEvent(result.Events, "docs/forms"))
	assert.Nil(t, findEvent(result.Events, "forms"))

	require.Len(t, result.Skipped, 1, "SharePoint root-level forms should be reported as actionable")
	assert.Equal(t, "forms", result.Skipped[0].Path)
	assert.Equal(t, synctypes.IssueInvalidFilename, result.Skipped[0].Reason)
	assert.NotEmpty(t, result.Skipped[0].Detail)
}

// Validates: R-2.4
func TestFullScan_AlwaysExcluded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "download.partial", "incomplete")
	writeTestFile(t, dir, "temp.tmp", "temporary")
	writeTestFile(t, dir, "~backup", "old")
	writeTestFile(t, dir, "legit.txt", "keep me")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	// Only legit.txt should produce an event.
	require.Len(t, result.Events, 1)
	assert.Equal(t, "legit.txt", result.Events[0].Path)

	// Always-excluded items are internal, not user-actionable — Skipped should be empty.
	assert.Empty(t, result.Skipped, "always-excluded items should not appear in Skipped")
}

func TestFullScan_ContextCanceled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "content")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	_, err := obs.FullScan(ctx, mustOpenSyncTree(t, dir))

	require.Error(t, err, "expected error")
	assert.ErrorIs(t, err, context.Canceled)
}

// Validates: R-2.13.1
func TestFullScan_NFCNormalization(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// NFD decomposed: e + combining acute accent (U+0301).
	nfdName := shortcutTestNFDResume
	// NFC composed: precomposed characters.
	nfcName := shortcutTestNFCResume

	writeTestFile(t, dir, nfdName, "resume content")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 1)
	assert.Equal(t, nfcName, result.Events[0].Path, "NFC-normalized")
	assert.Equal(t, nfcName, result.Events[0].Name, "NFC-normalized")
}

func TestFullScan_NestedDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "a/b/deep.txt", "deep content")
	writeTestFile(t, dir, "top.txt", "top content")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	// Expect: folder "a", folder "a/b", file "a/b/deep.txt", file "top.txt".
	require.Len(t, result.Events, 4)

	deepEv := findEvent(result.Events, "a/b/deep.txt")
	require.NotNil(t, deepEv, "a/b/deep.txt event not found")
	assert.Equal(t, synctypes.ChangeCreate, deepEv.Type)

	// Verify folder events exist with correct paths.
	aEv := findEvent(result.Events, "a")
	require.NotNil(t, aEv, "folder 'a' event not found")
	assert.Equal(t, synctypes.ItemTypeFolder, aEv.ItemType)

	abEv := findEvent(result.Events, "a/b")
	require.NotNil(t, abEv, "folder 'a/b' event not found")
}

func TestFullScan_EmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "empty.txt", "")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	require.Len(t, result.Events, 1)

	ev := result.Events[0]
	assert.Equal(t, synctypes.ChangeCreate, ev.Type)
	assert.NotEmpty(t, ev.Hash, "want non-empty hash for empty file")
	assert.Equal(t, hashContent(t, ""), ev.Hash)
	assert.Equal(t, int64(0), ev.Size)
}

func TestFullScan_MixedChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// New file (not in baseline).
	writeTestFile(t, dir, "new.txt", "new content")

	// Modified file (baseline has old hash).
	writeTestFile(t, dir, "modified.txt", "updated content")

	// Unchanged file (baseline has matching hash).
	writeTestFile(t, dir, "unchanged.txt", "same content")

	// Deleted file: in baseline but NOT on disk (don't create).

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{
			Path: "modified.txt", DriveID: driveid.New("d"), ItemID: "i1",
			ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, "original content"),
		},
		&synctypes.BaselineEntry{
			Path: "unchanged.txt", DriveID: driveid.New("d"), ItemID: "i2",
			ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, "same content"),
		},
		&synctypes.BaselineEntry{
			Path: "deleted.txt", DriveID: driveid.New("d"), ItemID: "i3",
			ItemType: synctypes.ItemTypeFile, LocalHash: "some-hash",
		},
	)

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	// 3 events: new, modified, deleted. Unchanged produces no event.
	require.Len(t, result.Events, 3)

	newEv := findEvent(result.Events, "new.txt")
	require.NotNil(t, newEv, "new.txt event not found")
	assert.Equal(t, synctypes.ChangeCreate, newEv.Type)

	modEv := findEvent(result.Events, "modified.txt")
	require.NotNil(t, modEv, "modified.txt event not found")
	assert.Equal(t, synctypes.ChangeModify, modEv.Type)

	delEv := findEvent(result.Events, "deleted.txt")
	require.NotNil(t, delEv, "deleted.txt event not found")
	assert.Equal(t, synctypes.ChangeDelete, delEv.Type)
}

// ---------------------------------------------------------------------------
// Unit tests for helper functions
// ---------------------------------------------------------------------------

func TestIsAlwaysExcluded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Suffix-based exclusions.
		{"partial file", "download.partial", true},
		{"tmp file", "temp.tmp", true},
		{"swap file", "file.swp", true},
		{"crdownload", "file.crdownload", true},
		{"db file", "data.db", false},
		{"db-wal file", "data.db-wal", false},
		{"db-shm file", "data.db-shm", false},

		// Case insensitive.
		{"tmp uppercase", "FILE.TMP", true},
		{"partial mixed case", "Download.Partial", true},
		{"db uppercase", "DATA.DB", false},

		// Prefix-based exclusions.
		{"tilde prefix", "~backup", true},
		{"dot-tilde prefix", ".~lock.file", true},
		{"tilde-dollar", "~$Budget.xlsx", false},

		// Not excluded.
		{"normal txt", "hello.txt", false},
		{"go file", "main.go", false},
		{"dotfile", ".gitignore", false},
		{"csv file", "data.csv", false},
		{"partial in middle", "my.partial.bak", false},
		{"db in middle", "data.db.backup", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := IsAlwaysExcluded(tt.in)
			assert.Equal(t, tt.want, got, "IsAlwaysExcluded(%q)", tt.in)
		})
	}
}

// TestIsAlwaysExcluded_DbFilesNotExcluded validates that .db, .db-wal, and
// .db-shm files are NOT excluded (B-308). The sync engine's state DB lives
// outside the sync directory, so these suffixes only cause false positives
// for legitimate user files.
func TestIsAlwaysExcluded_DbFilesNotExcluded(t *testing.T) {
	t.Parallel()

	// .db files should NOT be excluded.
	assert.False(t, IsAlwaysExcluded("data.db"), ".db should not be excluded")
	assert.False(t, IsAlwaysExcluded("data.db-wal"), ".db-wal should not be excluded")
	assert.False(t, IsAlwaysExcluded("data.db-shm"), ".db-shm should not be excluded")
	assert.False(t, IsAlwaysExcluded("DATA.DB"), "case-insensitive .db should not be excluded")

	// These should still be excluded.
	assert.True(t, IsAlwaysExcluded("file.partial"), ".partial should still be excluded")
	assert.True(t, IsAlwaysExcluded("file.tmp"), ".tmp should still be excluded")
	assert.True(t, IsAlwaysExcluded("file.swp"), ".swp should still be excluded")
	assert.True(t, IsAlwaysExcluded("file.crdownload"), ".crdownload should still be excluded")
}

func TestValidateOneDriveName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		wantValid bool
	}{
		// Valid names.
		{"simple file", "hello.txt", true},
		{"with spaces", "my file.txt", true},
		{"with dots", "file.tar.gz", true},
		{"unicode", "caf\u00e9.txt", true},
		{"max length", strings.Repeat("a", 255), true},
		{"numbers", "12345", true},

		// Empty.
		{"empty", "", false},

		// Trailing dot or space.
		{"trailing dot", "file.", false},
		{"trailing space", "file ", false},

		// Leading space.
		{"leading space", " file", false},

		// Too long.
		{"too long", strings.Repeat("a", 256), false},

		// Reserved device names.
		{"CON upper", "CON", false},
		{"con lower", "con", false},
		{"PRN", "PRN", false},
		{"prn", "prn", false},
		{"AUX", "AUX", false},
		{"NUL", "NUL", false},
		{"COM0", "COM0", false},
		{"COM9", "COM9", false},
		{"com5", "com5", false},
		{"LPT0", "LPT0", false},
		{"LPT9", "LPT9", false},
		{"lpt3", "lpt3", false},
		{"COM10 is valid", "COM10", true},
		{"CONX is valid", "CONX", true},
		{"COMMA is valid", "comma", true},

		// .lock extension.
		{"lock file", "file.lock", false},
		{"LOCK upper", "FILE.LOCK", false},

		// desktop.ini.
		{"desktop.ini", "desktop.ini", false},
		{"Desktop.INI", "Desktop.INI", false},

		// ~$ prefix.
		{"tilde-dollar", "~$Budget.xlsx", false},

		// _vti_ substring.
		{"vti substring", "test_vti_file", false},
		{"vti prefix", "_vti_history", false},

		// Invalid characters.
		{"double quote", "file\"name", false},
		{"asterisk", "file*name", false},
		{"colon", "file:name", false},
		{"less than", "file<name", false},
		{"greater than", "file>name", false},
		{"question mark", "file?name", false},
		{"forward slash", "file/name", false},
		{"backslash", "file\\name", false},
		{"pipe", "file|name", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reason, _ := ValidateOneDriveName(tt.in)
			if tt.wantValid {
				assert.Empty(t, reason, "ValidateOneDriveName(%q) should return empty reason", tt.in)
			} else {
				assert.NotEmpty(t, reason, "ValidateOneDriveName(%q) should return non-empty reason", tt.in)
			}
		})
	}
}

func TestItemTypeFromDirEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "x")

	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o700), "Mkdir")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "ReadDir")

	for _, e := range entries {
		got := itemTypeFromDirEntry(e)

		switch {
		case e.IsDir():
			assert.Equal(t, synctypes.ItemTypeFolder, got, "%s should be ItemTypeFolder", e.Name())
		default:
			assert.Equal(t, synctypes.ItemTypeFile, got, "%s should be ItemTypeFile", e.Name())
		}
	}
}

func TestSkipEntry_Dir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o700), "Mkdir")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "ReadDir")

	for _, e := range entries {
		if e.IsDir() {
			got := SkipEntry(e)
			assert.ErrorIs(t, got, filepath.SkipDir)
		}
	}
}

func TestSkipEntry_File(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "x")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "ReadDir")

	for _, e := range entries {
		if !e.IsDir() {
			got := SkipEntry(e)
			assert.NoError(t, got)
		}
	}
}

func TestSkipEntry_Nil(t *testing.T) {
	t.Parallel()

	got := SkipEntry(nil)
	assert.NoError(t, got)
}

func TestFullScan_NosyncGuardDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// .nosync as a directory should also trigger the guard.
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".nosync"), 0o700), "Mkdir")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	_, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))

	assert.ErrorIs(t, err, synctypes.ErrNosyncGuard)
}

// Validates: R-2.4
func TestFullScan_ExcludedDirSkipsSubtree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create an always-excluded directory and a file inside it.
	writeTestFile(t, dir, "~excluded/inner.txt", "hidden")
	writeTestFile(t, dir, "visible.txt", "shown")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	// Only visible.txt should appear; ~excluded dir and its contents are skipped.
	require.Len(t, result.Events, 1)
	assert.Equal(t, "visible.txt", result.Events[0].Path)
}

func TestFullScan_InvalidNameDirSkipsSubtree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Directory with a trailing dot (invalid OneDrive name).
	invalidDir := filepath.Join(dir, "bad.")

	// On some filesystems, trailing dots are stripped. Create and verify.
	if err := os.Mkdir(invalidDir, 0o700); err != nil {
		t.Skipf("filesystem does not support trailing dot in directory name: %v", err)
	}

	// Verify the directory was actually created with the trailing dot.
	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr, "ReadDir")

	hasBadDir := false

	for _, e := range entries {
		if e.Name() == "bad." {
			hasBadDir = true
		}
	}

	if !hasBadDir {
		t.Skip("filesystem stripped trailing dot from directory name")
	}

	writeTestFile(t, dir, "bad./child.txt", "child")
	writeTestFile(t, dir, "good.txt", "good")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	// Only good.txt should appear; bad. dir and its contents are skipped.
	assert.Nil(t, findEvent(result.Events, "bad./child.txt"), "child inside invalid-name dir should not produce an event")
	assert.NotNil(t, findEvent(result.Events, "good.txt"), "good.txt event not found")
}

func TestFullScan_PermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	t.Parallel()

	syncRoot := t.TempDir()

	// Create readable dir with a file.
	readableDir := filepath.Join(syncRoot, "readable")
	require.NoError(t, os.MkdirAll(readableDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(readableDir, "file.txt"), []byte("ok"), 0o600))

	// Create unreadable dir.
	unreadableDir := filepath.Join(syncRoot, "unreadable")
	require.NoError(t, os.MkdirAll(unreadableDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(unreadableDir, "hidden.txt"), []byte("secret"), 0o600))
	setTestDirPermissions(t, unreadableDir, 0o000)
	t.Cleanup(func() { setTestDirPermissions(t, unreadableDir, 0o700) })

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// Should see events for readable dir + file, but not unreadable contents.
	paths := eventPaths(result.Events)
	assert.True(t, containsPath(paths, "readable/file.txt"), "expected event for readable/file.txt")
	assert.False(t, containsPath(paths, "unreadable/hidden.txt"), "should not have event for unreadable/hidden.txt")
}

// eventPaths extracts paths from a slice of ChangeEvents.
func eventPaths(events []synctypes.ChangeEvent) []string {
	paths := make([]string, len(events))
	for i := range events {
		paths[i] = events[i].Path
	}

	return paths
}

// containsPath checks if a path exists in the paths slice.
func containsPath(paths []string, target string) bool {
	for _, p := range paths {
		if p == target {
			return true
		}
	}

	return false
}

// mockDirEntry implements fs.DirEntry for unit tests of helper functions.
type mockDirEntry struct {
	name  string
	isDir bool
}

func (m mockDirEntry) Name() string { return m.name }
func (m mockDirEntry) IsDir() bool  { return m.isDir }
func (m mockDirEntry) Info() (fs.FileInfo, error) {
	return mockFileInfo{name: m.name, dir: m.isDir}, nil
}

func (m mockDirEntry) Type() fs.FileMode {
	if m.isDir {
		return fs.ModeDir
	}

	return 0
}

type mockFileInfo struct {
	name string
	dir  bool
}

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return 0 }
func (m mockFileInfo) Mode() fs.FileMode  { return m.mode() }
func (m mockFileInfo) ModTime() time.Time { return time.Time{} }
func (m mockFileInfo) IsDir() bool        { return m.dir }
func (m mockFileInfo) Sys() any           { return nil }

func (m mockFileInfo) mode() fs.FileMode {
	if m.dir {
		return fs.ModeDir
	}

	return 0
}

func TestItemTypeFromDirEntry_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		isDir bool
		want  synctypes.ItemType
	}{
		{"file", false, synctypes.ItemTypeFile},
		{"directory", true, synctypes.ItemTypeFolder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := mockDirEntry{name: "test", isDir: tt.isDir}
			got := itemTypeFromDirEntry(d)

			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// Hash failure creates with empty hash (B-102) -- FullScan variant
// ---------------------------------------------------------------------------

// TestFullScan_HashFailureStillEmitsCreate verifies that FullScan emits a
// ChangeCreate event with empty hash when hash computation fails (B-102).
func TestFullScan_HashFailureStillEmitsCreate(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root can read all files)")
	}

	t.Parallel()

	dir := t.TempDir()
	path := writeTestFile(t, dir, "unreadable.txt", "secret")
	require.NoError(t, os.Chmod(path, 0o000))

	t.Cleanup(func() {
		assert.NoError(t, os.Chmod(path, 0o600))
	})

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err)

	ev := findEvent(result.Events, "unreadable.txt")
	require.NotNil(t, ev, "unreadable file should still produce an event")
	require.Equal(t, synctypes.ChangeCreate, ev.Type)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
	require.Equal(t, synctypes.SourceLocal, ev.Source)
}

// ---------------------------------------------------------------------------
// Hash failure modifies with empty hash (B-102) -- FullScan variant
// ---------------------------------------------------------------------------

// TestFullScan_HashFailureModifyStillEmitsEvent verifies that FullScan emits a
// ChangeModify event with empty hash when hash computation fails for a modified
// file (B-102). Before the fix, hash failures silently dropped modify events.
func TestFullScan_HashFailureModifyStillEmitsEvent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root can read all files)")
	}

	t.Parallel()

	dir := t.TempDir()
	path := writeTestFile(t, dir, "unreadable.txt", "modified content")

	// Baseline has a different hash so the slow path (hash check) kicks in.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "unreadable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: synctypes.ItemTypeFile, LocalHash: hashContent(t, "original content"),
	})

	// Make file unreadable -- stat still succeeds but hash computation fails.
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(path, 0o600)) })

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err)

	ev := findEvent(result.Events, "unreadable.txt")
	require.NotNil(t, ev, "unreadable modified file should still produce an event")
	require.Equal(t, synctypes.ChangeModify, ev.Type)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
	require.Equal(t, synctypes.SourceLocal, ev.Source)
}

// ---------------------------------------------------------------------------
// Sync root deletion detection (B-113)
// ---------------------------------------------------------------------------

func TestSyncRootExists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  bool
	}{
		{
			name: "existing directory",
			setup: func(t *testing.T) string {
				t.Helper()
				return t.TempDir()
			},
			want: true,
		},
		{
			name: "nonexistent path",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			want: false,
		},
		{
			name: "file not directory",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				p := filepath.Join(dir, "afile")
				require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
				return p
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := tt.setup(t)
			got := SyncRootExists(p)

			assert.Equal(t, tt.want, got, "SyncRootExists(%q)", p)
		})
	}
}

// ---------------------------------------------------------------------------
// Path too long (observation filter)
// ---------------------------------------------------------------------------

func TestFullScan_PathTooLong(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Build a nested directory structure whose relative path exceeds 400 characters.
	// Each segment is ~50 chars; 9 segments = 450+ chars including separators.
	segment := strings.Repeat("a", 50)
	parts := make([]string, 9)
	for i := range parts {
		parts[i] = segment
	}

	deepDir := filepath.Join(parts[:len(parts)-1]...)
	deepFile := filepath.Join(deepDir, parts[len(parts)-1]+".txt")
	writeTestFile(t, dir, deepFile, "deep content")

	// Also write a normal file to verify it still appears.
	writeTestFile(t, dir, "normal.txt", "ok")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan")

	// normal.txt should be in Events.
	assert.NotNil(t, findEvent(result.Events, "normal.txt"), "normal.txt event not found")

	// The too-long path should NOT appear in Events.
	for _, ev := range result.Events {
		assert.Less(t, len(ev.Path), MaxOneDrivePathLength+1, "event with too-long path should not be in Events: %s", ev.Path)
	}

	// The too-long path should appear in Skipped with IssuePathTooLong.
	var foundSkip *synctypes.SkippedItem
	for i := range result.Skipped {
		if result.Skipped[i].Reason == synctypes.IssuePathTooLong {
			foundSkip = &result.Skipped[i]
			break
		}
	}

	require.NotNil(t, foundSkip, "expected a SkippedItem with IssuePathTooLong")
	assert.Greater(t, len(foundSkip.Path), MaxOneDrivePathLength, "skipped path should exceed limit")
	assert.NotEmpty(t, foundSkip.Detail)
}

// ---------------------------------------------------------------------------
// ShouldObserve unit tests
// ---------------------------------------------------------------------------

// Validates: R-2.11.1, R-2.11.2, R-2.11.3, R-2.11.4
func TestShouldObserve_AllCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fileName   string
		path       string
		filter     synctypes.LocalFilterConfig
		rules      synctypes.LocalObservationRules
		wantNil    bool   // expect nil (observe)
		wantReason string // expected Reason if non-nil; "" for internal exclusions
	}{
		{
			name:     "normal file",
			fileName: "hello.txt",
			path:     "hello.txt",
			wantNil:  true,
		},
		{
			name:     "always-excluded file (.tmp)",
			fileName: "temp.tmp",
			path:     "temp.tmp",
			wantNil:  false,
			// internal exclusion → Reason==""
		},
		{
			name:       "invalid name (CON)",
			fileName:   "CON",
			path:       "CON",
			wantReason: synctypes.IssueInvalidFilename,
		},
		{
			name:       "invalid char",
			fileName:   "bad?.txt",
			path:       "bad?.txt",
			wantReason: synctypes.IssueInvalidFilename,
		},
		{
			name:       "reserved pattern (~$)",
			fileName:   "~$Budget.xlsx",
			path:       "~$Budget.xlsx",
			wantReason: synctypes.IssueInvalidFilename,
		},
		{
			name:       "reserved pattern (_vti_)",
			fileName:   "_vti_history",
			path:       "_vti_history",
			wantReason: synctypes.IssueInvalidFilename,
		},
		{
			name:       "trailing dot",
			fileName:   "bad.",
			path:       "bad.",
			wantReason: synctypes.IssueInvalidFilename,
		},
		{
			name:       "sharepoint root forms",
			fileName:   "forms",
			path:       "forms",
			rules:      synctypes.LocalObservationRules{RejectSharePointRootForms: true},
			wantReason: synctypes.IssueInvalidFilename,
		},
		{
			name:     "sharepoint nested forms allowed",
			fileName: "forms",
			path:     "team/forms",
			rules:    synctypes.LocalObservationRules{RejectSharePointRootForms: true},
			wantNil:  true,
		},
		{
			name:       "path too long (>400 chars)",
			fileName:   "file.txt",
			path:       strings.Repeat("a", 401),
			wantReason: synctypes.IssuePathTooLong,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			skip := shouldObserveWithFilter(tt.fileName, tt.path, observedKindUnknown, tt.filter, tt.rules)

			if tt.wantNil {
				assert.Nil(t, skip, "ShouldObserve(%q, %q) should return nil", tt.fileName, tt.path)
			} else {
				require.NotNil(t, skip, "ShouldObserve(%q, %q) should return non-nil", tt.fileName, tt.path)
				assert.Equal(t, tt.wantReason, skip.Reason)

				if tt.wantReason != "" {
					assert.Equal(t, tt.path, skip.Path)
					assert.NotEmpty(t, skip.Detail)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateOneDriveName unit tests
// ---------------------------------------------------------------------------

func TestValidateOneDriveName_AllCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantReason string
		wantDetail bool // true if we expect a non-empty detail string
	}{
		{
			name:       "valid name",
			input:      "hello.txt",
			wantReason: "",
			wantDetail: false,
		},
		{
			name:       "empty name",
			input:      "",
			wantReason: synctypes.IssueInvalidFilename,
			wantDetail: true,
		},
		{
			name:       "trailing dot",
			input:      "file.",
			wantReason: synctypes.IssueInvalidFilename,
			wantDetail: true,
		},
		{
			name:       "reserved name (CON)",
			input:      "CON",
			wantReason: synctypes.IssueInvalidFilename,
			wantDetail: true,
		},
		{
			name:       "invalid chars (asterisk)",
			input:      "file*name",
			wantReason: synctypes.IssueInvalidFilename,
			wantDetail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reason, detail := ValidateOneDriveName(tt.input)
			assert.Equal(t, tt.wantReason, reason, "ValidateOneDriveName(%q) reason", tt.input)

			if tt.wantDetail {
				assert.NotEmpty(t, detail, "ValidateOneDriveName(%q) should return a detail string", tt.input)
			} else {
				assert.Empty(t, detail, "ValidateOneDriveName(%q) should return empty detail", tt.input)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Scanner hash phase panic recovery tests
// ---------------------------------------------------------------------------

// Validates: R-6.7.5
func TestHashPhase_PanicRecovery(t *testing.T) {
	t.Parallel()

	// Inject a hash function that panics for a specific path. Other files
	// should still be hashed successfully, and the panicking file should
	// become a SkippedItem with IssueHashPanic.

	tests := []struct {
		name       string
		panicPath  string // which file triggers the panic
		files      []string
		wantEvents int // events from non-panicking files
	}{
		{
			name:       "panic on first job",
			panicPath:  "panic.txt",
			files:      []string{"panic.txt", "ok1.txt", "ok2.txt"},
			wantEvents: 2,
		},
		{
			name:       "panic on middle job",
			panicPath:  "ok1.txt",
			files:      []string{"a.txt", "ok1.txt", "b.txt"},
			wantEvents: 2,
		},
		{
			name:       "panic on last job",
			panicPath:  "last.txt",
			files:      []string{"first.txt", "second.txt", "last.txt"},
			wantEvents: 2,
		},
		{
			name:       "single file panics",
			panicPath:  "only.txt",
			files:      []string{"only.txt"},
			wantEvents: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for _, f := range tt.files {
				writeTestFile(t, dir, f, "content of "+f)
			}

			obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 1)
			// Inject a hash function that panics for the target path.
			obs.HashFunc = func(path string) (string, error) {
				if filepath.Base(path) == tt.panicPath {
					panic("simulated hash panic for " + tt.panicPath)
				}
				// Use real hash for other files.
				return hashContent(t, "content of "+filepath.Base(path)), nil
			}

			result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
			require.NoError(t, err, "FullScan should not fail due to panic in hash worker")

			// Non-panicking files should still produce events.
			assert.Len(t, result.Events, tt.wantEvents,
				"non-panicking files should still produce events")

			// The panicking file should appear in Skipped.
			var found *synctypes.SkippedItem
			for i := range result.Skipped {
				if result.Skipped[i].Path == tt.panicPath {
					found = &result.Skipped[i]
					break
				}
			}
			require.NotNil(t, found, "panicking file should be in Skipped")
			assert.Equal(t, synctypes.IssueHashPanic, found.Reason)
			assert.Contains(t, found.Detail, "panic:")
		})
	}
}

// Validates: R-6.7.5
func TestFullScan_HashPanicDoesNotAbort(t *testing.T) {
	t.Parallel()

	// End-to-end test: a panic in hash phase should not abort FullScan.
	// The scan completes with events for successful files and SkippedItems
	// for panicking files.

	const badFileName = "bad.txt"

	dir := t.TempDir()
	writeTestFile(t, dir, "good.txt", "good content")
	writeTestFile(t, dir, badFileName, "bad content")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.HashFunc = func(path string) (string, error) {
		if filepath.Base(path) == badFileName {
			panic("corrupted file")
		}
		return hashContent(t, "good content"), nil
	}

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, dir))
	require.NoError(t, err, "FullScan must not return error on hash panic")

	// good.txt should produce an event.
	goodEv := findEvent(result.Events, "good.txt")
	require.NotNil(t, goodEv, "good.txt should still produce an event")
	assert.Equal(t, synctypes.ChangeCreate, goodEv.Type)

	// bad.txt should NOT produce an event.
	badEv := findEvent(result.Events, badFileName)
	assert.Nil(t, badEv, "panicking file should not produce an event")

	// bad.txt should be in Skipped.
	var badSkip *synctypes.SkippedItem
	for i := range result.Skipped {
		if result.Skipped[i].Path == badFileName {
			badSkip = &result.Skipped[i]
			break
		}
	}
	require.NotNil(t, badSkip, "panicking file should be in Skipped")
	assert.Equal(t, synctypes.IssueHashPanic, badSkip.Reason)
	assert.Contains(t, badSkip.Detail, "corrupted file")
}

// ---------------------------------------------------------------------------
// Constructor tests
// ---------------------------------------------------------------------------

// Validates that maps are initialized in constructor, not Watch().
func TestNewLocalObserver_MapsInitialized(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	assert.NotNil(t, obs.PendingTimers, "pendingTimers should be initialized in constructor")
	assert.NotNil(t, obs.HashRequests, "hashRequests should be initialized in constructor")
}
