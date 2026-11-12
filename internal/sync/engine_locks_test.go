package sync

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func secondEngineInputs(t *testing.T, syncRoot string, logger *slog.Logger) *engineInputs {
	t.Helper()

	other := t.TempDir()
	mock := &engineMockClient{}

	return &engineInputs{
		DBPath:          filepath.Join(other, "other.db"),
		SyncRoot:        syncRoot,
		DataDir:         other,
		DriveID:         driveid.New(engineTestDriveID),
		AccountEmail:    engineTestEmail,
		Fetcher:         mock,
		SocketIOFetcher: mock,
		Items:           mock,
		Downloads:       mock,
		Uploads:         mock,
		PathConvergence: &enginePathConvergenceStub{},
		FolderDelta:     mock,
		RecursiveLister: mock,
		PermChecker:     mock,
		Logger:          logger,
	}
}

// Validates: R-2.9.5
func TestEngine_SecondEngineOnSameRootRefused(t *testing.T) {
	t.Parallel()

	syncRoot := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))
	dataDir := t.TempDir()

	firstInputs := secondEngineInputs(t, syncRoot, testLogger(t))
	firstInputs.DataDir = dataDir
	first, err := newEngine(t.Context(), firstInputs)
	require.NoError(t, err)

	// A second configuration sharing the data dir and reaching the same
	// folder through a symlink must be refused before it opens its store.
	link := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(syncRoot, link))
	secondInputs := secondEngineInputs(t, link, testLogger(t))
	secondInputs.DataDir = dataDir
	_, err = newEngine(t.Context(), secondInputs)
	require.ErrorIs(t, err, ErrSyncRootInUse)
	assert.NoFileExists(t, secondInputs.DBPath, "the refused engine never opened its state store")

	require.NoError(t, first.Close(t.Context()))

	thirdInputs := secondEngineInputs(t, syncRoot, testLogger(t))
	thirdInputs.DataDir = dataDir
	third, err := newEngine(t.Context(), thirdInputs)
	require.NoError(t, err, "the folder is free once its engine closes")
	require.NoError(t, third.Close(t.Context()))
}

// Two configurations sharing a data directory can give one drive two
// different sync folders, so both engines name one state store. The folder
// lock cannot see that; the store lock refuses the second engine, which lets
// go of the folder it had already locked.
//
// Validates: R-2.9.5
func TestEngine_SecondEngineOnSameStoreRefused(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	firstInputs := secondEngineInputs(t, t.TempDir(), testLogger(t))
	firstInputs.DataDir = dataDir
	first, err := newEngine(t.Context(), firstInputs)
	require.NoError(t, err)

	otherFolder := t.TempDir()
	secondInputs := secondEngineInputs(t, otherFolder, testLogger(t))
	secondInputs.DataDir = dataDir
	secondInputs.DBPath = firstInputs.DBPath
	_, err = newEngine(t.Context(), secondInputs)
	require.ErrorIs(t, err, ErrSyncStoreInUse)
	folderLock, err := acquireSyncRootLock(dataDir, otherFolder, testLogger(t), nil)
	require.NoError(t, err, "the refused engine let go of its folder")
	require.NoError(t, folderLock.release())

	require.NoError(t, first.Close(t.Context()))
	second, err := newEngine(t.Context(), secondInputs)
	require.NoError(t, err, "the store is free once its engine closes")
	require.NoError(t, second.Close(t.Context()))
}

// The offline-change lock is the engine's own locks taken from outside, so an
// offline state change and an engine on the folder at the sync path exclude
// each other in both directions.
//
// Validates: R-2.9.5, R-6.3.9
func TestLockDriveForOfflineChange_ContendsWithAnEngineOnTheFolderAtItsPath(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	dataDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	engineLock, err := acquireSyncRootLock(dataDir, syncRoot, testLogger(t), nil)
	require.NoError(t, err)

	_, err = LockDriveForOfflineChange(t.Context(), dataDir, syncRoot, dbPath, testLogger(t))
	require.ErrorIs(t, err, ErrSyncRootInUse, "an engine holds the folder")

	require.NoError(t, engineLock.release())
	release, err := LockDriveForOfflineChange(t.Context(), dataDir, syncRoot, dbPath, testLogger(t))
	require.NoError(t, err, "the folder is free once the engine lets go")

	_, err = acquireSyncRootLock(dataDir, syncRoot, testLogger(t), nil)
	require.ErrorIs(t, err, ErrSyncRootInUse, "no engine starts during the offline change")
	require.NoError(t, release())
}

