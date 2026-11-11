package config

import (
	"errors"
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
	locker.sleep = func(d time.Duration) {
		waits++
		waited += d
	}

	ran := false
	err := locker.withMutationLock(path, false, func() error {
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
func TestWithConfigMutationLock_AcquiresWhenTheHolderReleases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	releaseHolder := holdDirectoryLock(t, dir)

	const waitsBeforeRelease = 2
	waits := 0
	locker := defaultConfigLocker()
	locker.sleep = func(time.Duration) {
		waits++
		if waits == waitsBeforeRelease {
			releaseHolder()
		}
	}

	runs := 0
	require.NoError(t, locker.withMutationLock(path, false, func() error {
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
	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))
	before, err := localpath.ReadFile(path)
	require.NoError(t, err)

	holdDirectoryLock(t, dir)

	var waited time.Duration
	locker := defaultConfigLocker()
	locker.sleep = func(d time.Duration) {
		waited += d
	}

	err = locker.setDriveKey(path, cid, "paused", "true")

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
	locker.sleep = func(time.Duration) {
		firstWait.Do(func() {
			close(waiting)
			<-resume
		})
	}

	var updateErr error
	updateReturned := make(chan struct{})
	var updater sync.WaitGroup
	updater.Go(func() {
		defer close(updateReturned)
		updateErr = locker.updateCatalog(dataDir, func(catalog *Catalog) error {
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
	require.NoError(t, SaveCatalogForDataDir(dataDir, holderView))
	releaseHolder()
	resumeWaiter()
	updater.Wait()
	require.NoError(t, updateErr)

	final, err := LoadCatalogForDataDir(dataDir)
	require.NoError(t, err)
	assert.Contains(t, final.Accounts, holderAccount.CanonicalID, "the holder's update was lost")
	assert.Contains(t, final.Accounts, waiterAccount.CanonicalID, "the waiting update was lost")
}
