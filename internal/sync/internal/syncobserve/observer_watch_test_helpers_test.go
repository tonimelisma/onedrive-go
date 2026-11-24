package syncobserve

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

type watchObserverTestOptions struct {
	Baseline              syncdomain.BaselineView
	WriteCoalesceCooldown time.Duration
	HashFunc              func(ctx context.Context, path string) (string, error)
	AfterFunc             func(time.Duration, func()) syncdomain.SyncTimer
	AfterSafetyScan       func()
	TickCh                <-chan time.Time
	// DebugEvents is the observer's debug event sink, the one the watch
	// runtime installs (a debugEventRecorder's record in tests).
	DebugEvents func(syncdomain.EngineDebugEvent)
}

func newWatchTestObserver(t *testing.T, watcher FsWatcher, opts watchObserverTestOptions) *LocalObserver {
	t.Helper()

	baseline := opts.Baseline
	if baseline == nil {
		baseline = syncfixture.EmptyBaseline()
	}

	obs := NewLocalObserver(&LocalObserverInputs{Baseline: baseline, Logger: synctest.TestLogger(t)})

	// One clock value, not a set of independently overridable function fields:
	// a never-firing tick and an instant sleep by default, with the real clock
	// behind anything the test does not pin.
	clock := newObserverTestClock()
	clock.sleep = func(context.Context, time.Duration) error { return nil }
	clock.tickCh = make(chan time.Time)
	obs.clock = clock

	obs.hooks.WatcherFactory = func() (FsWatcher, error) {
		return watcher, nil
	}

	if opts.WriteCoalesceCooldown != 0 {
		obs.hooks.WriteCoalesceCooldown = opts.WriteCoalesceCooldown
	}
	if opts.HashFunc != nil {
		obs.hooks.HashFile = opts.HashFunc
	}
	if opts.AfterFunc != nil {
		clock.afterFunc = opts.AfterFunc
	}
	if opts.AfterSafetyScan != nil {
		obs.hooks.AfterSafetyScan = opts.AfterSafetyScan
	}
	if opts.TickCh != nil {
		clock.tickCh = opts.TickCh
	}
	obs.debugEvents = opts.DebugEvents

	return obs
}

// requireNoLocalPatch fails when batches holds a local patch. It never
// waits: the caller has a signal that whatever could have been sent was sent
// before it. Snapshots and suspect marks are not patches and are skipped.
func requireNoLocalPatch(t *testing.T, batches <-chan LocalObservation, signal string) {
	t.Helper()

	for {
		select {
		case batch := <-batches:
			if batch.Kind == LocalPatch {
				require.FailNow(t, "unexpected local patch", "got %+v before %s", batch, signal)
			}
		default:
			return
		}
	}
}

// localBatches gives obs a buffered local observation channel, the one
// output the watch runtime gives it at construction, and returns it. A test
// of this package may install it after construction, before Watch starts.
func localBatches(obs *LocalObserver, size int) chan LocalObservation {
	batches := make(chan LocalObservation, size)
	obs.localBatchCh = batches

	return batches
}

// patchRow returns the row a local patch writes for path.
func patchRow(batch *LocalObservation, path string) (syncdomain.LocalStateRow, bool) {
	if batch.Kind != LocalPatch {
		return syncdomain.LocalStateRow{}, false
	}
	for i := range batch.Rows {
		if batch.Rows[i].Path == path {
			return batch.Rows[i], true
		}
	}

	return syncdomain.LocalStateRow{}, false
}

// patchRemoves reports whether a local patch removes path and its subtree.
func patchRemoves(batch *LocalObservation, path string) bool {
	return batch.Kind == LocalPatch && slices.Contains(batch.DeletedPrefixes, path)
}

// waitForLocalPatch returns the first local patch on batches that match
// accepts, skipping every other observation.
func waitForLocalPatch(
	t *testing.T, batches <-chan LocalObservation, match func(*LocalObservation) bool, what string,
) LocalObservation {
	t.Helper()

	timeout := time.After(syncfixture.EventWait)
	for {
		select {
		case batch := <-batches:
			if batch.Kind == LocalPatch && match(&batch) {
				return batch
			}
		case <-timeout:
			require.FailNow(t, "timeout waiting for "+what)
			return LocalObservation{}
		}
	}
}

// waitForLocalSnapshot returns the next full scan snapshot on batches,
// skipping every other observation.
func waitForLocalSnapshot(t *testing.T, batches <-chan LocalObservation, what string) LocalObservation {
	t.Helper()

	timeout := time.After(syncfixture.EventWait)
	for {
		select {
		case batch := <-batches:
			if batch.Kind == LocalSnapshot {
				return batch
			}
		case <-timeout:
			require.FailNow(t, "timeout waiting for "+what)
			return LocalObservation{}
		}
	}
}

// waitForLocalRow waits for a local patch that writes a row for path and
// returns that row.
func waitForLocalRow(t *testing.T, batches <-chan LocalObservation, path string) syncdomain.LocalStateRow {
	t.Helper()

	var row syncdomain.LocalStateRow
	waitForLocalPatch(t, batches, func(batch *LocalObservation) bool {
		var ok bool
		row, ok = patchRow(batch, path)
		return ok
	}, "a row for "+path)

	return row
}

// waitForLocalRemoval waits for a local patch that removes path.
func waitForLocalRemoval(t *testing.T, batches <-chan LocalObservation, path string) {
	t.Helper()

	waitForLocalPatch(t, batches, func(batch *LocalObservation) bool {
		return patchRemoves(batch, path)
	}, "the removal of "+path)
}

// drainLocalPatches returns every local patch batches holds now, without
// waiting.
func drainLocalPatches(batches <-chan LocalObservation) []LocalObservation {
	var drained []LocalObservation
	for {
		select {
		case batch := <-batches:
			if batch.Kind == LocalPatch {
				drained = append(drained, batch)
			}
		default:
			return drained
		}
	}
}

// drainPatchRows returns, in order, the rows of every local patch batches
// holds now, without waiting.
func drainPatchRows(batches <-chan LocalObservation) []syncdomain.LocalStateRow {
	var rows []syncdomain.LocalStateRow
	patches := drainLocalPatches(batches)
	for i := range patches {
		rows = append(rows, patches[i].Rows...)
	}

	return rows
}

// patchRowPaths lists the row paths of patches, in order.
func patchRowPaths(patches []LocalObservation) []string {
	var paths []string
	for i := range patches {
		for j := range patches[i].Rows {
			paths = append(paths, patches[i].Rows[j].Path)
		}
	}

	return paths
}

// startMockWatch runs obs.Watch on dir in the background (runInBackground)
// and returns once the mock watcher has its first directory.
func startMockWatch(
	t *testing.T,
	obs *LocalObserver,
	watcher *syncfixture.MockFsWatcher,
	dir string,
) (context.CancelFunc, <-chan error) {
	t.Helper()

	tree := syncfixture.MustOpenSyncTree(t, dir)
	cancel, done := syncfixture.RunInBackground(t, t.Context(), func(ctx context.Context) error {
		return obs.Watch(ctx, tree)
	})

	select {
	case err := <-done:
		require.FailNow(t, "watch exited before becoming ready", "returned %v", err)
	case <-watcher.Added():
	case <-time.After(syncfixture.EventWait):
		require.FailNow(t, "timed out waiting for mock watch setup")
	}

	return cancel, done
}
