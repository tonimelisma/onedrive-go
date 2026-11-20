package sync

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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
			syncTree:      syncfixture.MustOpenSyncTree(t, syncRoot),
			logger:        syncfixture.TestLogger(t),
			contentFilter: syncdomain.ContentFilterConfig{IgnoredDirs: []string{"Shortcuts/Docs"}},
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
			syncTree:      syncfixture.MustOpenSyncTree(t, syncRoot),
			logger:        syncfixture.TestLogger(t),
			contentFilter: syncdomain.ContentFilterConfig{},
			protectedRoots: []syncshortcut.ProtectedRoot{{
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

// Validates: R-6.3.4
//
// The parent's final-drain acknowledgement runs on the multisync
// orchestrator's goroutine, through the ack handle, and rederives the
// protected roots while the parent's own run does its housekeeping. Run under
// -race: housekeeping reads the protected roots through the lock, like every
// other reader of the field.
//
// The hand-off lies between housekeeping's refresh and its read. A refresh
// failure streak that ends logs its recovery line after housekeeping's own
// refresh and before the cleanup reads the list; the logger hands that line
// to the acknowledging goroutine, which then rewrites the list. The test adds
// no synchronization between the hand-off and the read: the logger takes no
// lock, and housekeeping touches neither the sync store nor the context
// until the acknowledgement is back. An unguarded read can therefore fail
// under -race, though not in every run.
func TestPostSyncHousekeeping_ReadsProtectedRootsUnderTheLock(t *testing.T) {
	t.Parallel()

	refreshed := make(chan struct{}, 1)
	logger := slog.New(signalingHandler{
		message: protectedRootRefreshRecoveredMessage,
		signal:  refreshed,
	})
	eng, syncRoot := newTestEngineWithLogger(t, &syncfixture.EngineMockClient{}, logger)
	eng.shortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
	ack := eng.ShortcutChildAckHandle()

	const rounds = 10
	acked := make(chan error, 1)
	stop := make(chan struct{})
	var wg stdsync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-refreshed:
				_, err := ack.AcknowledgeChildFinalDrain(t.Context(), syncshortcut.ShortcutChildDrainAck{
					Ref: syncshortcut.NewShortcutChildAckRefForTest("binding-1"),
				})
				acked <- err
			}
		}
	}()
	defer wg.Wait()
	defer close(stop)

	for range rounds {
		seedRemovedSharedFolders(t, eng.Engine, syncRoot)
		eng.protectedRootRefreshFailing = true
		eng.flow.postSyncHousekeeping(t.Context())
		require.NoError(t, syncfixture.ReceiveWithin[error](t, acked,
			"the final-drain acknowledgement, which starts when housekeeping logs its refresh recovery line"))
		assert.Empty(t, eng.currentProtectedRoots(),
			"the acknowledgement rewrote the list housekeeping had just refreshed")
	}
}

// signalingHandler drops every record and sends on signal for each record
// whose message is message. It takes no lock, so it orders nothing between
// the goroutines that log through it.
type signalingHandler struct {
	message string
	signal  chan<- struct{}
}