// An engine started before its sync folder was moved or replaced still has
// the store open while another folder sits at the sync path, which is the
// case rebind-root is for. The store lock finds that engine without the
// store having recorded anything about its folder, and a refused change
// leaves no lock behind.
//
// Validates: R-2.9.5, R-6.3.9
func TestLockDriveForOfflineChange_ContendsWithAnEngineWhoseFolderMoved(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	syncRoot := filepath.Join(parent, "sync")
	require.NoError(t, os.Mkdir(syncRoot, 0o750))
	inputs := secondEngineInputs(t, syncRoot, testLogger(t))
	engine, err := newEngine(t.Context(), inputs)
	require.NoError(t, err)
	require.NoError(t, os.Rename(syncRoot, filepath.Join(parent, "moved")))
	require.NoError(t, os.Mkdir(syncRoot, 0o750))

	_, err = LockDriveForOfflineChange(t.Context(), inputs.DataDir, syncRoot, inputs.DBPath, testLogger(t))
	require.ErrorIs(t, err, ErrSyncStoreInUse, "the engine on the moved folder still has the store open")
	folderLock, err := acquireSyncRootLock(inputs.DataDir, syncRoot, testLogger(t), nil)
	require.NoError(t, err, "a refused change leaves the folder at the path unlocked")
	require.NoError(t, folderLock.release())

	require.NoError(t, engine.Close(t.Context()))
	release, err := LockDriveForOfflineChange(t.Context(), inputs.DataDir, syncRoot, inputs.DBPath, testLogger(t))
	require.NoError(t, err)
	_, err = newEngine(t.Context(), inputs)
	require.ErrorIs(t, err, ErrSyncRootInUse, "no engine starts on the folder during the change")
	elsewhere := secondEngineInputs(t, t.TempDir(), testLogger(t))
	elsewhere.DataDir = inputs.DataDir
	elsewhere.DBPath = inputs.DBPath
	_, err = newEngine(t.Context(), elsewhere)
	require.ErrorIs(t, err, ErrSyncStoreInUse, "nor on the store from another folder")
	require.NoError(t, release())
}

// A missing folder, or none named (a drive no longer in the config), adds no
// folder lock. The store lock is taken whether or not the store exists: an
// engine starting meanwhile would create it.
//
// Validates: R-2.9.5, R-6.3.9
func TestLockDriveForOfflineChange_WithoutAFolderTakesTheStoreLock(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	inputs := secondEngineInputs(t, t.TempDir(), testLogger(t))
	inputs.DataDir = dataDir
	release, err := LockDriveForOfflineChange(t.Context(), dataDir, filepath.Join(t.TempDir(), "gone"),
		inputs.DBPath, testLogger(t))
	require.NoError(t, err)

	_, err = LockDriveForOfflineChange(t.Context(), dataDir, "", inputs.DBPath, testLogger(t))
	require.ErrorIs(t, err, ErrSyncStoreInUse)
	_, err = newEngine(t.Context(), inputs)
	require.ErrorIs(t, err, ErrSyncStoreInUse)
	assert.NoFileExists(t, inputs.DBPath, "the refused engine never created its state store")
	require.NoError(t, release())

	release, err = LockDriveForOfflineChange(t.Context(), dataDir, "", inputs.DBPath, testLogger(t))
	require.NoError(t, err, "the release let go of the store")
	require.NoError(t, release())
}

