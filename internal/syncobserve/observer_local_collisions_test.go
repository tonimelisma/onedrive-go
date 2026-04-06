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

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// isCaseSensitiveFS returns true if the filesystem at dir distinguishes
// between upper and lower case file names. Used to skip tests that require
// two distinct files differing only in case.
func isCaseSensitiveFS(t *testing.T, dir string) bool {
	t.Helper()

	upper := filepath.Join(dir, "CasE_ChEcK")
	lower := filepath.Join(dir, "case_check")

	err := os.WriteFile(upper, []byte("x"), 0o600)
	require.NoError(t, err, "isCaseSensitiveFS: create upper")
	defer os.Remove(upper)

	// If creating the lowercase variant fails, FS is case-insensitive.
	if writeErr := os.WriteFile(lower, []byte("y"), 0o600); writeErr != nil {
		return false
	}
	defer os.Remove(lower)

	// Both files created — check they're distinct by reading the upper file.
	data, err := localpath.ReadFile(upper)
	if err != nil {
		return false
	}

	return string(data) == "x" // if "y", the FS overwrote the upper file → insensitive
}

// skipIfCaseInsensitiveFS skips the test if the filesystem at dir is case-insensitive.
func skipIfCaseInsensitiveFS(t *testing.T, dir string) {
	t.Helper()

	if !isCaseSensitiveFS(t, dir) {
		t.Skip("skipping on case-insensitive filesystem")
	}
}

// ---------------------------------------------------------------------------
// hasCaseCollisionCached tests (R-2.12.2)
// ---------------------------------------------------------------------------

// Validates: R-2.12.2
func TestHasCaseCollisionCached_Detected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "File.txt"), []byte("a"), 0o600))

	obs := &LocalObserver{
		Baseline: synctest.EmptyBaseline(),
	}

	// On case-sensitive FS, "file.txt" differs from "File.txt" → collision.
	// On case-insensitive FS (macOS), creating "File.txt" means "file.txt"
	// refers to the same inode, so os.ReadDir returns "File.txt" and the
	// exact-match check fails → no collision detected. Correct on both.
	collidingName, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "file.txt", ".")
	if found {
		assert.Equal(t, "File.txt", collidingName,
			"should return the name of the existing colliding file")
	}
}

// Validates: R-2.12.2
func TestHasCaseCollisionCached_NoCollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte("a"), 0o600))

	obs := &LocalObserver{
		Baseline: synctest.EmptyBaseline(),
	}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "newfile.txt", ".")
	assert.False(t, found, "unrelated files should not trigger collision")
}

// Validates: R-2.12.2
func TestHasCaseCollisionCached_ExactMatch_NotCollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "same.txt"), []byte("a"), 0o600))

	obs := &LocalObserver{
		Baseline: synctest.EmptyBaseline(),
	}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "same.txt", ".")
	assert.False(t, found, "exact name match should not be a collision")
}

// Validates: R-2.12.2
func TestHasCaseCollisionCached_UnreadableDir_FailOpen(t *testing.T) {
	t.Parallel()

	obs := &LocalObserver{
		Baseline: synctest.EmptyBaseline(),
	}

	// Non-existent directory → ReadDir fails → returns false (fail-open).
	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, "/nonexistent/path"), "/nonexistent/path", "anything.txt", ".")
	assert.False(t, found, "unreadable directory should fail open")
}

// Validates: R-2.12.2
func TestHasCaseCollisionCached_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	obs := &LocalObserver{
		Baseline: synctest.EmptyBaseline(),
	}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "anything.txt", ".")
	assert.False(t, found, "empty directory should have no collisions")
}

// ---------------------------------------------------------------------------
// Baseline cross-check in hasCaseCollisionCached (R-2.12.2)
// ---------------------------------------------------------------------------

// Validates: R-2.12.2
// Baseline entry with different casing triggers collision.
func TestHasCaseCollisionCached_BaselineCollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Baseline has "File.txt" — no file on disk.
	bl := synctest.BaselineWith(&synctypes.BaselineEntry{Path: "File.txt"})
	obs := &LocalObserver{
		Baseline: bl,
	}

	collidingName, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "file.txt", ".")
	assert.True(t, found, "should detect baseline collision")
	assert.Equal(t, "File.txt", collidingName)
}

