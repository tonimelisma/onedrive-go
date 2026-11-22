package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// A watch's bootstrap cleans up after its first sync before it arms the
// first full remote refresh: the leftover download file the cleanup removes
// is already gone when the refresh timer is armed, so the refresh never
// walks a tree housekeeping has not cleaned.
//
// Validates: R-2.4.10
func TestRunWatch_HousekeepingRunsBeforeTheFirstRefreshTimerIsArmed(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	partial := filepath.Join(syncRoot, testOwnedPartialName("left.txt"))
	require.NoError(t, os.WriteFile(partial, []byte("left"), 0o600))

	refreshDelay, err := observationOf(eng.Engine).fullRemoteRefreshDelay(t.Context())
	require.NoError(t, err)
	var armedWithPartial []bool
	rt.deps.clock = &syncfixture.ObservingClock{
		SyncClock: rt.deps.clock,
		OnAfterFunc: func(delay time.Duration) {
			if delay == refreshDelay {
				_, statErr := os.Stat(partial)
				armedWithPartial = append(armedWithPartial, statErr == nil)
			}
		},
	}

	require.NoError(t, rt.finishBootstrapAfterActions(t.Context()))
	t.Cleanup(func() { rt.resetRefreshTimer(nil) })

	require.Len(t, armedWithPartial, 1, "the bootstrap arms one full remote refresh")
	assert.False(t, armedWithPartial[0], "housekeeping removed the leftover before the refresh was armed")
}
