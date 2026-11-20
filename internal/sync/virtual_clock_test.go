package sync

import (
	"context"
	"os"
	"testing"
	gosynctest "testing/synctest"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Virtual-clock harness (D-095). inVirtualTime runs a test in a
// testing/synctest bubble: time.Now, timers, tickers and sleeps inside it run
// on a fake clock that jumps forward only when every goroutine in the bubble
// is blocked. A debounce, backoff or safety-scan test can then use the real
// timers and state exact durations -- "flushed 8 s after the first signal",
// "found at exactly the five-minute scan" -- with no real waiting and no
// manual clock to advance.
//
// A goroutine blocked in a syscall is not durably blocked, so the bubble
// cannot hold a real inotify watcher; the tests below drive the observer
// through mockFsWatcher, whose channels belong to the bubble.

// Validates: R-2.1.2
func TestDirtyBuffer_VirtualTime_FlushesOneQuietPeriodAfterTheLastSignal(t *testing.T) {
	t.Parallel()

	inVirtualTime(t, func(t *testing.T) { //nolint:thelper // a test body run in a synctest bubble, like a t.Run subtest
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		buf := NewDirtyBuffer(synctest.TestLogger(t))
		out := buf.FlushDebounced(ctx, 5*time.Second)
		start := time.Now()

		buf.MarkDirty()
		virtualWait(3 * time.Second)
		buf.MarkDirty()

		batch := <-out
		assert.Equal(t, 8*time.Second, time.Since(start), "one quiet period after the last signal")
		assert.False(t, batch.FullRefresh)

		gosynctest.Wait()
		select {
		case extra := <-out:
			assert.Failf(t, "coalesced signals flushed twice", "%+v", extra)
		default:
		}
	})
}

// Validates: R-2.8.20
func TestDirtyBuffer_VirtualTime_ContinuousSignalsFlushAtExactlyTheMaxWait(t *testing.T) {
	t.Parallel()

	inVirtualTime(t, func(t *testing.T) { //nolint:thelper // a test body run in a synctest bubble, like a t.Run subtest
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		const debounce = 5 * time.Second
		buf := NewDirtyBuffer(synctest.TestLogger(t))
		out := buf.FlushDebounced(ctx, debounce)
		start := time.Now()

		// A signal every 4 s never leaves the 5 s quiet period.
		stop := make(chan struct{})
		go func() {
			for {
				buf.MarkDirty()
				select {
				case <-stop:
					return
				case <-time.After(4 * time.Second):
				}
			}
		}()

		<-out
		close(stop)
		assert.Equal(t, dirtyMaxWait(debounce), time.Since(start), "sustained churn flushes at the max wait")
	})
}

// Validates: R-2.8.16
//
// Two writes 20 ms apart coalesce into one hash, taken one cooldown after
// the second write, and nothing more is emitted however long the file stays
// quiet. On a manual clock this needed polling for the pending timer and a
// real 300 ms wait to show no second event.
func TestHandleWrite_VirtualTime_RapidWritesCoalesceIntoOneHash(t *testing.T) {
	t.Parallel()

	inVirtualTime(t, func(t *testing.T) { //nolint:thelper // a test body run in a synctest bubble, like a t.Run subtest
		dir := t.TempDir()
		filePath := writeTestFile(t, dir, "rapid.txt", "v1")
		baseline := baselineWith(&BaselineEntry{
			Path: "rapid.txt", DriveID: driveid.New("d"), ItemID: "i1",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "v1"),
		})

		const cooldown = 100 * time.Millisecond
		mockWatcher := newMockFsWatcher()
		obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
			Baseline:              baseline,
			AfterFunc:             RealClock{}.AfterFunc,
			WriteCoalesceCooldown: cooldown,
		})
		batches := localBatches(obs, 10)
		cancel, done := startMockWatch(t, obs, mockWatcher, dir)

		require.NoError(t, os.WriteFile(filePath, []byte("v2"), 0o600))
		start := time.Now()
		mockWatcher.events <- fsnotify.Event{Name: filePath, Op: fsnotify.Write}
		virtualWait(20 * time.Millisecond)
		mockWatcher.events <- fsnotify.Event{Name: filePath, Op: fsnotify.Write}

		patch := <-batches
		assert.Equal(t, 20*time.Millisecond+cooldown, time.Since(start), "hashed one cooldown after the last write")
		row, ok := patchRow(&patch, "rapid.txt")
		require.True(t, ok, "the write's patch carries the file's row: %+v", patch)
		assert.Equal(t, hashContent(t, "v2"), row.Hash)

		virtualWait(time.Hour)
		requireNoLocalPatch(t, batches, "an hour after the coalesced writes")

		stopWatch(t, cancel, done)
	})
}

// Validates: R-2.1.2
//
// A file that appeared without any watch event is found by the safety scan,
// at exactly its interval and not before.
func TestWatch_VirtualTime_SafetyScanFindsAMissedFileAtItsInterval(t *testing.T) {
	t.Parallel()

	inVirtualTime(t, func(t *testing.T) { //nolint:thelper // a test body run in a synctest bubble, like a t.Run subtest
		dir := t.TempDir()
		mockWatcher := newMockFsWatcher()
		obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{})
		obs.clock = newObserverTestClock() // real ticker and sleeps, on the bubble's clock
		batches := localBatches(obs, 10)
		cancel, done := startMockWatch(t, obs, mockWatcher, dir)

		start := time.Now()
		writeTestFile(t, dir, "missed.txt", "no event for this one")

		snapshot := <-batches
		assert.Equal(t, safetyScanInterval, time.Since(start), "found by the first safety scan")
		require.Equal(t, LocalSnapshot, snapshot.Kind)
		require.NotNil(t, findRow(snapshot.Rows, "missed.txt"), "the snapshot holds the missed file: %+v", snapshot.Rows)

		stopWatch(t, cancel, done)
	})
}
