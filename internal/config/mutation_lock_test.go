package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// holdDirectoryLock takes the lock on dir the way another onedrive-go process
// would. flock locks belong to an open file description, not to a process, so
// this second open of the directory contends with the code under test exactly
// as a separate process does, without spawning one. The returned release is
// idempotent and also runs at cleanup.
func holdDirectoryLock(t *testing.T, dir string) func() {
	t.Helper()

	holder, err := localpath.Open(dir)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))

	release := sync.OnceFunc(func() {
		assert.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_UN))
		assert.NoError(t, holder.Close())
	})
	t.Cleanup(release)

	return release
}

// errBlockingFlock is what the fake flock below answers to a blocking request,
// which the bounded wait must never make.
var errBlockingFlock = errors.New("test: blocking flock requested")

// Validates: R-4.4.4
func TestWithConfigMutationLock_HeldLockTimesOutWithTheDirectoryNamed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	attempts := 0
	waits := 0
	var waited time.Duration
	locker := defaultConfigLocker()
	locker.flock = func(_ int, how int) error {
		if how != syscall.LOCK_EX|syscall.LOCK_NB {
			return errBlockingFlock
		}
		attempts++
		return syscall.EWOULDBLOCK
	}
	locker.sleep = func(_ context.Context, d time.Duration) error {
		waits++
		waited += d
		return nil
	}

	ran := false
	err := locker.withMutationLock(t.Context(), path, false, func() error {
		ran = true
		return nil
	})

	require.ErrorIs(t, err, ErrConfigLocked)
	assert.Contains(t, err.Error(), dir)
	assert.Contains(t, err.Error(), configLockTimeout.String())
	assert.False(t, ran, "the change must not run without the lock")
	assert.Equal(t, configLockTimeout, waited)
	assert.Equal(t, int(configLockTimeout/configLockPollInterval), waits)
	assert.Equal(t, waits+1, attempts, "one attempt before each wait and one after the last")
}

// Validates: R-4.4.4
func TestWithConfigMutationLock_CanceledContextEndsTheWait(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	holdDirectoryLock(t, dir)

	// The context ends during the first wait, as a Ctrl-C would; the real
	// pause between attempts must then return at once.
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	locker := defaultConfigLocker()
	locker.flock = func(fd int, how int) error {
		attempts++
		cancel()
		return syscall.Flock(fd, how)
	}

	ran := false
	err := locker.withMutationLock(ctx, path, false, func() error {
		ran = true
		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrConfigLocked, "the wait ended on the context, not on the timeout")
	assert.Contains(t, err.Error(), dir)
	assert.False(t, ran, "the change must not run without the lock")
	assert.Equal(t, 1, attempts, "no attempt after the context ended")
}

// Validates: R-4.4.4
func TestWithConfigMutationLock_EndedContextMakesNoAttempt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	locker := defaultConfigLocker()
	locker.flock = func(int, int) error {
		return errBlockingFlock
	}

	ran := false
	err := locker.withMutationLock(ctx, path, false, func() error {
		ran = true
		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, ran)
}

// Validates: R-4.4.4
func TestWithConfigMutationLock_AcquiresWhenTheHolderReleases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	releaseHolder := holdDirectoryLock(t, dir)

	const waitsBeforeRelease = 2
	waits := 0
	locker := defaultConfigLocker()
	locker.sleep = func(context.Context, time.Duration) error {
		waits++
		if waits == waitsBeforeRelease {
			releaseHolder()
		}
		return nil
	}

	runs := 0
	require.NoError(t, locker.withMutationLock(t.Context(), path, false, func() error {
		runs++
		return nil
	}))

	assert.Equal(t, 1, runs)
	assert.Equal(t, waitsBeforeRelease, waits)

	// The change released the lock again, so the next holder gets it at once.
	holdDirectoryLock(t, dir)()
}

// Validates: R-4.4.4
func TestSetDriveKey_CrossProcessHolderIsNotWaitedForForever(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:lock-holder@example.com")
	require.NoError(t, AppendDriveSection(t.Context(), path, cid, "~/OneDrive"))
	before, err := localpath.ReadFile(path)
	require.NoError(t, err)

	holdDirectoryLock(t, dir)

	var waited time.Duration
	locker := defaultConfigLocker()
	locker.sleep = func(_ context.Context, d time.Duration) error {
		waited += d
		return nil
	}

	err = locker.setDriveKey(t.Context(), path, cid, "paused", "true")

	require.ErrorIs(t, err, ErrConfigLocked)
	assert.Contains(t, err.Error(), dir)
	assert.Equal(t, configLockTimeout, waited)

	after, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a timed-out change must leave the file as it was")
}