// Validates: R-2.9.5
func TestSyncStoreLock_NamesOneFilePerStore(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	storeDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(storeDir, link))

	direct, err := syncStoreLockPath(dataDir, filepath.Join(storeDir, "state.db"))
	require.NoError(t, err)
	viaLink, err := syncStoreLockPath(dataDir, filepath.Join(link, "state.db"))
	require.NoError(t, err)
	notYetDirect, err := syncStoreLockPath(dataDir, filepath.Join(storeDir, "later", "state.db"))
	require.NoError(t, err)
	notYetViaLink, err := syncStoreLockPath(dataDir, filepath.Join(link, "later", "state.db"))
	require.NoError(t, err)
	other, err := syncStoreLockPath(dataDir, filepath.Join(storeDir, "other.db"))
	require.NoError(t, err)

	assert.Equal(t, direct, viaLink, "every path to one store names one lock")
	assert.Equal(t, notYetDirect, notYetViaLink, "also for a store whose folder does not exist yet")
	assert.NotEqual(t, direct, other)
	assert.NotEqual(t, direct, notYetDirect)
	assert.Equal(t, filepath.Join(dataDir, "locks"), filepath.Dir(direct), "the lock lives beside the folder locks")
	assert.NoFileExists(t, filepath.Join(storeDir, "state.db"), "naming the lock needs no store")
}

// Validates: R-2.9.5
func TestSyncRootLock_IsADataDirFileKeyedByDeviceAndInode(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	dataDir := t.TempDir()
	lock, err := acquireSyncRootLock(dataDir, syncRoot, testLogger(t), nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, lock.release()) })

	info, err := os.Stat(syncRoot)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	want := filepath.Join(dataDir, "locks", fmt.Sprintf("root-%d-%d.lock", statDeviceNumber(stat.Dev), stat.Ino))
	assert.FileExists(t, want)

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is written into the user's folder")
}

// Validates: R-2.9.5
func TestEngineLocks_UnsupportedFilesystemWarnsAndContinues(t *testing.T) {
	t.Parallel()

	for _, errno := range []syscall.Errno{syscall.EBADF, syscall.ENOSYS, syscall.ENOLCK, syscall.EOPNOTSUPP} {
		t.Run(errno.Error(), func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			inputs := secondEngineInputs(t, t.TempDir(), logger)
			inputs.lockFlock = func(int, int) error { return errno }

			engine, err := newEngine(t.Context(), inputs)
			require.NoError(t, err, "a filesystem that cannot lock must not stop the sync")
			require.NoError(t, engine.Close(t.Context()))
			assert.Contains(t, buf.String(), "second sync from using this sync folder")
			assert.Contains(t, buf.String(), "second sync from using this sync state")
		})
	}
}

// lockFileName says which of the named lock files the descriptor fd has
// open, by device and inode.
func lockFileName(t *testing.T, fd int, names map[string]string) string {
	t.Helper()

	var open syscall.Stat_t
	require.NoError(t, syscall.Fstat(fd, &open))
	for name, path := range names {
		info, err := os.Stat(path)
		require.NoError(t, err)
		stat, ok := info.Sys().(*syscall.Stat_t)
		require.True(t, ok)
		if stat.Dev == open.Dev && stat.Ino == open.Ino {
			return name
		}
	}

	return "no lock of the engine"
}

// Both locks outlive the store, so the next owner never starts against a
// store this engine is still writing, and they go in reverse order of
// taking: the store lock, then the folder lock.
//
// Validates: R-2.9.5
func TestEngineLocks_ReleasedInReverseOnlyAfterTheStoreCloses(t *testing.T) {
	t.Parallel()

	inputs := secondEngineInputs(t, t.TempDir(), testLogger(t))
	rootLockPath, err := syncRootLockPath(inputs.DataDir, inputs.SyncRoot)
	require.NoError(t, err)
	storeLockPath, err := syncStoreLockPath(inputs.DataDir, inputs.DBPath)
	require.NoError(t, err)
	names := map[string]string{"folder": rootLockPath, "store": storeLockPath}

	var engine *Engine
	var unlocked []string
	var storeOpenAtUnlock []error
	inputs.lockFlock = func(fd int, how int) error {
		if how == syscall.LOCK_UN {
			unlocked = append(unlocked, lockFileName(t, fd, names))
			storeOpenAtUnlock = append(storeOpenAtUnlock, engine.baseline.rawDB().PingContext(context.Background()))
		}
		return syscall.Flock(fd, how)
	}

	engine, err = newEngine(t.Context(), inputs)
	require.NoError(t, err)
	require.NoError(t, engine.Close(t.Context()))

	assert.Equal(t, []string{"store", "folder"}, unlocked)
	for i, pingErr := range storeOpenAtUnlock {
		require.Error(t, pingErr, "the store must already be closed when the %s lock goes", unlocked[i])
	}
}