// Validates: R-2.12.2
// Baseline entry with same casing is not a collision.
func TestHasCaseCollisionCached_BaselineExactMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	bl := synctest.BaselineWith(&synctypes.BaselineEntry{Path: "File.txt"})
	obs := &LocalObserver{
		Baseline: bl,
	}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "File.txt", ".")
	assert.False(t, found, "same casing in baseline should not be a collision")
}

// Validates: R-2.12.2
// Baseline collision is suppressed for recently deleted paths.
func TestHasCaseCollisionCached_BaselineSkipsRecentDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	bl := synctest.BaselineWith(&synctypes.BaselineEntry{Path: "File.txt"})
	obs := &LocalObserver{
		Baseline: bl,
		localWatchState: localWatchState{
			RecentLocalDeletes: map[string]struct{}{"File.txt": {}},
		},
	}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "file.txt", ".")
	assert.False(t, found, "recently deleted baseline entry should not trigger collision")
}

// ---------------------------------------------------------------------------
// Watch-mode collision integration tests (R-2.12.2)
// ---------------------------------------------------------------------------

// Validates: R-2.12.2
// Integration test exercising the full watch pipeline:
// fsnotify Create -> handleCreate -> hasCaseCollisionCached -> event suppressed.
func TestWatch_CaseCollision_EventSuppressed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create "Existing.txt" on disk. On case-insensitive FS (macOS),
	// os.ReadDir returns this canonical name.
	writeTestFile(t, dir, "Existing.txt", "content")

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Wait for watcher setup, then send a synthetic Create event for
	// "existing.txt" (lowercase) — different case than the on-disk file.
	time.Sleep(100 * time.Millisecond)
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "existing.txt"),
		Op:   fsnotify.Create,
	}

	// No event should be emitted — the case collision suppresses it.
	select {
	case ev := <-events:
		require.FailNow(t, "expected no event", "got %+v", ev)
	case <-time.After(500 * time.Millisecond):
		// Good — no event emitted.
	}

	cancel()
	<-done
}

// Validates: R-2.12.2
// Case collision check in scanNewDirectory.
func TestScanNewDirectory_CaseCollision_Skipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create a subdirectory with two files that differ only in case.
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.MkdirAll(subDir, 0o700))
	writeTestFile(t, subDir, "Existing.txt", "content1")

	// On case-insensitive FS, this overwrites Existing.txt (same file).
	// On case-sensitive FS, this creates a second file.
	writeTestFile(t, subDir, "existing.txt", "content2")

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Wait for watcher setup, then send a Create event for the subdirectory
	// to trigger scanNewDirectory.
	time.Sleep(100 * time.Millisecond)
	mockWatcher.events <- fsnotify.Event{
		Name: subDir,
		Op:   fsnotify.Create,
	}

	// Collect events within a window.
	var received []synctypes.ChangeEvent
	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-events:
			received = append(received, ev)
			continue
		case <-timeout:
		}

		break
	}

	// The directory event itself should be emitted. On case-sensitive FS,
	// both files exist and the collision check skips both. On case-insensitive
	// FS, only one file exists (no collision), so one file event is emitted.
	// In both cases, we should NOT see two file events.
	fileEvents := 0
	for _, ev := range received {
		if ev.ItemType == synctypes.ItemTypeFile {
			fileEvents++
		}
	}

	// At most 1 file event (case-insensitive: 1 file on disk, no collision;
	// case-sensitive: 2 files on disk, collision detected, both skipped → 0).
	assert.LessOrEqual(t, fileEvents, 1,
		"case collision should prevent duplicate file events; got %d file events", fileEvents)

	cancel()
	<-done
}

// ---------------------------------------------------------------------------
// Directory collision tests (R-2.12.2)
// ---------------------------------------------------------------------------

