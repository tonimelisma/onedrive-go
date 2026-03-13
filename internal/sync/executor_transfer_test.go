package sync

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// checkDiskSpace tests (R-2.10.43, R-2.10.44, R-6.2.6, R-6.4.7)
//
// Tests mutate the package-level diskAvailableFunc, so they run sequentially
// within a parent test (no t.Parallel on the subtests).
// ---------------------------------------------------------------------------

// Validates: R-2.10.43, R-2.10.44, R-6.2.6, R-6.4.7
func TestCheckDiskSpace(t *testing.T) {
	// No t.Parallel() — subtests mutate package-level diskAvailableFunc.

	// Validates: R-2.10.43
	t.Run("DiskFull", func(t *testing.T) {
		orig := diskAvailableFunc
		diskAvailableFunc = func(string) (uint64, error) { return 500, nil }
		t.Cleanup(func() { diskAvailableFunc = orig })

		exec := &Executor{
			ExecutorConfig: &ExecutorConfig{
				minFreeSpace: 1000,
				syncRoot:     t.TempDir(),
				driveID:      driveid.New("d"),
				logger:       testLogger(t),
			},
		}

		action := &Action{
			Type:    ActionDownload,
			Path:    "test.txt",
			DriveID: driveid.New("d"),
			ItemID:  "item1",
		}

		outcome, blocked := exec.checkDiskSpace(action)
		require.True(t, blocked, "should block when available < minFreeSpace")
		assert.False(t, outcome.Success)
		assert.ErrorIs(t, outcome.Error, ErrDiskFull)
	})

	// Validates: R-2.10.44
	t.Run("FileTooLarge", func(t *testing.T) {
		// available (2000) >= minFreeSpace (1000) but < fileSize (1500) + minFreeSpace (1000)
		orig := diskAvailableFunc
		diskAvailableFunc = func(string) (uint64, error) { return 2000, nil }
		t.Cleanup(func() { diskAvailableFunc = orig })

		exec := &Executor{
			ExecutorConfig: &ExecutorConfig{
				minFreeSpace: 1000,
				syncRoot:     t.TempDir(),
				driveID:      driveid.New("d"),
				logger:       testLogger(t),
			},
		}

		action := &Action{
			Type:    ActionDownload,
			Path:    "large.bin",
			DriveID: driveid.New("d"),
			ItemID:  "item2",
			View: &PathView{
				Remote: &RemoteState{Size: 1500},
			},
		}

		outcome, blocked := exec.checkDiskSpace(action)
		require.True(t, blocked, "should block when file + minFreeSpace > available")
		assert.False(t, outcome.Success)
		assert.ErrorIs(t, outcome.Error, ErrFileTooLargeForSpace)
	})

	// Validates: R-6.4.7
	t.Run("Disabled", func(t *testing.T) {
		// With minFreeSpace = 0, the guard in executeDownload prevents
		// checkDiskSpace from being called. If called directly, minFreeSpace=0
		// means uint64(0), so any available space passes.
		orig := diskAvailableFunc
		diskAvailableFunc = func(string) (uint64, error) { return 100, nil }
		t.Cleanup(func() { diskAvailableFunc = orig })

		exec := &Executor{
			ExecutorConfig: &ExecutorConfig{
				minFreeSpace: 0,
				syncRoot:     t.TempDir(),
				driveID:      driveid.New("d"),
				logger:       testLogger(t),
			},
		}

		action := &Action{
			Type:    ActionDownload,
			Path:    "test.txt",
			DriveID: driveid.New("d"),
			ItemID:  "item3",
		}

		_, blocked := exec.checkDiskSpace(action)
		assert.False(t, blocked, "minFreeSpace=0 should not block any download")
	})

	// Validates: R-6.2.6
	t.Run("StatfsError_FailOpen", func(t *testing.T) {
		// When statfs fails, the check should fail open (not block downloads).
		orig := diskAvailableFunc
		diskAvailableFunc = func(string) (uint64, error) {
			return 0, fmt.Errorf("simulated statfs error")
		}
		t.Cleanup(func() { diskAvailableFunc = orig })

		exec := &Executor{
			ExecutorConfig: &ExecutorConfig{
				minFreeSpace: 1000,
				syncRoot:     t.TempDir(),
				driveID:      driveid.New("d"),
				logger:       testLogger(t),
			},
		}

		action := &Action{
			Type:    ActionDownload,
			Path:    "test.txt",
			DriveID: driveid.New("d"),
			ItemID:  "item4",
		}

		_, blocked := exec.checkDiskSpace(action)
		assert.False(t, blocked, "statfs error should fail open, not block downloads")
	})

	// Validates: R-6.2.6
	t.Run("SufficientSpace", func(t *testing.T) {
		// available (5000) >= fileSize (2000) + minFreeSpace (1000) — no block.
		orig := diskAvailableFunc
		diskAvailableFunc = func(string) (uint64, error) { return 5000, nil }
		t.Cleanup(func() { diskAvailableFunc = orig })

		exec := &Executor{
			ExecutorConfig: &ExecutorConfig{
				minFreeSpace: 1000,
				syncRoot:     t.TempDir(),
				driveID:      driveid.New("d"),
				logger:       testLogger(t),
			},
		}

		action := &Action{
			Type:    ActionDownload,
			Path:    "test.txt",
			DriveID: driveid.New("d"),
			ItemID:  "item5",
			View: &PathView{
				Remote: &RemoteState{Size: 2000},
			},
		}

		_, blocked := exec.checkDiskSpace(action)
		assert.False(t, blocked, "should not block when sufficient space available")
	})
}
