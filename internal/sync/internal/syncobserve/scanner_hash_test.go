package syncobserve

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// countingGoClock is a manual clock that counts the goroutines started
// through it, so a test can see which work runs on the engine's clock.
type countingGoClock struct {
	*syncfixture.ManualClock
	goCalls atomic.Int64
}

func (c *countingGoClock) Go(fn func()) {
	c.goCalls.Add(1)
	c.ManualClock.Go(fn)
}

// newHashPoolObserver returns an observer over a fresh directory holding
// fileCount new files, hashing at most workers at a time on clock.
func newHashPoolObserver(t *testing.T, fileCount, workers int, clock *countingGoClock) (*LocalObserver, string) {
	t.Helper()

	dir := t.TempDir()
	for i := range fileCount {
		syncfixture.WriteTestFile(t, dir, fmt.Sprintf("file-%d.txt", i), fmt.Sprintf("content %d", i))
	}
	obs := NewLocalObserver(&LocalObserverInputs{
		Baseline:     syncfixture.BaselineWith(),
		Logger:       synctest.TestLogger(t),
		CheckWorkers: workers,
		Clock:        clock,
	})

	return obs, dir
}

// Validates: R-6.3.10
//
// The hash workers start through the observer's clock, one per file and no
// more than the worker limit at a time, so a replay that owns the clock owns
// every goroutine of the scan.
func TestFullScan_HashWorkersStartThroughTheObserverClock(t *testing.T) {
	t.Parallel()

	const fileCount, workers = 6, 2
	clock := &countingGoClock{ManualClock: syncfixture.NewManualClock(time.Now())}
	obs, dir := newHashPoolObserver(t, fileCount, workers, clock)

	var inFlight, maxInFlight atomic.Int64
	obs.hooks.HashFile = func(_ context.Context, path string) (string, error) {
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			seen := maxInFlight.Load()
			if current <= seen || maxInFlight.CompareAndSwap(seen, current) {
				break
			}
		}

		return "hash-of-" + path, nil
	}

	result, err := obs.FullScan(t.Context(), syncfixture.MustOpenSyncTree(t, dir))
	require.NoError(t, err)
	assert.Len(t, result.Rows, fileCount)
	assert.Len(t, result.Events, fileCount)
	assert.Equal(t, int64(fileCount), clock.goCalls.Load(), "every hash worker starts through the clock")
	assert.LessOrEqual(t, maxInFlight.Load(), int64(workers), "no more workers hash at once than the limit")
}

// Validates: R-6.3.10
//
// Canceling the scan while a worker hashes stops the phase: the scan fails
// with the context's error and no further worker starts.
func TestFullScan_CancelStopsTheHashWorkers(t *testing.T) {
	t.Parallel()

	const fileCount, workers = 5, 1
	clock := &countingGoClock{ManualClock: syncfixture.NewManualClock(time.Now())}
	obs, dir := newHashPoolObserver(t, fileCount, workers, clock)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hashing := make(chan struct{})
	var hashCalls atomic.Int64
	obs.hooks.HashFile = func(hashCtx context.Context, _ string) (string, error) {
		if hashCalls.Add(1) == 1 {
			close(hashing)
		}
		<-hashCtx.Done()

		return "", hashCtx.Err()
	}

	scanDone := make(chan error, 1)
	tree := syncfixture.MustOpenSyncTree(t, dir)
	go func() {
		_, scanErr := obs.FullScan(ctx, tree)
		scanDone <- scanErr
	}()

	<-hashing
	cancel()
	scanErr := <-scanDone

	require.ErrorIs(t, scanErr, context.Canceled)
	assert.Equal(t, int64(1), hashCalls.Load(), "no file is hashed after the cancel")
	assert.Equal(t, int64(1), clock.goCalls.Load(), "no worker starts after the cancel")
}
