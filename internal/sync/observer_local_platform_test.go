package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		writeTestFile(t, root, fmt.Sprintf("sub/f%d.txt", i), "x")
	}

	watcher := newRecordingFsWatcher()
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	// kqueue would hold root + sub + six files = eight descriptors.
	obs.descriptorBudget = 5

	err := obs.AddWatchesRecursive(t.Context(), watcher, mustOpenSyncTree(t, root))

	require.ErrorIs(t, err, errLocalWatchUnavailable)
	cause, ok := watchUnavailableCauseOf(err)
	require.True(t, ok)
	assert.Equal(t, watchCauseDescriptorBudget, cause)
	assert.Empty(t, obs.watchedDirs, "watches added before the budget was exceeded are rolled back")
}

// Validates: R-2.8.18
func TestAddWatchesRecursive_WithinDescriptorBudgetWatches(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, root, "sub/a.txt", "x")

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.descriptorBudget = 100

	require.NoError(t, obs.AddWatchesRecursive(t.Context(), newRecordingFsWatcher(), mustOpenSyncTree(t, root)))
	assert.Contains(t, obs.watchedDirs, filepath.Join(root, "sub"))
}

// ---------------------------------------------------------------------------
// D-081c: network and FUSE sync roots
// ---------------------------------------------------------------------------

// Validates: R-2.8.19
func TestWatch_NetworkRootShortensSafetyScanInterval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	watcher := newMockFsWatcher()
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	clock := newManualClock(time.Now())
	obs.clock = clock
	obs.watcherFactory = func() (fsWatcher, error) { return watcher, nil }
	obs.safetyScanInterval = localWatchHealthySafetyScanInterval
	obs.filesystemKind = func(string) (filesystemInfo, error) {
		return filesystemInfo{kind: "cifs", remote: true}, nil
	}

	cancel, done := startMockWatch(t, obs, watcher, dir, make(chan changeEvent, 10))
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool { return manualTickerIntervals(clock) != nil }, 5*time.Second, time.Millisecond)
	assert.Equal(t, []time.Duration{networkSafetyScanInterval}, manualTickerIntervals(clock),
		"changes made on other machines reach only the rescan, so it runs more often")
}

// Validates: R-2.8.19
func TestAdaptToRootFilesystem_LocalRootKeepsInterval(t *testing.T) {
	t.Parallel()

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.safetyScanInterval = localWatchHealthySafetyScanInterval
	obs.filesystemKind = func(string) (filesystemInfo, error) {
		return filesystemInfo{kind: "ext4"}, nil
	}

	obs.adaptToRootFilesystem(t.TempDir())

	assert.Equal(t, localWatchHealthySafetyScanInterval, obs.safetyScanInterval)
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

func manualTickerIntervals(clock *manualClock) []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	var intervals []time.Duration
	for _, ticker := range clock.ticks {
		intervals = append(intervals, ticker.interval)
	}

	return intervals
}
