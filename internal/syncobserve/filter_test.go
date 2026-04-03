package syncobserve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.4.1, R-2.4.2, R-2.4.3
func TestFullScan_ConfiguredFiltersExcludeConfiguredEntries(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	writeTestFile(t, syncRoot, ".hidden/secrets.txt", "hidden")
	writeTestFile(t, syncRoot, "vendor/lib.txt", "vendored")
	writeTestFile(t, syncRoot, "debug.log", "noise")
	writeTestFile(t, syncRoot, "keep.txt", "keep")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetFilterConfig(synctypes.LocalFilterConfig{
		SkipDotfiles: true,
		SkipDirs:     []string{"vendor"},
		SkipFiles:    []string{"*.log"},
	})

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	require.Len(t, result.Events, 1, "only keep.txt should survive configured filters")
	assert.Equal(t, "keep.txt", result.Events[0].Path)
	assert.Empty(t, result.Skipped, "configured filters are silent exclusions, not actionable issues")
}

// Validates: R-2.4.1, R-2.4.2, R-2.4.3
func TestObserveSinglePathWithFilter_ConfiguredFiltersResolveSilently(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "vendor"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "debug.log"), []byte("noise"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, ".env"), []byte("secret"), 0o600))

	filter := synctypes.LocalFilterConfig{
		SkipDotfiles: true,
		SkipDirs:     []string{"vendor"},
		SkipFiles:    []string{"*.log"},
	}

	testCases := []string{".env", "vendor", "debug.log"}
	for _, relPath := range testCases {
		t.Run(relPath, func(t *testing.T) {
			result, err := ObserveSinglePathWithFilter(
				nil,
				mustOpenSyncTree(t, syncRoot),
				relPath,
				nil,
				time.Now().UnixNano(),
				nil,
				filter,
			)
			require.NoError(t, err)
			assert.Nil(t, result.Event)
			assert.Nil(t, result.Skipped)
			assert.True(t, result.Resolved)
		})
	}
}

// Validates: R-2.4.1, R-2.4.2
func TestAddWatchesRecursive_ConfiguredDirectoryFiltersSkipWatchedSubtrees(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, ".git"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "vendor"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "docs"), 0o700))

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetFilterConfig(synctypes.LocalFilterConfig{
		SkipDotfiles: true,
		SkipDirs:     []string{"vendor"},
	})

	tracker := &addTrackingWatcher{
		events:     make(chan fsnotify.Event, 10),
		errs:       make(chan error, 10),
		addedPaths: make(map[string]bool),
	}

	err := obs.AddWatchesRecursive(context.Background(), tracker, mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	assert.True(t, tracker.addedPaths[syncRoot], "sync root itself should still be watched")
	assert.True(t, tracker.addedPaths[filepath.Join(syncRoot, "docs")], "non-filtered directories should be watched")
	assert.False(t, tracker.addedPaths[filepath.Join(syncRoot, ".git")], "dotfile directories should be skipped by watch setup")
	assert.False(t, tracker.addedPaths[filepath.Join(syncRoot, "vendor")], "configured skipped directories should be skipped by watch setup")
}

// Validates: R-2.4.3
func TestHandleFsEvent_ConfiguredFileFilterSkipsWrite(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	logFile := writeTestFile(t, syncRoot, "debug.log", "noise")

	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.SetFilterConfig(synctypes.LocalFilterConfig{
		SkipFiles: []string{"*.log"},
	})

	watcher := newMockFsWatcher()
	events := make(chan synctypes.ChangeEvent, 2)

	obs.HandleFsEvent(
		t.Context(),
		fsnotify.Event{Name: logFile, Op: fsnotify.Write},
		watcher,
		mustOpenSyncTree(t, syncRoot),
		events,
	)

	select {
	case ev := <-events:
		require.Failf(t, "unexpected event for configured skipped file", "%+v", ev)
	default:
	}
}