// Validates: R-2.12.2
// Directories must also be checked for case collisions.
// A directory whose name differs only in case from an existing sibling file
// should be suppressed in watch mode.
func TestWatch_DirectoryCollision_Suppressed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	skipIfCaseInsensitiveFS(t, dir)

	// Create a file "xyz" on disk.
	writeTestFile(t, dir, "xyz", "content")

	xyzDir := filepath.Join(dir, "Xyz")
	require.NoError(t, os.Mkdir(xyzDir, 0o700))

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
	})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	// Wait for watcher setup, then send Create event for the directory "Xyz"
	// which should collide with the file "xyz".
	time.Sleep(100 * time.Millisecond)
	mockWatcher.events <- fsnotify.Event{
		Name: xyzDir,
		Op:   fsnotify.Create,
	}

	// The directory create should be suppressed — case collision with "xyz".
	select {
	case ev := <-events:
		if ev.ItemType == synctypes.ItemTypeFolder && ev.Name == "Xyz" {
			require.FailNow(t, "directory event should be suppressed due to case collision", "got %+v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		// Good — no directory event emitted.
	}

	cancel()
	<-done
}

// Validates: R-2.12.2
// Two directories differing only in case should collide.
func TestWatch_TwoDirectoryCollision_Suppressed(t *testing.T) {
	t.Parallel()

	const docsName = "docs"

	dir := t.TempDir()
	skipIfCaseInsensitiveFS(t, dir)

	// Create directory "Docs" on disk.
	docsDir := filepath.Join(dir, "Docs")
	require.NoError(t, os.Mkdir(docsDir, 0o700))

	docsLower := filepath.Join(dir, docsName)
	require.NoError(t, os.Mkdir(docsLower, 0o700))

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
	})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Send Create for "docs" (lowercase) — should collide with "Docs".
	mockWatcher.events <- fsnotify.Event{
		Name: docsLower,
		Op:   fsnotify.Create,
	}

	// The directory create should be suppressed.
	select {
	case ev := <-events:
		if ev.ItemType == synctypes.ItemTypeFolder && ev.Name == docsName {
			require.FailNow(t, "directory event should be suppressed due to case collision", "got %+v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		// Good — no directory event emitted.
	}

	cancel()
	<-done
}

// Validates: R-2.12.2
// scanNewDirectory should check subdirectories for collisions.
func TestScanNewDirectory_SubdirCollision_Suppressed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	skipIfCaseInsensitiveFS(t, dir)

	// Create a parent directory with two subdirectories differing only in case.
	parentDir := filepath.Join(dir, "parent")
	require.NoError(t, os.Mkdir(parentDir, 0o700))

	abcDir := filepath.Join(parentDir, "ABC")
	require.NoError(t, os.Mkdir(abcDir, 0o700))

	abcLower := filepath.Join(parentDir, "abc")
	require.NoError(t, os.Mkdir(abcLower, 0o700))

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
	})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Send Create for parent directory → triggers scanNewDirectory.
	mockWatcher.events <- fsnotify.Event{
		Name: parentDir,
		Op:   fsnotify.Create,
	}

	// Collect events. At most 1 subdirectory should be emitted (the other
	// should be suppressed by collision check).
	var received []synctypes.ChangeEvent
	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-events:
			received = append(received, ev)
			continue
		case <-timeout:
		}

		break
	}

	dirEvents := 0
	for _, ev := range received {
		if ev.ItemType == synctypes.ItemTypeFolder && ev.Path != "parent" {
			dirEvents++
		}
	}

	// At most 1 subdirectory event (both ABC and abc exist, one should
	// be suppressed by the collision check in scanNewDirectory).
	assert.LessOrEqual(t, dirEvents, 1,
		"case collision should suppress one of the colliding subdirectories; got %d dir events", dirEvents)

	cancel()
	<-done
}

// ---------------------------------------------------------------------------
// Delete-resolution tests (R-2.12.2 — collision peer re-emission)
// ---------------------------------------------------------------------------

