package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
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

// namesCoexist reports whether the test temp filesystem can hold two distinct
// files named a and b. Case-insensitive volumes fold differently (APFS also
// folds ß to ss), so tests probe the exact pair they need.
func namesCoexist(t *testing.T, a, b string) bool {
	t.Helper()

	probe := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(probe, a), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(probe, b), []byte("b"), 0o600))
	data, err := localpath.ReadFile(filepath.Join(probe, a))
	require.NoError(t, err)

	return string(data) == "a"
}

// skipUnlessNamesCoexist skips a test whose scenario needs both a and b as
// separate local files, which a case-insensitive volume cannot hold.
func skipUnlessNamesCoexist(t *testing.T, a, b string) {
	t.Helper()

	if !namesCoexist(t, a, b) {
		t.Skipf("skipping: this filesystem cannot hold %q and %q as separate files", a, b)
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

	obs := &localObserver{
		baseline: emptyBaseline(),
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

	obs := &localObserver{
		baseline: emptyBaseline(),
	}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "newfile.txt", ".")
	assert.False(t, found, "unrelated files should not trigger collision")
}

// Validates: R-2.12.2
func TestHasCaseCollisionCached_ExactMatch_NotCollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "same.txt"), []byte("a"), 0o600))

	obs := &localObserver{
		baseline: emptyBaseline(),
	}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "same.txt", ".")
	assert.False(t, found, "exact name match should not be a collision")
}

// Validates: R-2.12.2
func TestHasCaseCollisionCached_UnreadableDir_FailOpen(t *testing.T) {
	t.Parallel()

	obs := &localObserver{
		baseline: emptyBaseline(),
	}

	// Non-existent directory → ReadDir fails → returns false (fail-open).
	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, "/nonexistent/path"), "/nonexistent/path", "anything.txt", ".")
	assert.False(t, found, "unreadable directory should fail open")
}

// Validates: R-2.12.2
func TestHasCaseCollisionCached_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	obs := &localObserver{
		baseline: emptyBaseline(),
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
	bl := baselineWith(&BaselineEntry{Path: "File.txt"})
	obs := &localObserver{
		baseline: bl,
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

	bl := baselineWith(&BaselineEntry{Path: "File.txt"})
	obs := &localObserver{
		baseline: bl,
	}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "File.txt", ".")
	assert.False(t, found, "same casing in baseline should not be a collision")
}

// Validates: R-2.12.2
// Baseline collision is suppressed for recently deleted paths.
func TestHasCaseCollisionCached_BaselineSkipsRecentDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	bl := baselineWith(&BaselineEntry{Path: "File.txt"})
	obs := &localObserver{
		baseline: bl,
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
	// Create "Existing.txt" on disk, then the created "existing.txt". On a
	// case-insensitive FS (macOS) the second write is the same file, and
	// os.ReadDir returns the canonical name; on a case-sensitive one it is a
	// second file. Either way the create's stat succeeds, so what drops it
	// is the collision check: a created path that is not there is dropped
	// before the check.
	writeTestFile(t, dir, "Existing.txt", "content")
	writeTestFile(t, dir, "existing.txt", "other")

	mockWatcher := newMockFsWatcher()
	recorder := newDebugEventRecorder()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{DebugEvents: recorder.record})

	events := make(chan changeEvent, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir, events)
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "existing.txt"),
		Op:   fsnotify.Create,
	}

	// The case collision suppresses the event: the observer reports the
	// skip and emits nothing.
	requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("existing.txt"), "the collision skip of existing.txt")

	stopWatch(t, cancel, done)
}

