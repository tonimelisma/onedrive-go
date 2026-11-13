package sync

import (
	"bytes"
	"context"
	"errors"
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

// recordNothing stands in for the journal write of a move that starts now: a
// move given a record callback is a new one, and one given none is the rest
// of a move a journal already names.
func recordNothing() error { return nil }

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

	require.NoError(t, MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{{From: from, To: to}}, recordNothing, testLogger(t)))

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

	recorded := 0
	err = MoveStateDBs(t.Context(), dataDir, moves, func() error { recorded++; return nil }, testLogger(t))
	require.ErrorIs(t, err, ErrSyncStoreInUse)
	assert.Zero(t, recorded, "a refused move is never put on record")
	for _, move := range moves {
		assert.FileExists(t, move.From)
		assert.NoFileExists(t, move.To)
	}
	require.NoError(t, release())

	again, err := LockDriveForOfflineChange(t.Context(), dataDir, "", moves[0].From, testLogger(t))
	require.NoError(t, err, "the refused move let go of the stores it had locked")
	require.NoError(t, again())
}

// A database at the new name before the change starts is somebody's store:
// the move is refused before it is put on record or renames anything, and
// the error names the file to remove.
//
// Validates: R-3.7.2, R-3.7.3
func TestMoveStateDBs_RefusesATargetThatExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	free := config.StateDBMove{From: filepath.Join(dir, "free-old.db"), To: filepath.Join(dir, "free-new.db")}
	move := config.StateDBMove{From: filepath.Join(dir, "old.db"), To: filepath.Join(dir, "new.db")}
	require.NoError(t, os.WriteFile(free.From, []byte("free"), 0o600))
	require.NoError(t, os.WriteFile(move.From, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(move.To, []byte("new"), 0o600))

	recorded := 0
	err := MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{free, move},
		func() error { recorded++; return nil }, testLogger(t))
	require.ErrorContains(t, err, "already exists")
	require.ErrorContains(t, err, "remove "+move.To)
	assert.Zero(t, recorded, "a refused move is never put on record")
	assert.FileExists(t, free.From, "no store moves when one of them cannot")
	assert.NoFileExists(t, free.To)
	assert.FileExists(t, move.From)
	kept, readErr := os.ReadFile(move.To)
	require.NoError(t, readErr)
	assert.Equal(t, "new", string(kept))
}

// A move that starts now is put on record exactly once, at the one moment
// that makes the record true: the move holds every store, nothing stands at
// a new name, and nothing has been renamed. Sidecars a store long gone left
// at a new name are cleared before that moment, because from then on whatever
// stands at a new name is taken for part of this move.
//
// Validates: R-3.7.3
func TestMoveStateDBs_RecordsTheChangeOnceBeforeTheFirstRename(t *testing.T) {
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
	require.NoError(t, os.WriteFile(moves[1].To+"-journal", []byte("journal of a store long gone"), 0o600))

	recorded := 0
	record := func() error {
		recorded++
		for _, move := range moves {
			assert.FileExists(t, move.From, "nothing is renamed before the change is on record")
			for _, path := range config.StateDBFamily(move.To) {
				assert.NoFileExists(t, path, "nothing stands at a new name when the change goes on record")
			}
			_, lockErr := LockDriveForOfflineChange(t.Context(), dataDir, "", move.To, testLogger(t))
			require.ErrorIs(t, lockErr, ErrSyncStoreInUse, "the move holds every store when it goes on record")
		}
		return nil
	}

	require.NoError(t, MoveStateDBs(t.Context(), dataDir, moves, record, testLogger(t)))
	assert.Equal(t, 1, recorded)
	for _, move := range moves {
		for i, path := range config.StateDBFamily(move.To) {
			assert.FileExists(t, path)
			assert.NoFileExists(t, config.StateDBFamily(move.From)[i])
		}
	}
}

// A change that cannot be put on record renames nothing: a rename with no
// record is what an interruption could not be recovered from.
//
// Validates: R-3.7.3
func TestMoveStateDBs_AChangeThatCannotBeRecordedRenamesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	move := config.StateDBMove{From: filepath.Join(dir, "old.db"), To: filepath.Join(dir, "new.db")}
	writeStateFamily(t, move.From, "kept")
	failed := errors.New("no room for the journal")

	err := MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{move}, func() error { return failed }, testLogger(t))
	require.ErrorIs(t, err, failed)
	for i, path := range config.StateDBFamily(move.From) {
		assert.FileExists(t, path)
		assert.NoFileExists(t, config.StateDBFamily(move.To)[i])
	}
}

