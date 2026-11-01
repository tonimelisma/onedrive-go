package sync

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func testOwnedPartialName(base string) string {
	return ".onedrive-go." + base + ".partial"
}

// Validates: R-2.4.10
func TestPostSyncHousekeeping_SkipsConfiguredChildRoots(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	childRoot := filepath.Join(syncRoot, "Shortcuts", "Docs")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, testOwnedPartialName("root.txt")), []byte("root"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, testOwnedPartialName("child.txt")), []byte("child"), 0o600))

	flow := &engineFlow{
		engine: &Engine{
			syncTree:      mustOpenSyncTree(t, syncRoot),
			logger:        testLogger(t),
			contentFilter: ContentFilterConfig{IgnoredDirs: []string{"Shortcuts/Docs"}},
		},
	}
	flow.deps = newRuntimeDeps(flow.engine)
	flow.postSyncHousekeeping(t.Context())

	assert.NoFileExists(t, filepath.Join(syncRoot, testOwnedPartialName("root.txt")))
	assert.FileExists(t, filepath.Join(childRoot, testOwnedPartialName("child.txt")))
}

// Validates: R-2.4.10
func TestPostSyncHousekeeping_SkipsProtectedShortcutRoots(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	childRoot := filepath.Join(syncRoot, "Shared Project")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, testOwnedPartialName("root.txt")), []byte("root"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, testOwnedPartialName("shortcut-sentinel.txt")), []byte("child"), 0o600))

	flow := &engineFlow{
		engine: &Engine{
			syncTree:      mustOpenSyncTree(t, syncRoot),
			logger:        testLogger(t),
			contentFilter: ContentFilterConfig{},
			protectedRoots: []ProtectedRoot{{
				Path:      "Shared Project",
				BindingID: "shortcut-binding",
			}},
		},
	}
	flow.deps = newRuntimeDeps(flow.engine)
	flow.postSyncHousekeeping(t.Context())

	assert.NoFileExists(t, filepath.Join(syncRoot, testOwnedPartialName("root.txt")))
	assert.FileExists(t, filepath.Join(childRoot, testOwnedPartialName("shortcut-sentinel.txt")))
}

// Validates: R-2.4.10
func TestPostSyncHousekeeping_RespectsIncludedDirs(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	includedRoot := filepath.Join(syncRoot, "Projects")
	excludedRoot := filepath.Join(syncRoot, "Archive")
	require.NoError(t, os.MkdirAll(includedRoot, 0o700))
	require.NoError(t, os.MkdirAll(excludedRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(includedRoot, testOwnedPartialName("owned.txt")), []byte("included"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(excludedRoot, "user.partial"), []byte("excluded"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "root.partial"), []byte("root"), 0o600))

	flow := &engineFlow{
		engine: &Engine{
			syncTree:      mustOpenSyncTree(t, syncRoot),
			logger:        testLogger(t),
			contentFilter: ContentFilterConfig{IncludedDirs: []string{"Projects"}},
		},
	}
	flow.deps = newRuntimeDeps(flow.engine)
	flow.postSyncHousekeeping(t.Context())

	assert.NoFileExists(t, filepath.Join(includedRoot, testOwnedPartialName("owned.txt")))
	assert.FileExists(t, filepath.Join(excludedRoot, "user.partial"))
	assert.FileExists(t, filepath.Join(syncRoot, "root.partial"))
}

// closedTestStore returns a store whose database is already closed, so every
// query fails the way an unreadable state database does.
func closedTestStore(t *testing.T) *SyncStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "closed.db")
	require.NoError(t, os.WriteFile(dbPath, loadTestStoreTemplate(t), 0o600))
	store, err := openSyncStore(synctest.TestContext(t), dbPath, synctest.TestLogger(t), false)
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))

	return store
}

func newDebugLogBuffer() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer

	return &buf, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func countLogLines(logs, needle string) int {
	count := 0
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, needle) {
			count++
		}
	}

	return count
}