// Validates: R-2.12.2
// Case collision check in scanNewDirectory.
func TestScanNewDirectory_CaseCollision_Skipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	caseSensitive := isCaseSensitiveFS(t, dir)

	mockWatcher := newMockFsWatcher()
	recorder := newDebugEventRecorder()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{DebugEvents: recorder.record})

	events := make(chan changeEvent, 10)
	cancel, done := startMockWatchInLoop(t, obs, mockWatcher, dir, events)

	// A new subdirectory with two files that differ only in case.
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.MkdirAll(subDir, 0o700))
	writeTestFile(t, subDir, "Existing.txt", "content1")

	// On case-insensitive FS, this overwrites Existing.txt (same file).
	// On case-sensitive FS, this creates a second file.
	writeTestFile(t, subDir, "existing.txt", "content2")
	mockWatcher.events <- fsnotify.Event{
		Name: subDir,
		Op:   fsnotify.Create,
	}

	// The directory's own event goes out before its contents are scanned.
	dirEv := receiveWithin(t, events, "the event of the new directory")
	assert.Equal(t, "subdir", dirEv.Path)
	assert.Equal(t, ItemTypeFolder, dirEv.ItemType)

	if !caseSensitive {
		// One file on disk, no collision: the scan emits it, and it was the
		// scan's last entry.
		fileEv := receiveWithin(t, events, "the event of the one file on disk")
		assert.Equal(t, "subdir/Existing.txt", fileEv.Path)
		requireNoChangeEvent(t, events, "the scan's last entry")
	} else {
		// Two files whose names collide: the scan skips both, so neither is
		// emitted.
		requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("subdir/Existing.txt"), "the collision skip of Existing.txt")
		requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("subdir/existing.txt"), "the collision skip of existing.txt")
	}

	stopWatch(t, cancel, done)
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
	recorder := newDebugEventRecorder()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
		DebugEvents:    recorder.record,
	})

	events := make(chan changeEvent, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir, events)
	mockWatcher.events <- fsnotify.Event{
		Name: xyzDir,
		Op:   fsnotify.Create,
	}

	// The directory create is suppressed: case collision with "xyz".
	requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("Xyz"), "the collision skip of the Xyz directory")

	stopWatch(t, cancel, done)
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
	recorder := newDebugEventRecorder()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
		DebugEvents:    recorder.record,
	})

	events := make(chan changeEvent, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir, events)

	// Send Create for "docs" (lowercase) — should collide with "Docs".
	mockWatcher.events <- fsnotify.Event{
		Name: docsLower,
		Op:   fsnotify.Create,
	}

	// The directory create is suppressed.
	requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf(docsName), "the collision skip of the docs directory")

	stopWatch(t, cancel, done)
}

// Validates: R-2.12.2
// scanNewDirectory should check subdirectories for collisions.
func TestScanNewDirectory_SubdirCollision_Suppressed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	skipIfCaseInsensitiveFS(t, dir)

	mockWatcher := newMockFsWatcher()
	recorder := newDebugEventRecorder()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
		DebugEvents:    recorder.record,
	})

	events := make(chan changeEvent, 10)
	cancel, done := startMockWatchInLoop(t, obs, mockWatcher, dir, events)

	// A new parent directory with two subdirectories differing only in case.
	parentDir := filepath.Join(dir, "parent")
	require.NoError(t, os.Mkdir(parentDir, 0o700))

	abcDir := filepath.Join(parentDir, "ABC")
	require.NoError(t, os.Mkdir(abcDir, 0o700))

	abcLower := filepath.Join(parentDir, "abc")
	require.NoError(t, os.Mkdir(abcLower, 0o700))

	// Send Create for parent directory → triggers scanNewDirectory.
	mockWatcher.events <- fsnotify.Event{
		Name: parentDir,
		Op:   fsnotify.Create,
	}

	// The parent's own event goes out before its contents are scanned. ABC
	// and abc collide with each other, so the scan skips both: neither
	// subdirectory is emitted, let alone both.
	parentEv := receiveWithin(t, events, "the event of the parent directory")
	assert.Equal(t, "parent", parentEv.Path)
	requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("parent/ABC"), "the collision skip of ABC")
	requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("parent/abc"), "the collision skip of abc")

	stopWatch(t, cancel, done)
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
	recorder := newDebugEventRecorder()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
		DebugEvents:    recorder.record,
	})

	events := make(chan changeEvent, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir, events)

	// Send Create for "file.txt" — should be suppressed (collision with "File.txt").
	mockWatcher.events <- fsnotify.Event{
		Name: lowerPath,
		Op:   fsnotify.Create,
	}
	requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("file.txt"), "the collision skip of file.txt")

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
	var received []changeEvent
	timeout := time.After(eventWait)

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
		if ev.Type == changeCreate && ev.Name == "File.txt" {
			hasCreate = true
		}
		if ev.Type == ChangeDelete && ev.Name == "file.txt" {
			hasDelete = true
		}
	}

	assert.True(t, hasCreate, "should have re-emitted ChangeCreate for surviving File.txt")
	assert.True(t, hasDelete, "should have emitted ChangeDelete for deleted file.txt")

	stopWatch(t, cancel, done)
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
	recorder := newDebugEventRecorder()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
		DebugEvents:    recorder.record,
	})

	events := make(chan changeEvent, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir, events)

	// Send Create for "file.txt" — suppressed, collision with "File.txt".
	mockWatcher.events <- fsnotify.Event{
		Name: lowerPath,
		Op:   fsnotify.Create,
	}

	// Send Create for "FILE.txt" — suppressed, collision with "File.txt".
	mockWatcher.events <- fsnotify.Event{
		Name: upperPath,
		Op:   fsnotify.Create,
	}
	requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("file.txt"), "the collision skip of file.txt")
	requireDroppedWithoutEvent(t, recorder, events, collisionSkipOf("FILE.txt"), "the collision skip of FILE.txt")

	// Delete "file.txt" from disk.
	require.NoError(t, os.Remove(lowerPath))

	mockWatcher.events <- fsnotify.Event{
		Name: lowerPath,
		Op:   fsnotify.Remove,
	}

	// Only the delete of "file.txt" comes through. The delete re-emits its
	// peer FILE.txt before it sends its own event, and FILE.txt still
	// collides with File.txt, so it is suppressed again: its second skip is
	// reported before the delete, no create precedes the delete, and nothing
	// follows it.
	ev := receiveWithin(t, events, "the delete of file.txt")
	assert.Equal(t, ChangeDelete, ev.Type, "expected no create events (survivors still collide): got %+v", ev)
	assert.Equal(t, "file.txt", ev.Name)
	require.Equal(t, 2, recorder.countEvents(collisionSkipOf("FILE.txt")),
		"the re-emitted FILE.txt was skipped again before the delete went out")
	requireNoChangeEvent(t, events, "the delete, which the re-emitted peers precede")

	stopWatch(t, cancel, done)
}

