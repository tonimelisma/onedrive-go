package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

type fakeDirtyDebounceTimer struct {
	mu         sync.Mutex
	ch         chan time.Time
	resetCh    chan struct{}
	active     bool
	resetCount int
}

func newFakeDirtyDebounceTimer() *fakeDirtyDebounceTimer {
	return &fakeDirtyDebounceTimer{
		ch:      make(chan time.Time, 1),
		resetCh: make(chan struct{}, 8),
	}
}

const dirtyBufferTestTimeout = time.Second

func (t *fakeDirtyDebounceTimer) Chan() <-chan time.Time { return t.ch }
func (t *fakeDirtyDebounceTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *fakeDirtyDebounceTimer) Reset(time.Duration) bool {
	t.mu.Lock()
	wasActive := t.active
	t.active = true
	t.resetCount++
	t.mu.Unlock()
	t.resetCh <- struct{}{}
	return wasActive
}

func (t *fakeDirtyDebounceTimer) Fire() {
	t.mu.Lock()
	if !t.active {
		t.mu.Unlock()
		return
	}
	t.active = false
	t.mu.Unlock()
	t.ch <- time.Now()
}

func (t *fakeDirtyDebounceTimer) resetCountSnapshot() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.resetCount
}

// Validates: R-2.1.2
func TestDirtyBuffer_FlushImmediate_CoalescesDirtySignalAndPreservesFullRefresh(t *testing.T) {
	t.Parallel()

	buf := newDirtyBuffer(synctest.TestLogger(t))
	buf.MarkDirty()
	buf.MarkDirty()
	buf.MarkDirty()
	buf.MarkFullRefresh()

	batch := buf.FlushImmediate()
	require.NotNil(t, batch)
	assert.True(t, batch.FullRefresh)
	assert.Nil(t, buf.FlushImmediate())
}

// Validates: R-2.1.2
func TestDirtyBuffer_FlushDebounced_UsesLastObservationWindow(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := newDirtyBuffer(synctest.TestLogger(t))
	timer := newFakeDirtyDebounceTimer()
	buf.newTimer = func(time.Duration) debounceTimer { return timer }
	out := buf.FlushDebounced(ctx, time.Second)

	buf.MarkDirty()
	buf.MarkDirty()
	select {
	case <-timer.resetCh:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for debounce reset")
	}
	resetCount := timer.resetCountSnapshot()
	assert.GreaterOrEqual(t, resetCount, 1)

	batch := fireDirtyDebounceUntilBatch(t, timer, out)
	assert.False(t, batch.FullRefresh)
}

// Validates: R-2.1.2
func TestDirtyBuffer_FlushDebounced_CoalescesMultipleDirtySignalsIntoOneBatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := newDirtyBuffer(synctest.TestLogger(t))
	timer := newFakeDirtyDebounceTimer()
	buf.newTimer = func(time.Duration) debounceTimer { return timer }
	out := buf.FlushDebounced(ctx, time.Second)

	buf.MarkDirty()
	buf.MarkDirty()
	buf.MarkFullRefresh()

	select {
	case <-timer.resetCh:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for debounce reset")
	}

	resetCount := timer.resetCountSnapshot()
	assert.GreaterOrEqual(t, resetCount, 1)

	batch := fireDirtyDebounceUntilBatch(t, timer, out)
	assert.True(t, batch.FullRefresh)

	select {
	case extra := <-out:
		require.Failf(t, "expected one coalesced dirty batch", "unexpected extra batch: %#v", extra)
	default:
	}
}

func fireDirtyDebounceUntilBatch(t *testing.T, timer *fakeDirtyDebounceTimer, out <-chan dirtyBatch) dirtyBatch {
	t.Helper()

	timer.Fire()
	timeout := time.After(dirtyBufferTestTimeout)
	for {
		select {
		case batch, ok := <-out:
			require.True(t, ok, "dirty batch channel closed before producing a batch")
			return batch
		case <-timer.resetCh:
			timer.Fire()
		case <-timeout:
			require.FailNow(t, "timed out waiting for debounced dirty batch")
		}
	}
}

// Validates: R-2.8.20
func TestDirtyBuffer_FlushDebounced_ContinuousSignalsFlushAtMaxWait(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	buf := newDirtyBuffer(synctest.TestLogger(t))
	quiet := newFakeDirtyDebounceTimer()
	deadline := newFakeDirtyDebounceTimer()
	var deadlineDelay time.Duration
	buf.newTimer = func(time.Duration) debounceTimer { return quiet }
	buf.newDeadlineTimer = func(delay time.Duration) debounceTimer {
		deadlineDelay = delay
		return deadline
	}
	out := buf.FlushDebounced(ctx, time.Second)

	waitReset := func(timer *fakeDirtyDebounceTimer, what string) {
		t.Helper()
		select {
		case <-timer.resetCh:
		case <-time.After(dirtyBufferTestTimeout):
			require.Fail(t, "timed out waiting for "+what)
		}
	}

	// Observations keep arriving: each restarts the quiet period, which
	// therefore never elapses. Only the first arms the deadline.
	buf.MarkDirty()
	waitReset(quiet, "quiet-period reset")
	waitReset(deadline, "deadline armed")
	buf.MarkDirty()
	waitReset(quiet, "second quiet-period reset")
	buf.MarkFullRefresh()
	waitReset(quiet, "third quiet-period reset")

	assert.Equal(t, 1, deadline.resetCountSnapshot(), "later observations do not push the deadline back")
	assert.Equal(t, dirtyMaxWait(time.Second), deadlineDelay)

	// Before the fix the batch waited for a quiet period that sustained
	// churn never allows.
	deadline.Fire()
	select {
	case batch := <-out:
		assert.True(t, batch.FullRefresh)
	case <-time.After(dirtyBufferTestTimeout):
		require.Fail(t, "continuous observations must still flush at the max wait")
	}
}

// Validates: R-2.8.20
func TestDirtyMaxWait_ScalesWithLongDebounce(t *testing.T) {
	t.Parallel()

	assert.Equal(t, minDirtyMaxWait, dirtyMaxWait(5*time.Second))
	assert.Equal(t, 6*time.Minute, dirtyMaxWait(time.Minute))
}
