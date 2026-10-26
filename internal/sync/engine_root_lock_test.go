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
func TestSyncRootLock_UnsupportedFilesystemWarnsAndContinues(t *testing.T) {
	t.Parallel()

	for _, errno := range []syscall.Errno{syscall.EBADF, syscall.ENOSYS, syscall.ENOLCK, syscall.EOPNOTSUPP} {
		t.Run(errno.Error(), func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			inputs := secondEngineInputs(t, t.TempDir(), logger)
			inputs.rootLockFlock = func(int, int) error { return errno }

			engine, err := newEngine(t.Context(), inputs)
			require.NoError(t, err, "a filesystem that cannot lock must not stop the sync")
			require.NoError(t, engine.Close(t.Context()))
			assert.Contains(t, buf.String(), "cannot lock its data folder")
		})
	}
}

// Validates: R-2.9.5
func TestSyncRootLock_ReleasedOnlyAfterTheStoreCloses(t *testing.T) {
	t.Parallel()

	inputs := secondEngineInputs(t, t.TempDir(), testLogger(t))
	var engine *Engine
	var storeClosedAtUnlock error
	unlocked := false
	inputs.rootLockFlock = func(fd int, how int) error {
		if how == syscall.LOCK_UN {
			unlocked = true
			storeClosedAtUnlock = engine.baseline.rawDB().PingContext(context.Background())
		}
		return syscall.Flock(fd, how)
	}

	var err error
	engine, err = newEngine(t.Context(), inputs)
	require.NoError(t, err)
	require.NoError(t, engine.Close(t.Context()))

	require.True(t, unlocked)
	require.Error(t, storeClosedAtUnlock, "the store must already be closed when the folder is unlocked")
}

// Validates: R-2.9.5
func TestSyncRootLock_ReleasedWhenStartupFailsAfterTakingIt(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	inputs := secondEngineInputs(t, syncRoot, testLogger(t))
	// A directory where the state DB should be: the store cannot open.
	require.NoError(t, os.MkdirAll(inputs.DBPath, 0o750))
	_, err := newEngine(t.Context(), inputs)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSyncRootInUse)

	retry := secondEngineInputs(t, syncRoot, testLogger(t))
	retry.DataDir = inputs.DataDir
	engine, err := newEngine(t.Context(), retry)
	require.NoError(t, err, "the failed start released the folder")
	require.NoError(t, engine.Close(t.Context()))
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
