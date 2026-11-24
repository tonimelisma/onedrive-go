package sync

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// A local scan that reaches the watch after its stop is still committed, under
// the stop's one deadline rather than a bound of its own. When the sync
// database does not answer within that deadline, the lost scan is said once
// and the watch ends as the stop asked, without a new error: the next sync
// scans the tree again.
//
// Validates: R-6.3.9, R-2.8.3
func TestWatchRuntime_AScanCommitTheStopsDeadlineCutsIsSaidNotAnError(t *testing.T) {
	t.Parallel()

	logger, logs := syncfixture.NewCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &syncfixture.EngineMockClient{}, logger)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.hooks.stopBound = workerTestCommitBound
	stopped := stoppedContext(t)
	scan := &syncobserve.LocalObservation{
		Kind: syncobserve.LocalSnapshot,
		Rows: []syncdomain.LocalStateRow{{Path: "notes.txt", ItemType: syncdomain.ItemTypeFile, Hash: "h"}},
	}

	release := holdStoreConnection(t, eng.baseline)
	dirty, err := rt.applyLocalSnapshot(stopped, scan)
	release()

	require.NoError(t, err, "the stop ends the watch, not the lost scan")
	assert.False(t, dirty)
	assert.Equal(t, 1, strings.Count(logs.String(), "before the last local scan was recorded"),
		"the lost scan is said once: %s", logs.String())
	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows, "nothing half-written")
}

// A scan that reaches the watch after its stop, with a store that answers, is
// committed as before.
//
// Validates: R-6.3.9, R-2.8.3
func TestWatchRuntime_AScanAfterTheStopIsStillCommitted(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineWithLogger(t, &syncfixture.EngineMockClient{}, slog.New(slog.DiscardHandler))
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	scan := &syncobserve.LocalObservation{
		Kind: syncobserve.LocalSnapshot,
		Rows: []syncdomain.LocalStateRow{{Path: "notes.txt", ItemType: syncdomain.ItemTypeFile, Hash: "h"}},
	}

	_, err := rt.applyLocalSnapshot(stoppedContext(t), scan)
	require.NoError(t, err)

	rows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "notes.txt", rows[0].Path)
}
