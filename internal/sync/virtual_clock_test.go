package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

func inVirtualTime(t *testing.T, fn func(t *testing.T)) {
	t.Helper()

	gosynctest.Test(t, fn)
}

// virtualWait lets d of virtual time pass. Only meaningful inside
// inVirtualTime, where it returns as soon as everything else is idle.
func virtualWait(d time.Duration) {
	<-time.After(d)
}

// Validates: R-2.1.2
func TestDirtyBuffer_VirtualTime_FlushesOneQuietPeriodAfterTheLastSignal(t *testing.T) {
	t.Parallel()

	inVirtualTime(t, func(t *testing.T) { //nolint:thelper // a test body run in a synctest bubble, like a t.Run subtest
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		buf := newDirtyBuffer(synctest.TestLogger(t))
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
		buf := newDirtyBuffer(synctest.TestLogger(t))
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
			AfterFunc:             realAfterFunc,
			WriteCoalesceCooldown: cooldown,
		})
		events := make(chan changeEvent, 10)
		cancel, done := startMockWatch(t, obs, mockWatcher, dir, events)

		require.NoError(t, os.WriteFile(filePath, []byte("v2"), 0o600))
		start := time.Now()
		mockWatcher.events <- fsnotify.Event{Name: filePath, Op: fsnotify.Write}
		virtualWait(20 * time.Millisecond)
		mockWatcher.events <- fsnotify.Event{Name: filePath, Op: fsnotify.Write}

		ev := <-events
		assert.Equal(t, 20*time.Millisecond+cooldown, time.Since(start), "hashed one cooldown after the last write")
		assert.Equal(t, ChangeModify, ev.Type)
		assert.Equal(t, "rapid.txt", ev.Path)
		assert.Equal(t, hashContent(t, "v2"), ev.Hash)

		virtualWait(time.Hour)
		select {
		case extra := <-events:
			assert.Failf(t, "coalesced writes emitted twice", "%+v", extra)
		default:
		}

		cancel()
		require.NoError(t, <-done)
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
		events := make(chan changeEvent, 10)
		cancel, done := startMockWatch(t, obs, mockWatcher, dir, events)

		start := time.Now()
		writeTestFile(t, dir, "missed.txt", "no event for this one")

		ev := <-events
		assert.Equal(t, safetyScanInterval, time.Since(start), "found by the first safety scan")
		assert.Equal(t, "missed.txt", filepath.ToSlash(ev.Path))
		assert.Equal(t, changeCreate, ev.Type)

		cancel()
		require.NoError(t, <-done)
	})
}

// Validates: R-2.17.2, R-2.15.3
//
// A tool that deletes a synced folder online on every poll must not cause a
// full enumeration per poll. A folder deletion makes the full refresh due
// fullRefreshAfterFolderDeleteDelay later, not at once, so the deletions of
// one burst share one refresh and refreshes stay that far apart; the item
// moved into a deleted folder (model test seed 28) is still resolved, by the
// deferred refresh.
func TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes(t *testing.T) {
	skipInShort(t)
	t.Parallel()

	inVirtualTime(t, func(t *testing.T) { //nolint:thelper // a test body run in a synctest bubble, like a t.Run subtest
		remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
		const folders = 6
		for i := range folders {
			id := fmt.Sprintf("item-f%d", i)
			remote.addFolder(id, "root", fmt.Sprintf("f%d", i))
			remote.addFile(t, id+"-in", id, "in.txt", fmt.Sprintf("in %d\n", i))
		}
		remote.addFile(t, "item-moved", "root", "moved.txt", "moved\n")
		remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
		h := newSyncHarness(t, remote)
		runScenarioPass(t, h.eng)
		start := time.Now()

		remote.move("item-moved", "item-f0", "moved.txt")
		for minute := range 40 {
			if minute < folders {
				remote.deleteRemotely(fmt.Sprintf("item-f%d", minute))
			}
			_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{ConfirmDeletes: true})
			require.NoError(t, err)
			virtualWait(time.Minute)
		}

		var refreshes []time.Time
		for _, at := range remote.fullDeltaAt {
			if at.After(start) {
				refreshes = append(refreshes, at)
			}
		}
		require.NotEmpty(t, refreshes, "the deferred full refresh ran")
		for i := 1; i < len(refreshes); i++ {
			assert.GreaterOrEqual(t, refreshes[i].Sub(refreshes[i-1]), fullRefreshAfterFolderDeleteDelay,
				"full refreshes %d and %d are too close", i-1, i)
		}
		assert.NoFileExists(t, filepath.Join(h.syncRoot, "moved.txt"), "the item moved into a deleted folder is resolved")
		assertSyncInvariants(t, h)
	})
}