// A store's log holds commits its database file does not have yet. Before a
// store moves its log is folded into the database, so what is renamed is one
// file and no interruption between two renames can part the two.
//
// Validates: R-3.7.3, R-2.1.3
func TestMoveStateDBs_FoldsTheLogIntoTheDatabaseBeforeItMoves(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	donorPath := walDonor(t, dir)
	move := config.StateDBMove{From: filepath.Join(dir, "old.db"), To: filepath.Join(dir, "new.db")}
	copyStateFile(t, donorPath, move.From)
	copyStateFile(t, donorPath+"-wal", move.From+"-wal")

	require.NoError(t, MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{move}, recordNothing, testLogger(t)))

	for _, path := range config.StateDBFamily(move.From) {
		assert.NoFileExists(t, path)
	}
	assert.NoFileExists(t, move.To+"-wal", "the log was folded into the database, so one file moved")
	assert.True(t, sqliteHasTable(t, openPlainSQLite(t, move.To), staleMarkerTable),
		"the state committed to the log is in the moved database")
}

// A move a journal already names is finished, never started over: what
// stands at a new name was put there by the interrupted run, so it is kept,
// whether that is a log whose database has not followed yet or a whole store.
//
// Validates: R-3.7.3, R-2.1.3
func TestMoveStateDBs_AResumedMoveKeepsWhatItAlreadyMoved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	donorPath := walDonor(t, dir)
	logMoved := config.StateDBMove{From: filepath.Join(dir, "log-old.db"), To: filepath.Join(dir, "log-new.db")}
	copyStateFile(t, donorPath, logMoved.From)
	copyStateFile(t, donorPath+"-wal", logMoved.To+"-wal")
	allMoved := config.StateDBMove{From: filepath.Join(dir, "all-old.db"), To: filepath.Join(dir, "all-new.db")}
	writeStateFamily(t, allMoved.To, "moved before the interruption")
	neverThere := config.StateDBMove{From: filepath.Join(dir, "none-old.db"), To: filepath.Join(dir, "none-new.db")}

	require.NoError(t, MoveStateDBs(t.Context(), t.TempDir(),
		[]config.StateDBMove{logMoved, allMoved, neverThere}, nil, testLogger(t)))

	assert.NoFileExists(t, logMoved.From)
	assert.True(t, sqliteHasTable(t, openPlainSQLite(t, logMoved.To), staleMarkerTable),
		"the state committed to the log must survive the resumed move")
	for _, path := range config.StateDBFamily(allMoved.To) {
		content, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
		require.NoError(t, err)
		assert.Equal(t, "moved before the interruption", string(content))
	}
	for _, path := range config.StateDBFamily(neverThere.To) {
		assert.NoFileExists(t, path)
	}
}

// A database at both names when a move is resumed: the journal says this
// move put the one at the new name there, so the one at the old name was
// created afterwards. Neither is touched, and the error says which to remove
// and why.
//
// Validates: R-3.7.3
func TestMoveStateDBs_AResumedMoveWithADatabaseAtBothNamesSaysWhichToRemove(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	move := config.StateDBMove{From: filepath.Join(dir, "old.db"), To: filepath.Join(dir, "new.db")}
	require.NoError(t, os.WriteFile(move.From, []byte("created after the change"), 0o600))
	require.NoError(t, os.WriteFile(move.To, []byte("moved"), 0o600))

	err := MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{move}, nil, testLogger(t))
	require.ErrorContains(t, err, "remove "+move.From)
	require.ErrorContains(t, err, "after the account's email changed")
	created, readErr := os.ReadFile(move.From)
	require.NoError(t, readErr)
	assert.Equal(t, "created after the change", string(created))
	moved, readErr := os.ReadFile(move.To)
	require.NoError(t, readErr)
	assert.Equal(t, "moved", string(moved))
}

