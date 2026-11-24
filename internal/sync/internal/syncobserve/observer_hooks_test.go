package syncobserve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// A local observer with zero hooks behaves as production does: it watches
// through fsnotify, coalesces writes over the default cooldown and hashes
// with QuickXorHash.
//
// Validates: R-6.10.29
func TestHooks_ZeroValueIsProduction(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(&LocalObserverInputs{
		Baseline: syncfixture.EmptyBaseline(),
		Logger:   synctest.TestLogger(t),
		Hooks:    Hooks{},
	})

	watcher, err := obs.newWatcher()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, watcher.Close()) })
	assert.IsType(t, &fsnotifyWrapper{}, watcher, "the watcher is fsnotify's")
	assert.Equal(t, defaultWriteCoalesceCooldown, obs.writeCooldown(), "writes coalesce over the default cooldown")

	path := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("hooked or not\n"), 0o600))
	want, err := driveops.ComputeQuickXorHashContext(t.Context(), path)
	require.NoError(t, err)
	got, err := computeStableHashWith(t.Context(), path, obs.hooks.HashFile)
	require.NoError(t, err)
	assert.Equal(t, want, got, "a file hashes with QuickXorHash")
}