// Validates: R-2.9.5
func TestEngineLocks_ReleasedWhenStartupFailsAfterTakingThem(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	inputs := secondEngineInputs(t, syncRoot, testLogger(t))
	// A directory where the state DB should be: the store cannot open.
	require.NoError(t, os.MkdirAll(inputs.DBPath, 0o750))
	_, err := newEngine(t.Context(), inputs)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSyncRootInUse)
	require.NotErrorIs(t, err, ErrSyncStoreInUse)

	release, err := LockDriveForOfflineChange(t.Context(), inputs.DataDir, "", inputs.DBPath, testLogger(t))
	require.NoError(t, err, "the failed start released the store")
	require.NoError(t, release())

	retry := secondEngineInputs(t, syncRoot, testLogger(t))
	retry.DataDir = inputs.DataDir
	engine, err := newEngine(t.Context(), retry)
	require.NoError(t, err, "the failed start released the folder")
	require.NoError(t, engine.Close(t.Context()))
}

// writeStateFamily writes a stand-in for every file of the store at dbPath.
func writeStateFamily(t *testing.T, dbPath, content string) {
	t.Helper()

	for _, path := range config.StateDBFamily(dbPath) {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

// An email change renames a store with every file SQLite keeps for it: a
// log left behind under the old name would be lost, and a stale one at the
// new name replayed into the moved store.
//
// Validates: R-3.7.2
func TestMoveStateDBs_MovesTheWholeFamily(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	from := filepath.Join(dir, "state-old.db")
	to := filepath.Join(dir, "state-new.db")
	require.NoError(t, os.WriteFile(from, []byte("database"), 0o600))
	require.NoError(t, os.WriteFile(from+"-wal", []byte("log"), 0o600))
	require.NoError(t, os.WriteFile(to+"-journal", []byte("journal of a store long gone"), 0o600))

	require.NoError(t, MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{{From: from, To: to}}, testLogger(t)))

	for _, path := range config.StateDBFamily(from) {
		assert.NoFileExists(t, path)
	}
	moved, err := os.ReadFile(to) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Equal(t, "database", string(moved))
	movedLog, err := os.ReadFile(to + "-wal") //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Equal(t, "log", string(movedLog))
	assert.NoFileExists(t, to+"-journal", "a journal beside no database is not left for the moved store")
}

// A store an engine has open refuses the whole move: nothing is renamed, and
// the locks the move took are let go.
//
// Validates: R-3.7.2, R-6.3.9
func TestMoveStateDBs_RefusesEveryMoveWhileOneStoreIsInUse(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	dir := t.TempDir()
	moves := []config.StateDBMove{
		{From: filepath.Join(dir, "a-old.db"), To: filepath.Join(dir, "a-new.db")},
		{From: filepath.Join(dir, "b-old.db"), To: filepath.Join(dir, "b-new.db")},
	}
	for _, move := range moves {
		writeStateFamily(t, move.From, "kept")
	}
	release, err := LockDriveForOfflineChange(t.Context(), dataDir, "", moves[1].From, testLogger(t))
	require.NoError(t, err)

	err = MoveStateDBs(t.Context(), dataDir, moves, testLogger(t))
	require.ErrorIs(t, err, ErrSyncStoreInUse)
	for _, move := range moves {
		assert.FileExists(t, move.From)
		assert.NoFileExists(t, move.To)
	}
	require.NoError(t, release())

	again, err := LockDriveForOfflineChange(t.Context(), dataDir, "", moves[0].From, testLogger(t))
	require.NoError(t, err, "the refused move let go of the stores it had locked")
	require.NoError(t, again())
}

// Validates: R-3.7.2
func TestMoveStateDBs_RefusesATargetThatExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	move := config.StateDBMove{From: filepath.Join(dir, "old.db"), To: filepath.Join(dir, "new.db")}
	require.NoError(t, os.WriteFile(move.From, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(move.To, []byte("new"), 0o600))

	err := MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{move}, testLogger(t))
	require.ErrorContains(t, err, "already exists")
	assert.FileExists(t, move.From)
	kept, readErr := os.ReadFile(move.To)
	require.NoError(t, readErr)
	assert.Equal(t, "new", string(kept))
}

