package syncobserve

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// D-079: kqueue descriptor budget
// ---------------------------------------------------------------------------

// Validates: R-2.8.18
func TestAddWatchesRecursive_OverDescriptorBudgetFallsBackBeforeExhaustingFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o700))
	for i := range 6 {
		syncfixture.WriteTestFile(t, root, fmt.Sprintf("sub/f%d.txt", i), "x")
	}

	watcher := syncfixture.NewRecordingFsWatcher()
	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	// kqueue would hold root + sub + six files = eight descriptors.
	obs.descriptorBudget = 5

	err := obs.AddWatchesRecursive(t.Context(), watcher, syncfixture.MustOpenSyncTree(t, root))

	require.ErrorIs(t, err, errLocalWatchUnavailable)
	cause, ok := WatchUnavailableCauseOf(err)
	require.True(t, ok)
	assert.Equal(t, watchCauseDescriptorBudget, cause)
	assert.Empty(t, obs.watchedDirs.paths(), "watches added before the budget was exceeded are rolled back")
}

// Validates: R-2.8.18
func TestAddWatchesRecursive_WithinDescriptorBudgetWatches(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	syncfixture.WriteTestFile(t, root, "sub/a.txt", "x")

	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.descriptorBudget = 100

	require.NoError(t, obs.AddWatchesRecursive(t.Context(), syncfixture.NewRecordingFsWatcher(), syncfixture.MustOpenSyncTree(t, root)))
	assert.Contains(t, obs.watchedDirs.paths(), filepath.Join(root, "sub"))
}

// ---------------------------------------------------------------------------
// D-081c: network and FUSE sync roots
// ---------------------------------------------------------------------------

// Validates: R-2.8.19
func TestWatch_NetworkRootShortensSafetyScanInterval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	watcher := newMockFsWatcher()
	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	clock := syncfixture.NewManualClock(time.Now())
	obs.clock = clock
	obs.hooks.WatcherFactory = func() (FsWatcher, error) { return watcher, nil }
	obs.safetyScanInterval = LocalWatchHealthySafetyScanInterval
	obs.filesystemKind = func(string) (filesystemInfo, error) {
		return filesystemInfo{kind: "cifs", remote: true}, nil
	}

	cancel, done := startMockWatch(t, obs, watcher, dir)

	require.Eventually(t, func() bool { return manualTickerIntervals(clock) != nil }, syncfixture.EventWait, time.Millisecond)
	assert.Equal(t, []time.Duration{networkSafetyScanInterval}, manualTickerIntervals(clock),
		"changes made on other machines reach only the rescan, so it runs more often")
	syncfixture.StopWatch(t, cancel, done)
}

// Validates: R-2.8.19
func TestAdaptToRootFilesystem_LocalRootKeepsInterval(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.safetyScanInterval = LocalWatchHealthySafetyScanInterval
	obs.filesystemKind = func(string) (filesystemInfo, error) {
		return filesystemInfo{kind: "ext4"}, nil
	}

	obs.adaptToRootFilesystem(t.TempDir())

	assert.Equal(t, LocalWatchHealthySafetyScanInterval, obs.safetyScanInterval)
}

// Validates: R-2.8.19
func TestLocalFilesystemKind_ReadsTheRealFilesystem(t *testing.T) {
	t.Parallel()

	info, err := localFilesystemKind(t.TempDir())
	require.NoError(t, err)
	assert.NotEmpty(t, info.kind)

	_, err = localFilesystemKind(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func manualTickerIntervals(clock *syncfixture.ManualClock) []time.Duration {
	clock.Mu.Lock()
	defer clock.Mu.Unlock()

	var intervals []time.Duration
	for _, ticker := range clock.Ticks {
		intervals = append(intervals, ticker.Interval)
	}

	return intervals
}

// Validates: R-2.8.19
func TestAdaptToRootFilesystem_NonDefaultIntervalIsNeverOverridden(t *testing.T) {
	t.Parallel()

	for _, chosen := range []time.Duration{time.Minute, 10 * time.Minute} {
		obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
		obs.safetyScanInterval = chosen
		obs.filesystemKind = func(string) (filesystemInfo, error) {
			return filesystemInfo{kind: "nfs", remote: true}, nil
		}

		obs.adaptToRootFilesystem(t.TempDir())

		assert.Equal(t, chosen, obs.safetyScanInterval, "only the default interval is shortened")
	}
}

// Validates: R-2.8.18
func TestAddWatchesRecursive_DescriptorProjectionCountsSubdirectoriesOnce(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for i := range 6 {
		syncfixture.WriteTestFile(t, root, fmt.Sprintf("sub/f%d.txt", i), "x")
	}

	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	// root + sub + six files: kqueue holds exactly eight descriptors (the
	// watch on sub reuses the descriptor of sub's entry in root).
	obs.descriptorBudget = 8

	require.NoError(t, obs.AddWatchesRecursive(t.Context(), syncfixture.NewRecordingFsWatcher(), syncfixture.MustOpenSyncTree(t, root)),
		"a tree that fits the budget exactly is watched")
}