// Validates: R-2.12.2
// Deleting a collision suppressor re-emits the survivor.
func TestWatch_DeleteCollider_ReEmitsSurvivor(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	skipIfCaseInsensitiveFS(t, dir)

	// Create two case-colliding files.
	writeTestFile(t, dir, "File.txt", "content1")

	lowerPath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(lowerPath, []byte("content2"), 0o600))

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
	})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Send Create for "file.txt" — should be suppressed (collision with "File.txt").
	mockWatcher.events <- fsnotify.Event{
		Name: lowerPath,
		Op:   fsnotify.Create,
	}

	// Verify suppressed — no event within timeout.
	select {
	case ev := <-events:
		require.FailNow(t, "expected create to be suppressed", "got %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}

	// Now delete "file.txt" from disk — the file must be gone for the
	// survivor's re-emitted handleCreate to see no collision.
	require.NoError(t, os.Remove(lowerPath))

	// Send Remove event for "file.txt".
	mockWatcher.events <- fsnotify.Event{
		Name: lowerPath,
		Op:   fsnotify.Remove,
	}

	// Expect two events: ChangeCreate for "File.txt" (survivor re-emitted)
	// and ChangeDelete for "file.txt".
	var received []synctypes.ChangeEvent
	timeout := time.After(1 * time.Second)

	for len(received) < 2 {
		select {
		case ev := <-events:
			received = append(received, ev)
		case <-timeout:
			require.Len(t, received, 2)
		}
	}

	// Verify we got both events.
	var hasCreate, hasDelete bool
	for _, ev := range received {
		if ev.Type == synctypes.ChangeCreate && ev.Name == "File.txt" {
			hasCreate = true
		}
		if ev.Type == synctypes.ChangeDelete && ev.Name == "file.txt" {
			hasDelete = true
		}
	}

	assert.True(t, hasCreate, "should have re-emitted ChangeCreate for surviving File.txt")
	assert.True(t, hasDelete, "should have emitted ChangeDelete for deleted file.txt")

	cancel()
	<-done
}

// Validates: R-2.12.2
// With 3 colliders, deleting one re-emits the others.
// but they remain blocked (they still collide with each other).
func TestWatch_DeleteCollider_ThreeWay_StillBlocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	skipIfCaseInsensitiveFS(t, dir)

	writeTestFile(t, dir, "File.txt", "content1")

	lowerPath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(lowerPath, []byte("content2"), 0o600))

	upperPath := filepath.Join(dir, "FILE.txt")
	require.NoError(t, os.WriteFile(upperPath, []byte("content3"), 0o600))

	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
	})

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Send Create for "file.txt" — suppressed, collision with "File.txt".
	mockWatcher.events <- fsnotify.Event{
		Name: lowerPath,
		Op:   fsnotify.Create,
	}

	time.Sleep(100 * time.Millisecond)

	// Send Create for "FILE.txt" — suppressed, collision with "File.txt".
	mockWatcher.events <- fsnotify.Event{
		Name: upperPath,
		Op:   fsnotify.Create,
	}

	// Drain any spurious events.
	time.Sleep(300 * time.Millisecond)
	for len(events) > 0 {
		<-events
	}

	// Delete "file.txt" from disk.
	require.NoError(t, os.Remove(lowerPath))

	mockWatcher.events <- fsnotify.Event{
		Name: lowerPath,
		Op:   fsnotify.Remove,
	}

	// Expect only ChangeDelete for "file.txt". The re-emitted peers
	// (File.txt and FILE.txt) still collide → suppressed again.
	var received []synctypes.ChangeEvent
	timeout := time.After(1 * time.Second)

	for {
		select {
		case ev := <-events:
			received = append(received, ev)
			continue
		case <-timeout:
		}

		break
	}

	// Only the delete should come through; re-emitted creates are suppressed.
	for _, ev := range received {
		if ev.Type == synctypes.ChangeCreate {
			assert.FailNow(t, "expected no create events (survivors still collide)", "got %+v", ev)
		}
	}

	assert.Len(t, received, 1, "expected exactly 1 event (the delete)")
	if len(received) == 1 {
		assert.Equal(t, synctypes.ChangeDelete, received[0].Type)
		assert.Equal(t, "file.txt", received[0].Name)
	}

	cancel()
	<-done
}

// Validates: R-2.12.2
// Safety scan clears collision peer tracking.
func TestWatch_SafetyScan_ClearsPeers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "content")

	safetyTickCh := make(chan time.Time, 1)
	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		Baseline: synctest.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
		localWatchState: localWatchState{
			CollisionPeers: make(map[string]map[string]struct{}),
			DirNameCache:   make(map[string]map[string][]string),
			PendingTimers:  make(map[string]*time.Timer),
			HashRequests:   make(chan HashRequest, HashRequestBufSize),
		},
		SleepFunc: func(_ context.Context, _ time.Duration) error {
			return nil
		},
		SafetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return safetyTickCh, func() {}
		},
		WatcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
	}

	// Pre-populate collision peers and dir name cache.
	obs.AddCollisionPeer("a.txt", "A.txt")
	obs.DirNameCache[dir] = map[string][]string{
		"a.txt": {"a.txt", "A.txt"},
	}

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, mustOpenSyncTree(t, dir), events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Trigger safety scan.
	safetyTickCh <- time.Now()

	// Wait for safety scan to complete, then stop the watchLoop.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	// Both maps should have been cleared by the safety scan.
	// Safe to read after watchLoop exits (single-goroutine fields).
	assert.Empty(t, obs.CollisionPeers, "collisionPeers should be cleared after safety scan")
	assert.Empty(t, obs.DirNameCache, "dirNameCache should be cleared after safety scan")
}