// Validates: R-2.4.10, R-6.6.30
//
// The transfer-artifact cleanup removes every owned partial outside the
// protected roots with no age check. When the protected-root list cannot be
// refreshed it used to walk with the stale list at Debug, so a parent could
// delete a child mount's in-flight partials. The cleanup is now skipped for
// that pass, warned once per failure streak, and resumes when the store reads
// again.
func TestPostSyncHousekeeping_SkipsTransferCleanupWhenProtectedRootsCannotBeRead(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	childRoot := filepath.Join(syncRoot, "Shared Project")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	rootPartial := filepath.Join(syncRoot, testOwnedPartialName("root.txt"))
	childPartial := filepath.Join(childRoot, testOwnedPartialName("child.txt"))
	require.NoError(t, os.WriteFile(rootPartial, []byte("root"), 0o600))
	require.NoError(t, os.WriteFile(childPartial, []byte("child"), 0o600))

	logBuf, logger := newDebugLogBuffer()
	broken := closedTestStore(t)
	flow := &engineFlow{
		engine: &Engine{
			syncTree:            mustOpenSyncTree(t, syncRoot),
			logger:              logger,
			baseline:            broken,
			shortcutNamespaceID: shortcutNamespaceTestID,
			contentFilter:       ContentFilterConfig{},
		},
	}
	flow.deps = newRuntimeDeps(flow.engine)

	const warnLine = "Could not refresh the shared-folder list; skipping"

	flow.postSyncHousekeeping(t.Context())
	assert.FileExists(t, rootPartial, "no cleanup walk with a protected-root list that could not be refreshed")
	assert.FileExists(t, childPartial)
	assert.Equal(t, 1, countLogLines(logBuf.String(), warnLine))
	assert.Contains(t, logBuf.String(), "level=WARN")

	flow.postSyncHousekeeping(t.Context())
	assert.FileExists(t, rootPartial)
	assert.Equal(t, 1, countLogLines(logBuf.String(), warnLine), "a repeat in the same streak is Debug")
	assert.Equal(t, 1, countLogLines(logBuf.String(), "failed again"))

	// Recovery: the store reads again, cleanup resumes and the streak ends.
	flow.engine.baseline = newTestStore(t)
	flow.deps = newRuntimeDeps(flow.engine)
	flow.postSyncHousekeeping(t.Context())
	assert.NoFileExists(t, rootPartial, "cleanup runs once the protected roots are current")
	assert.Contains(t, logBuf.String(), "transfer cleanup resumes")

	// A new streak is reported once more.
	flow.engine.baseline = broken
	flow.deps = newRuntimeDeps(flow.engine)
	flow.postSyncHousekeeping(t.Context())
	assert.Equal(t, 2, countLogLines(logBuf.String(), warnLine))
}

// Validates: R-6.6.30
//
// A trash purge that keeps failing used to be Debug-only, hourly, forever,
// while the trash grew without bound. It is one Warn per failure streak,
// naming the trash directory, Debug on repeats, and the streak ends when a
// purge succeeds.
func TestPurgeExpiredTrash_WarnsOncePerFailureStreak(t *testing.T) {
	t.Parallel()

	trashPath := filepath.Join(t.TempDir(), "trash")
	breakTrash := func() {
		require.NoError(t, os.RemoveAll(trashPath))
		require.NoError(t, os.WriteFile(trashPath, []byte("a file where the trash directory should be"), 0o600))
	}
	repairTrash := func() {
		require.NoError(t, os.RemoveAll(trashPath))
		require.NoError(t, os.MkdirAll(trashPath, 0o700))
	}
	breakTrash()

	logBuf, logger := newDebugLogBuffer()
	eng := &Engine{
		trash:  localtrash.Open(trashPath, nil),
		logger: logger,
		clock:  newManualClock(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)),
	}

	const warnLine = "Could not remove expired files from the sync trash"

	eng.purgeExpiredTrash(true)
	eng.purgeExpiredTrash(true)
	assert.Equal(t, 1, countLogLines(logBuf.String(), warnLine), "one Warn per streak")
	assert.Contains(t, logBuf.String(), "level=WARN")
	assert.Contains(t, logBuf.String(), trashPath, "the Warn names the trash directory")
	assert.Equal(t, 1, countLogLines(logBuf.String(), "still failing"))

	repairTrash()
	eng.purgeExpiredTrash(true)
	assert.Contains(t, logBuf.String(), "expired files can be removed from the sync trash again")
	assert.Equal(t, 1, countLogLines(logBuf.String(), warnLine))

	breakTrash()
	eng.purgeExpiredTrash(true)
	assert.Equal(t, 2, countLogLines(logBuf.String(), warnLine), "a new streak is reported once more")
}