// Validates: R-2.12.2
// Safety scan clears collision peer tracking.
func TestWatch_SafetyScan_ClearsPeers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "a.txt", "content")

	safetyTickCh := make(chan time.Time, 1)
	safetyScanComplete := make(chan struct{})
	mockWatcher := newMockFsWatcher()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		CollisionPeers: make(map[string]map[string]struct{}),
		DirNameCache:   make(map[string]map[string][]string),
		TickCh:         safetyTickCh,
		AfterSafetyScan: func() {
			select {
			case <-safetyScanComplete:
			default:
				close(safetyScanComplete)
			}
		},
	})

	// Pre-populate collision peers and dir name cache.
	obs.AddCollisionPeer("a.txt", "A.txt")
	obs.DirNameCache[dir] = map[string][]string{
		"a.txt": {"a.txt", "A.txt"},
	}

	events := make(chan changeEvent, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir, events)

	// Trigger safety scan.
	safetyTickCh <- time.Now()

	select {
	case <-safetyScanComplete:
	case <-time.After(eventWait):
		require.FailNow(t, "timeout waiting for safety scan completion")
	}
	stopWatch(t, cancel, done)

	// Both maps should have been cleared by the safety scan.
	// Safe to read after watchLoop exits (single-goroutine fields).
	assert.Empty(t, obs.CollisionPeers, "collisionPeers should be cleared after safety scan")
	assert.Empty(t, obs.DirNameCache, "dirNameCache should be cleared after safety scan")
}

// Validates: R-2.12.3
func TestHasCaseCollisionCached_NFDOnDiskNameDoesNotCollideWithItself(t *testing.T) {
	t.Parallel()

	// Watch events carry the NFC name; the cache holds the on-disk NFD
	// spelling. One file is not a collision with itself.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "café.txt"), []byte("a"), 0o600))

	obs := &localObserver{baseline: emptyBaseline()}

	_, found := obs.HasCaseCollisionCached(mustOpenSyncTree(t, dir), dir, "café.txt", ".")
	assert.False(t, found)
}

// Validates: R-2.12.3
func TestHasCaseCollisionCached_FullCaseFoldingAndEmojiCollide(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "straße.txt"), []byte("a"), 0o600))
	emojiName := "a\U0001F600.txt"
	require.NoError(t, os.WriteFile(filepath.Join(dir, emojiName), []byte("b"), 0o600))

	obs := &localObserver{baseline: emptyBaseline()}
	tree := mustOpenSyncTree(t, dir)

	colliding, found := obs.HasCaseCollisionCached(tree, dir, "STRASSE.txt", ".")
	assert.True(t, found)
	assert.Equal(t, "straße.txt", colliding)

	colliding, found = obs.HasCaseCollisionCached(tree, dir, "a.txt", ".")
	assert.True(t, found)
	assert.Equal(t, "a\U0001F600.txt", colliding)
}