func (h signalingHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler fixes the by-value Record parameter.
func (h signalingHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == h.message {
		h.signal <- struct{}{}
	}

	return nil
}

func (h signalingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h signalingHandler) WithGroup(string) slog.Handler { return h }

// seedRemovedSharedFolders stores two removed shared folders with emptied
// local roots: one whose child is finishing its final drain, which the test
// acknowledges, and one whose release is pending. The acknowledgement
// releases the first itself; the shortcut-root reconcile it runs then
// releases the second and rederives the protected roots.
func seedRemovedSharedFolders(t *testing.T, eng *Engine, syncRoot string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "Shared", "Docs"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "Shared", "Notes"), 0o700))
	require.NoError(t, eng.baseline.ReplaceShortcutRoots(t.Context(), []syncshortcut.ShortcutRootRecord{
		{
			NamespaceID:       syncfixture.ShortcutNamespaceTestID,
			BindingItemID:     "binding-1",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     driveid.New("drive-1"),
			RemoteItemID:      "target-1",
			RemoteIsFolder:    true,
			State:             syncshortcut.ShortcutRootStateRemovedFinalDrain,
			ProtectedPaths:    []string{"Shared/Docs"},
		},
		{
			NamespaceID:       syncfixture.ShortcutNamespaceTestID,
			BindingItemID:     "binding-2",
			RelativeLocalPath: "Shared/Notes",
			LocalAlias:        "Notes",
			RemoteDriveID:     driveid.New("drive-1"),
			RemoteItemID:      "target-2",
			RemoteIsFolder:    true,
			State:             syncshortcut.ShortcutRootStateRemovedReleasePending,
			ProtectedPaths:    []string{"Shared/Notes"},
		},
	}))
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
			syncTree:      syncfixture.MustOpenSyncTree(t, syncRoot),
			logger:        syncfixture.TestLogger(t),
			contentFilter: syncdomain.ContentFilterConfig{IncludedDirs: []string{"Projects"}},
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
func closedTestStore(t *testing.T) *syncstore.SyncStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "closed.db")
	require.NoError(t, os.WriteFile(dbPath, syncstoretest.LoadTestStoreTemplate(t), 0o600))
	store, err := syncstore.OpenSyncStoreForTest(synctest.TestContext(t), dbPath, synctest.TestLogger(t), false)
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))

	return store
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

		logs, logger := syncfixture.NewDebugLogBuffer()
		eng, syncRoot := newTestEngineWithLogger(t, &syncfixture.EngineMockClient{}, logger)
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

		logs, logger := syncfixture.NewDebugLogBuffer()
		eng, syncRoot := newTestEngineWithLogger(t, &syncfixture.EngineMockClient{}, logger)
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

	store := driveops.NewSessionStore(dataDir, syncfixture.TestLogger(t))
	require.NoError(t, store.Save(syncfixture.EngineTestDriveID, "/old.txt", &driveops.SessionRecord{SessionURL: "https://example.com/old"}))
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

	logBuf, logger := syncfixture.NewDebugLogBuffer()
	broken := closedTestStore(t)
	flow := &engineFlow{
		engine: &Engine{
			syncTree:            syncfixture.MustOpenSyncTree(t, syncRoot),
			logger:              logger,
			baseline:            broken,
			shortcutNamespaceID: syncfixture.ShortcutNamespaceTestID,
			contentFilter:       syncdomain.ContentFilterConfig{},
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
	flow.engine.baseline = syncstoretest.NewTestStore(t)
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

	logBuf, logger := syncfixture.NewDebugLogBuffer()
	flow := &engineFlow{
		engine: &Engine{
			syncTree:            syncfixture.MustOpenSyncTree(t, syncRoot),
			logger:              logger,
			baseline:            syncstoretest.NewTestStore(t),
			shortcutNamespaceID: syncfixture.ShortcutNamespaceTestID,
			contentFilter:       syncdomain.ContentFilterConfig{},
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
					syncTree:      syncfixture.MustOpenSyncTree(t, syncRoot),
					logger:        logger,
					contentFilter: syncdomain.ContentFilterConfig{},
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
					syncTree:      syncfixture.MustOpenSyncTree(t, t.TempDir()),
					sessionStore:  driveops.NewSessionStore(dataDir, logger),
					logger:        logger,
					contentFilter: syncdomain.ContentFilterConfig{},
				}

				return eng, func() string { return saveStaleEngineSession(t, dataDir) }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logs, logger := syncfixture.NewDebugLogBuffer()
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
			syncfixture.SetTestDirPermissions(t, dir, 0o700)
			flow.postSyncHousekeeping(t.Context())
			flow.postSyncHousekeeping(t.Context())
			assert.Zero(t, leftovers())
			require.Equal(t, 1, countLogLines(logs.String(), tc.recovered), logs.String())
			assert.Contains(t, logLineWith(logs.String(), tc.recovered), "level=INFO")
			assert.Equal(t, 1, countLogLines(logs.String(), tc.warn))

			// A new streak is reported once more.
			leaveLeftover()
			syncfixture.SetTestDirPermissions(t, dir, 0o500)
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

	logBuf, logger := syncfixture.NewDebugLogBuffer()
	eng := &Engine{
		trash:  localtrash.Open(trashPath, nil),
		logger: logger,
		hooks:  engineHooks{clock: syncfixture.NewManualClock(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))},
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