// A name with no database holds only leftovers of a deleted store (a log or
// journal that outlived its database). They are not a store, so nothing
// moves to the new name; and they are deleted, because after the change no
// account has the old email and no command would look there again.
//
// Validates: R-3.7.2
func TestMoveStateDBs_RemovesTheLeftoversOfAStoreWithoutItsDatabase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	move := config.StateDBMove{From: filepath.Join(dir, "old.db"), To: filepath.Join(dir, "new.db")}
	require.NoError(t, os.WriteFile(move.From+"-wal", []byte("log of a deleted store"), 0o600))
	require.NoError(t, os.WriteFile(move.From+"-journal", []byte("journal of a deleted store"), 0o600))

	require.NoError(t, MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{move}, testLogger(t)))
	for _, path := range config.StateDBFamily(move.From) {
		assert.NoFileExists(t, path)
	}
	for _, path := range config.StateDBFamily(move.To) {
		assert.NoFileExists(t, path)
	}
}

// The leftovers go only under the store lock, like every other change to a
// store's files: an engine starting on the old name creates its database
// while it holds that lock, and its fresh log must not be taken for a
// leftover.
//
// Validates: R-3.7.2, R-6.3.9
func TestMoveStateDBs_KeepsLeftoversWhileTheirStoreIsLocked(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	dir := t.TempDir()
	move := config.StateDBMove{From: filepath.Join(dir, "old.db"), To: filepath.Join(dir, "new.db")}
	require.NoError(t, os.WriteFile(move.From+"-wal", []byte("log"), 0o600))
	release, err := LockDriveForOfflineChange(t.Context(), dataDir, "", move.From, testLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, release()) })

	err = MoveStateDBs(t.Context(), dataDir, []config.StateDBMove{move}, testLogger(t))
	require.ErrorIs(t, err, ErrSyncStoreInUse)
	assert.FileExists(t, move.From+"-wal")
}

// A source that cannot be checked is an error, not "nothing to move": read as
// absent, the email change would go on to rename the drive's config away
// from a store it never moved.
//
// Validates: R-3.7.2
func TestMoveStateDBs_ASourceThatCannotBeCheckedIsAnError(t *testing.T) {
	t.Parallel()

	locked := filepath.Join(t.TempDir(), "unreadable")
	require.NoError(t, os.Mkdir(locked, 0o700))
	move := config.StateDBMove{From: filepath.Join(locked, "old.db"), To: filepath.Join(t.TempDir(), "new.db")}
	require.NoError(t, os.WriteFile(move.From, []byte("old"), 0o600))
	require.NoError(t, os.Chmod(locked, 0))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(locked, 0o700)) }) //nolint:gosec // restore a test folder for cleanup

	err := MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{move}, testLogger(t))
	require.ErrorIs(t, err, os.ErrPermission)
	assert.NoFileExists(t, move.To)
}

// Validates: R-2.9.6
func TestWarnAboutForeignSyncClients(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".stfolder"), 0o750))
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	warnAboutForeignSyncClients(root, logger)
	assert.Contains(t, buf.String(), "another sync client (.stfolder)")

	buf.Reset()
	warnAboutForeignSyncClients(t.TempDir(), logger)
	assert.Empty(t, buf.String(), "a plain folder is not reported")

	buf.Reset()
	warnAboutForeignSyncClients("/Users/u/Library/CloudStorage/Dropbox/OneDrive", logger)
	assert.Contains(t, buf.String(), "~/Library/CloudStorage")
}
