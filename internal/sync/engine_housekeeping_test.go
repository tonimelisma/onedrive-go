package sync

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
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

// Validates: R-5.2
//
// Closing an engine cleans its stale upload sessions and reports the outcome
// once, with the count: a session it could not remove is a Warn, a clean run
// an Info. An engine closes once, so there is no streak to keep quiet here.
func TestEngineClose_ReportsStaleSessionCleanupWithTheCount(t *testing.T) {
	t.Parallel()

	t.Run("removal refused", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("the superuser ignores folder modes")
		}

		logs, logger := newDebugLogBuffer()
		eng, syncRoot := newTestEngineWithLogger(t, &engineMockClient{}, logger)
		makeReadOnlyForTest(t, saveStaleEngineSession(t, filepath.Join(filepath.Dir(syncRoot), "data")))

		require.NoError(t, eng.Close(t.Context()))

		require.Equal(t, 1, countLogLines(logs.String(), "failed to clean stale sessions on close"), logs.String())
		line := logLineWith(logs.String(), "failed to clean stale sessions on close")
		assert.Contains(t, line, "level=WARN")
		assert.Contains(t, line, "deleted=0")
		assert.Contains(t, line, "permission denied")
	})

	t.Run("removed", func(t *testing.T) {
		t.Parallel()

		logs, logger := newDebugLogBuffer()
		eng, syncRoot := newTestEngineWithLogger(t, &engineMockClient{}, logger)
		saveStaleEngineSession(t, filepath.Join(filepath.Dir(syncRoot), "data"))

		require.NoError(t, eng.Close(t.Context()))

		line := logLineWith(logs.String(), "cleaned stale upload sessions on close")
		assert.Contains(t, line, "level=INFO")
		assert.Contains(t, line, "deleted=1")
	})
}

// saveStaleEngineSession saves one upload session in the engine's data folder,
// last written longer ago than StaleSessionAge, and returns the folder that
// holds it.
func saveStaleEngineSession(t *testing.T, dataDir string) string {
	t.Helper()

	store := driveops.NewSessionStore(dataDir, testLogger(t))
	require.NoError(t, store.Save(engineTestDriveID, "/old.txt", &driveops.SessionRecord{SessionURL: "https://example.com/old"}))
	saved, err := filepath.Glob(filepath.Join(dataDir, "*", "*.json"))
	require.NoError(t, err)
	require.Len(t, saved, 1, "one saved session file")
	stale := time.Now().Add(-2 * driveops.StaleSessionAge)
	require.NoError(t, os.Chtimes(saved[0], stale, stale))

	return filepath.Dir(saved[0])
}

// logLineWith returns the first log line holding needle, or "".
func logLineWith(logs, needle string) string {
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}

	return ""
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

// Validates: R-6.6.21, R-6.6.30
//
// A shutdown cancels the context before the watch bootstrap's housekeeping
// runs (live, 2026-11-10): the protected-root read failed on the canceled
// context and warned "Could not refresh the shared-folder list" at a clean
// stop. A canceled context is no read failure. The cleanup is skipped at
// Debug and no failure streak starts, so the next pass cleans up without
// announcing a recovery.
func TestPostSyncHousekeeping_CanceledContextIsQuiet(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	rootPartial := filepath.Join(syncRoot, testOwnedPartialName("root.txt"))
	require.NoError(t, os.WriteFile(rootPartial, []byte("root"), 0o600))

	logBuf, logger := newDebugLogBuffer()
	flow := &engineFlow{
		engine: &Engine{
			syncTree:            mustOpenSyncTree(t, syncRoot),
			logger:              logger,
			baseline:            newTestStore(t),
			shortcutNamespaceID: shortcutNamespaceTestID,
			contentFilter:       ContentFilterConfig{},
		},
	}
	flow.deps = newRuntimeDeps(flow.engine)

	shuttingDown, cancel := context.WithCancel(t.Context())
	cancel()
	flow.postSyncHousekeeping(shuttingDown)
	assert.NotContains(t, logBuf.String(), "level=WARN", "a clean shutdown warns about nothing")
	assert.FileExists(t, rootPartial, "no cleanup walk with a protected-root list that was not read")

	flow.postSyncHousekeeping(t.Context())
	assert.NoFileExists(t, rootPartial, "the next pass cleans up")
	assert.NotContains(t, logBuf.String(), "transfer cleanup resumes", "a shutdown is no failure streak to recover from")
}