// writeUnfinishedEmailChange leaves in dataDir the journal of an account
// email change cut short, as internal/config records one, naming one state
// store by its old and its new file name. It returns the journal's path.
func writeUnfinishedEmailChange(t *testing.T, dataDir, oldName, newName string) string {
	t.Helper()

	journalPath := filepath.Join(dataDir, "email-change", "journal.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(journalPath), 0o700))
	journal := fmt.Sprintf(`{
  "schema_version": 1,
  "config_path": %q,
  "account_renames": [{"from": "business:user@example.com", "to": "business:renamed@example.com"}],
  "drive_renames": [{"from": "business:user@example.com", "to": "business:renamed@example.com"}],
  "state_db_moves": [{"from": %q, "to": %q}],
  "token_moves": []
}`, filepath.Join(dataDir, "config.toml"), oldName, newName)
	require.NoError(t, os.WriteFile(journalPath, []byte(journal), 0o600))

	return journalPath
}

// emailChangeEngineInputs are the inputs of an engine on the state store
// called name in dataDir.
func emailChangeEngineInputs(t *testing.T, dataDir, name string) *engineInputs {
	t.Helper()

	inputs := secondEngineInputs(t, t.TempDir(), testLogger(t))
	inputs.DataDir = dataDir
	inputs.DBPath = filepath.Join(dataDir, name)

	return inputs
}

// An account email change cut short leaves a journal that names every state
// store it moves. Until a command finishes the change such a store may be at
// its old name, at its new one, or split between them, so an engine refuses
// to start on either name. It never finishes the change itself and never
// waits for it. A drive the journal does not name starts as usual.
//
// Validates: R-3.7.3, R-2.9.5
func TestEngine_RefusesToStartWhileAnEmailChangeIsUnfinished(t *testing.T) {
	t.Parallel()

	const (
		oldName = "state_business_user@example.com.db"
		newName = "state_business_renamed@example.com.db"
	)
	dataDir := t.TempDir()
	journalPath := writeUnfinishedEmailChange(t, dataDir, oldName, newName)

	for _, name := range []string{oldName, newName} {
		inputs := emailChangeEngineInputs(t, dataDir, name)
		_, err := newEngine(t.Context(), inputs)
		require.ErrorIs(t, err, ErrEmailChangeUnfinished, name)
		assert.NoFileExists(t, inputs.DBPath, "the refused engine never created a state store")
		release, lockErr := LockDriveForOfflineChange(t.Context(), dataDir, inputs.SyncRoot, inputs.DBPath, testLogger(t))
		require.NoError(t, lockErr, "the refused engine let go of its locks")
		require.NoError(t, release())
	}

	other, err := newEngine(t.Context(), emailChangeEngineInputs(t, dataDir, "state_personal_user@example.com.db"))
	require.NoError(t, err, "a drive the change does not name starts")
	require.NoError(t, other.Close(t.Context()))

	require.NoError(t, os.Remove(journalPath))
	renamed, err := newEngine(t.Context(), emailChangeEngineInputs(t, dataDir, newName))
	require.NoError(t, err, "the drive starts once the change is finished")
	require.NoError(t, renamed.Close(t.Context()))
}

// A journal that cannot be read may name any store, so no engine in that
// data directory starts, and the error names the file.
//
// Validates: R-3.7.3
func TestEngine_RefusesToStartWhileTheEmailChangeJournalCannotBeRead(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	journalPath := filepath.Join(dataDir, "email-change", "journal.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(journalPath), 0o700))
	require.NoError(t, os.WriteFile(journalPath, []byte("{not a journal"), 0o600))

	inputs := emailChangeEngineInputs(t, dataDir, "state_personal_user@example.com.db")
	_, err := newEngine(t.Context(), inputs)
	require.ErrorContains(t, err, journalPath)
	assert.NoFileExists(t, inputs.DBPath, "the refused engine never created a state store")
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

	require.NoError(t, MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{move}, recordNothing, testLogger(t)))
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

	err = MoveStateDBs(t.Context(), dataDir, []config.StateDBMove{move}, recordNothing, testLogger(t))
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

	err := MoveStateDBs(t.Context(), t.TempDir(), []config.StateDBMove{move}, recordNothing, testLogger(t))
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