// Validates: R-4.4.4
func TestUpdateCatalog_CrossProcessUpdatesAreNotLost(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	holderAccount := CatalogAccount{
		CanonicalID: "personal:holder@example.com",
		Email:       "holder@example.com",
		DriveType:   driveTypePersonal,
	}
	waiterAccount := CatalogAccount{
		CanonicalID: "personal:waiter@example.com",
		Email:       "waiter@example.com",
		DriveType:   driveTypePersonal,
	}

	// The test plays the other process: it holds the lock and has read the
	// catalog, halfway through its own read-modify-write.
	releaseHolder := holdDirectoryLock(t, dataDir)
	holderView, err := LoadCatalogForDataDir(dataDir)
	require.NoError(t, err)

	waiting := make(chan struct{})
	resume := make(chan struct{})
	resumeWaiter := sync.OnceFunc(func() { close(resume) })
	var firstWait sync.Once
	locker := defaultConfigLocker()
	locker.sleep = func(context.Context, time.Duration) error {
		firstWait.Do(func() {
			close(waiting)
			<-resume
		})
		return nil
	}

	var updateErr error
	updateReturned := make(chan struct{})
	var updater sync.WaitGroup
	updater.Go(func() {
		defer close(updateReturned)
		updateErr = locker.updateCatalog(t.Context(), dataDir, func(catalog *Catalog) error {
			catalog.UpsertAccount(&waiterAccount)
			return nil
		})
	})
	t.Cleanup(func() {
		resumeWaiter()
		updater.Wait()
	})

	// The update is now either waiting for the lock or done without it.
	select {
	case <-waiting:
	case <-updateReturned:
	}

	holderView.UpsertAccount(&holderAccount)
	require.NoError(t, saveCatalogForDataDir(dataDir, holderView))
	releaseHolder()
	resumeWaiter()
	updater.Wait()
	require.NoError(t, updateErr)

	final, err := LoadCatalogForDataDir(dataDir)
	require.NoError(t, err)
	assert.Contains(t, final.Accounts, holderAccount.CanonicalID, "the holder's update was lost")
	assert.Contains(t, final.Accounts, waiterAccount.CanonicalID, "the waiting update was lost")
}

// A filesystem that cannot lock at all (flock answers ENOLCK, EOPNOTSUPP or
// ENOSYS on some NFS and FUSE mounts) refuses the change at once, naming the
// directory and the filesystem's limit: the config and the catalog are never
// written without the lock, so no unlocked write can lose another process's
// change. Neither file is touched and nothing waits.
//
// Validates: R-4.4.4
func TestWithConfigMutationLock_AFilesystemWithoutLocksRefusesTheChange(t *testing.T) {
	t.Parallel()

	for _, errno := range []syscall.Errno{syscall.ENOLCK, syscall.EOPNOTSUPP, syscall.ENOSYS} {
		t.Run(errno.Error(), func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.toml")
			const before = "# kept as it was\n"
			require.NoError(t, os.WriteFile(cfgPath, []byte(before), 0o600))

			attempts := 0
			locker := defaultConfigLocker()
			locker.flock = func(int, int) error {
				attempts++
				return errno
			}
			locker.sleep = func(context.Context, time.Duration) error {
				assert.Fail(t, "a filesystem that cannot lock is not waited for")
				return nil
			}

			cid := driveid.MustCanonicalID("personal:nolock@example.com")
			err := locker.setDriveKey(t.Context(), cfgPath, cid, "paused", "true")
			require.ErrorIs(t, err, ErrConfigLockUnsupported)
			require.ErrorIs(t, err, errno, "flock's answer is kept")
			require.NotErrorIs(t, err, ErrConfigLocked)
			assert.Contains(t, err.Error(), dir)
			assert.Contains(t, err.Error(), "does not support file locks")
			assert.Contains(t, err.Error(), "local filesystem")
			assert.Equal(t, 1, attempts)
			written, readErr := os.ReadFile(cfgPath) //nolint:gosec // the test's own temp file
			require.NoError(t, readErr)
			assert.Equal(t, before, string(written), "the config is not written without the lock")

			catalogRan := false
			err = locker.updateCatalog(t.Context(), dir, func(*Catalog) error {
				catalogRan = true
				return nil
			})
			require.ErrorIs(t, err, ErrConfigLockUnsupported)
			assert.False(t, catalogRan, "the catalog is not changed without the lock")
			assert.NoFileExists(t, CatalogPathForDataDir(dir))
		})
	}
}