// Validates: R-6.6.30
//
// A leftover download file or an expired upload session file the cleanup
// cannot remove is still there the next time housekeeping runs. Each of the
// two is one Warn per failure streak and Debug on repeats; the first run that
// removes the rest says so at Info, and a new streak warns once more.
func TestPostSyncHousekeeping_WarnsOncePerStreakWhenLeftoversCannotBeRemoved(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}

	cases := []struct {
		name      string
		warn      string
		repeat    string
		recovered string
		// arrange returns an engine and a function that leaves one leftover
		// for its cleanup and returns the folder holding it.
		arrange func(t *testing.T, logger *slog.Logger) (*Engine, func() string)
	}{
		{
			name:      "leftover download file",
			warn:      "Could not remove every leftover download file",
			repeat:    "could not remove every leftover download file (still failing)",
			recovered: "leftover download files can be removed again",
			arrange: func(t *testing.T, logger *slog.Logger) (*Engine, func() string) {
				t.Helper()

				syncRoot := t.TempDir()
				locked := filepath.Join(syncRoot, "locked")
				require.NoError(t, os.Mkdir(locked, 0o700))
				eng := &Engine{
					syncTree:      mustOpenSyncTree(t, syncRoot),
					logger:        logger,
					contentFilter: ContentFilterConfig{},
				}

				return eng, func() string {
					partial := filepath.Join(locked, testOwnedPartialName("stuck.txt"))
					require.NoError(t, os.WriteFile(partial, []byte("x"), 0o600))

					return locked
				}
			},
		},
		{
			name:      "expired upload session file",
			warn:      "Could not remove every expired upload session file",
			repeat:    "could not remove every expired upload session file (still failing)",
			recovered: "expired upload session files can be removed again",
			arrange: func(t *testing.T, logger *slog.Logger) (*Engine, func() string) {
				t.Helper()

				dataDir := t.TempDir()
				eng := &Engine{
					syncTree:      mustOpenSyncTree(t, t.TempDir()),
					sessionStore:  driveops.NewSessionStore(dataDir, logger),
					logger:        logger,
					contentFilter: ContentFilterConfig{},
				}

				return eng, func() string { return saveStaleEngineSession(t, dataDir) }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logs, logger := newDebugLogBuffer()
			eng, leaveLeftover := tc.arrange(t, logger)
			flow := &engineFlow{engine: eng}
			flow.deps = newRuntimeDeps(eng)

			dir := leaveLeftover()
			leftovers := func() int {
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)

				return len(entries)
			}
			makeReadOnlyForTest(t, dir)

			flow.postSyncHousekeeping(t.Context())
			flow.postSyncHousekeeping(t.Context())
			require.Equal(t, 1, countLogLines(logs.String(), tc.warn), "one Warn per streak: %s", logs.String())
			assert.Contains(t, logLineWith(logs.String(), tc.warn), "level=WARN")
			assert.Contains(t, logLineWith(logs.String(), tc.warn), "permission denied")
			require.Equal(t, 1, countLogLines(logs.String(), tc.repeat), "a repeat in the same streak is Debug")
			assert.Contains(t, logLineWith(logs.String(), tc.repeat), "level=DEBUG")
			assert.Equal(t, 1, leftovers(), "what could not be removed stays")

			// Recovery: the folder is writable again, the leftover goes and
			// the streak ends. A clean pass after that says nothing more.
			setTestDirPermissions(t, dir, 0o700)
			flow.postSyncHousekeeping(t.Context())
			flow.postSyncHousekeeping(t.Context())
			assert.Zero(t, leftovers())
			require.Equal(t, 1, countLogLines(logs.String(), tc.recovered), logs.String())
			assert.Contains(t, logLineWith(logs.String(), tc.recovered), "level=INFO")
			assert.Equal(t, 1, countLogLines(logs.String(), tc.warn))

			// A new streak is reported once more.
			leaveLeftover()
			setTestDirPermissions(t, dir, 0o500)
			flow.postSyncHousekeeping(t.Context())
			assert.Equal(t, 2, countLogLines(logs.String(), tc.warn), "a new streak is reported once more")
		})
	}
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
